package main

import (
	"strings"
)

// Job liveness probe used by `srv ui` to filter "still running" from
// "finished" without keeping each entry around forever. The signal is
// the same one `wait_job` uses to decide completion: the spawn
// wrapper writes ~/.srv-jobs/<id>.exit when the user command
// returns. An .exit marker means "done"; absent means "still alive"
// (or got SIGKILL'd externally before it could write).
//
// We list markers per profile in a single SSH call, so cost scales
// with the number of distinct profiles in the jobs table, not with
// the number of jobs. With the daemon connection pool warm, one
// profile's call is sub-100ms.

// checkJobLiveness returns a map jobID -> alive built from one SSH
// probe per profile. Jobs in profiles we couldn't reach (or whose
// profile no longer exists in config) are omitted from the result;
// callers should treat "not in map" as alive (= don't hide).

// remoteExitMarkers asks the profile for the set of `.exit` files
// under ~/.srv-jobs/. Returns a map jobID -> true for each present
// marker, or nil on SSH failure. We use `ls` rather than a `find`
// or `stat` loop because it's a single round-trip and the directory
// is small (a handful of markers max in real usage).
func remoteExitMarkers(prof *Profile) map[string]bool {
	res, err := runRemoteCapture(prof, "", "ls -1 ~/.srv-jobs/ 2>/dev/null")
	if err != nil || res == nil {
		return nil
	}
	if res.ExitCode != 0 && res.Stdout == "" {
		// Empty dir / no dir -- not an error, just no markers.
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
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
