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
	"sync"

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

// CheckLiveness returns a map jobID -> alive built from one probe
// per profile. Probes run concurrently across profiles -- a single
// slow / unreachable host no longer blocks the rest of the table.
//
// The lister must therefore be safe to call from multiple
// goroutines simultaneously (it typically just opens an SSH conn
// to the named profile, so this is automatic).
//
// Jobs in profiles the probe couldn't reach are omitted from the
// result; callers should treat "not in map" as alive (= don't hide
// the row in `srv ui`).
func CheckLiveness(rs []*Record, list ExitMarkerLister) map[string]bool {
	out := map[string]bool{}
	if len(rs) == 0 {
		return out
	}
	byProfile := map[string][]*Record{}
	for _, j := range rs {
		byProfile[j.Profile] = append(byProfile[j.Profile], j)
	}

	type probeResult struct {
		prof    string
		markers map[string]bool
		ok      bool
	}
	results := make(chan probeResult, len(byProfile))
	var wg sync.WaitGroup
	for prof, profJobs := range byProfile {
		_ = profJobs // captured per-iteration below
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			markers, ok := list(p)
			results <- probeResult{prof: p, markers: markers, ok: ok}
		}(prof)
	}
	wg.Wait()
	close(results)

	for r := range results {
		if !r.ok {
			continue
		}
		for _, j := range byProfile[r.prof] {
			out[j.ID] = !r.markers[j.ID]
		}
	}
	return out
}

// RemoteExitMarkersFn is the dependency-injection seam to read the
// remote's `.exit` marker filenames -- the caller provides a runner
// that does `runRemoteCapture(prof, ...)` so internal/jobs doesn't
// have to import remote / sshx.
type RemoteCaptureFn func(cmd string) (stdout string, exitCode int, ok bool)

// RemoteExitMarkers asks the profile (via captureFn) for the set of
// `.exit` files under ~/.srv-jobs/. Returns a map jobID -> true for
// each present marker, or nil on SSH failure. We use `ls` rather
// than `find` / `stat` because it's a single round-trip and the
// directory is small.
func RemoteExitMarkers(captureFn RemoteCaptureFn) map[string]bool {
	stdout, exitCode, ok := captureFn("ls -1 ~/.srv-jobs/ 2>/dev/null")
	if !ok {
		return nil
	}
	if exitCode != 0 && stdout == "" {
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ".exit") {
			continue
		}
		id := strings.TrimSuffix(line, ".exit")
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out
}
