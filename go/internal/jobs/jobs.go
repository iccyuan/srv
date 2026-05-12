// Package jobs owns the detached-job ledger at ~/.srv/jobs.json: the
// Record shape, the load / save dance, and the liveness probe that
// the UI uses to decide "still running" vs "finished".
//
// Spawning a job, killing one, and the cmd handlers themselves stay
// in package main -- they coordinate Profile / Config / SSH stuff
// that this package deliberately does not depend on. Strategy (b)
// dependency injection: the liveness probe takes an ExitMarkerLister
// callback so jobs need not pull in the SSH client or Config.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"srv/internal/srvio"
	"srv/internal/srvpath"
)

// Record is one detached-job entry. Mirrors what spawnDetached
// writes after a successful `nohup` invocation.
type Record struct {
	ID      string `json:"id"`
	Profile string `json:"profile"`
	Cmd     string `json:"cmd"`
	Cwd     string `json:"cwd"`
	Pid     int    `json:"pid"`
	Log     string `json:"log"`
	Started string `json:"started"`
}

// File is the on-disk wrapper: a version stamp plus the slice of
// records. Exposed (rather than hidden behind getters) because
// callers need to mutate the slice in place when adding / removing
// jobs and then Save it.
type File struct {
	Version int       `json:"_version,omitempty"`
	Jobs    []*Record `json:"jobs"`
}

// Load reads jobs.json off disk. Corrupted file -> warning to stderr
// + empty result, never a fatal: jobs.json is convenience state, not
// authoritative, and a crash here would block every CLI invocation.
func Load() *File {
	data, err := os.ReadFile(srvpath.Jobs())
	j := &File{}
	if err != nil {
		return j
	}
	if err := json.Unmarshal(data, j); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s is not valid JSON: %v\n", srvpath.Jobs(), err)
		return j
	}
	if j.Jobs == nil {
		j.Jobs = []*Record{}
	}
	srvio.WarnIfNewerSchema(srvpath.Jobs(), j.Version)
	return j
}

// Save persists the file, stamping the current schema version.
func Save(j *File) error {
	j.Version = srvio.SchemaVersion
	return srvio.WriteJSONFile(srvpath.Jobs(), j)
}

// Find resolves an id-or-prefix to its Record. Returns nil when no
// match; emits a one-line warning to stderr on ambiguous prefix
// matches and still returns nil so the caller treats "ambiguous" as
// "not found" and surfaces a hint.
func Find(j *File, idOrPrefix string) *Record {
	for _, job := range j.Jobs {
		if job.ID == idOrPrefix {
			return job
		}
	}
	matches := []*Record{}
	for _, job := range j.Jobs {
		if strings.HasPrefix(job.ID, idOrPrefix) {
			matches = append(matches, job)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 {
		fmt.Fprintf(os.Stderr, "ambiguous job id %q matches %d jobs.\n", idOrPrefix, len(matches))
		return nil
	}
	return nil
}

// ExitMarkerLister is the dependency-injection seam for CheckLiveness:
// given a profile name, return the set of remote `.exit` filenames
// (without the .exit suffix; jobID keys) plus a bool reporting whether
// the probe even succeeded. ok=false means "couldn't reach this
// profile" -- callers should treat its jobs as alive (= don't hide).
type ExitMarkerLister func(profileName string) (markers map[string]bool, ok bool)

// CheckLiveness returns a map jobID -> alive built from one probe per
// profile (the caller batches the SSH ls). Jobs in profiles the probe
// couldn't reach are omitted from the result; callers should treat
// "not in map" as alive.
func CheckLiveness(rs []*Record, list ExitMarkerLister) map[string]bool {
	out := map[string]bool{}
	if len(rs) == 0 {
		return out
	}
	byProfile := map[string][]*Record{}
	for _, j := range rs {
		byProfile[j.Profile] = append(byProfile[j.Profile], j)
	}
	for prof, profJobs := range byProfile {
		markers, ok := list(prof)
		if !ok {
			continue
		}
		for _, j := range profJobs {
			out[j.ID] = !markers[j.ID]
		}
	}
	return out
}
