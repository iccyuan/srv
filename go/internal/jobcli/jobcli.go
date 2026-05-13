// Package jobcli implements the four CLI commands that operate on
// detached jobs: `srv -d <cmd>` (detach), `srv jobs` (list), `srv
// logs <id> [-f]` (tail the log), `srv kill <id>` (signal).
//
// The on-disk registry and SpawnDetached live in internal/jobs and
// internal/remote respectively; this package is the thin CLI surface
// that glues them together. A separate package (rather than a file
// inside internal/jobs) avoids an import cycle: internal/remote
// already depends on internal/jobs for the Record type, so
// internal/jobs cannot itself import internal/remote.
package jobcli

import (
	"fmt"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/remote"
	"strings"
)

// CmdDetach implements `srv -d <cmd>` (and the explicit `srv detach`
// alias if the dispatcher exposes one).
func CmdDetach(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return clierr.Errf(1, "error: srv -d needs a command.")
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}
	userCmd := strings.Join(args, " ")
	rec, err := remote.SpawnDetached(name, profile, userCmd)
	if err != nil {
		return clierr.Errf(1, "error: %v", err)
	}
	fmt.Printf("job   %s  pid=%d  profile=%s\n", rec.ID, rec.Pid, rec.Profile)
	fmt.Printf("log   %s\n", rec.Log)
	fmt.Printf("tail  srv logs %s -f\n", rec.ID)
	fmt.Printf("kill  srv kill %s\n", rec.ID)
	return nil
}

// CmdJobs prints the active job table. profileOverride filters
// the list down to one profile when set.
func CmdJobs(cfg *config.Config, profileOverride string) error {
	rs := jobs.Load().Jobs
	if profileOverride != "" {
		filtered := rs[:0]
		for _, j := range rs {
			if j.Profile == profileOverride {
				filtered = append(filtered, j)
			}
		}
		rs = filtered
	}
	if len(rs) == 0 {
		fmt.Println("(no jobs)")
		return nil
	}
	sort.Slice(rs, func(i, k int) bool { return rs[i].ID < rs[k].ID })
	for _, j := range rs {
		cmd := j.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		fmt.Printf("%s  pid=%-7d profile=%-10s started=%s  cmd=%s\n",
			j.ID, j.Pid, j.Profile, j.Started, cmd)
	}
	return nil
}

// CmdLogs implements `srv logs <id> [-f]` -- streams or cats the
// job's remote log file. `-f` switches from `cat` to `tail -f`.
func CmdLogs(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 || args[0] == "-f" || args[0] == "--follow" {
		return clierr.Errf(1, `usage: srv logs <id> [-f]                    output of a detached job (~/.srv-jobs/<id>.log)
see also:
  srv tail [-n N] [--grep RE] <path>            any remote file (auto-reconnect)
  srv journal -u UNIT [-f]                      systemd journal for a service`)
	}
	jid := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	jf := jobs.Load()
	j := jobs.Find(jf, jid)
	if j == nil {
		return clierr.Errf(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return clierr.Errf(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := "cat " + j.Log
	if follow {
		cmd = "tail -f " + j.Log
	}
	return clierr.Code(remote.RunStream(prof, "", cmd, follow))
}

// CmdKill implements `srv kill <id> [--signal=NAME | -9]`. Always
// drops the local job record afterwards, even when the kill failed
// (the remote pid was already gone -- nothing to track anymore).
func CmdKill(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return clierr.Errf(1, "usage: srv kill <id>")
	}
	jid := args[0]
	sig := "TERM"
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--signal=") {
			sig = strings.TrimPrefix(a, "--signal=")
		} else if a == "-9" {
			sig = "KILL"
		}
	}
	jf := jobs.Load()
	j := jobs.Find(jf, jid)
	if j == nil {
		return clierr.Errf(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return clierr.Errf(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid (already exited?)'", sig, j.Pid)
	rc := remote.RunStream(prof, "", cmd, false)
	// Drop the job record regardless of the kill's exit code -- a
	// non-zero rc usually means "already gone", which is what kill
	// was supposed to achieve anyway. Keeping a phantom row would
	// just confuse `srv jobs`.
	out := jf.Jobs[:0]
	for _, x := range jf.Jobs {
		if x.ID != j.ID {
			out = append(out, x)
		}
	}
	jf.Jobs = out
	_ = jobs.Save(jf)
	return clierr.Code(rc)
}
