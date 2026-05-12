package main

import (
	"fmt"
	"sort"
	"strings"

	"srv/internal/jobs"
)

// spawnDetached moved to srv/internal/remote.SpawnDetached. Aliased
// in remote_alias.go.

func cmdDetach(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 {
		return exitErr(1, "error: srv -d needs a command.")
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	userCmd := strings.Join(args, " ")
	rec, err := spawnDetached(name, profile, userCmd)
	if err != nil {
		return exitErr(1, "error: %v", err)
	}
	fmt.Printf("job   %s  pid=%d  profile=%s\n", rec.ID, rec.Pid, rec.Profile)
	fmt.Printf("log   %s\n", rec.Log)
	fmt.Printf("tail  srv logs %s -f\n", rec.ID)
	fmt.Printf("kill  srv kill %s\n", rec.ID)
	return nil
}

func cmdJobs(cfg *Config, profileOverride string) error {
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

func cmdLogs(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 || args[0] == "-f" || args[0] == "--follow" {
		return exitErr(1, `usage: srv logs <id> [-f]                    output of a detached job (~/.srv-jobs/<id>.log)
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
		return exitErr(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return exitErr(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := "cat " + j.Log
	if follow {
		cmd = "tail -f " + j.Log
	}
	return exitCode(runRemoteStream(prof, "", cmd, follow))
}

func cmdKill(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 {
		return exitErr(1, "usage: srv kill <id>")
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
		return exitErr(1, "error: no such job %q", jid)
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return exitErr(1, "error: profile %q (from job) not found.", j.Profile)
	}
	cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid (already exited?)'", sig, j.Pid)
	rc := runRemoteStream(prof, "", cmd, false)
	// Drop the job record.
	out := jf.Jobs[:0]
	for _, x := range jf.Jobs {
		if x.ID != j.ID {
			out = append(out, x)
		}
	}
	jf.Jobs = out
	_ = jobs.Save(jf)
	return exitCode(rc)
}
