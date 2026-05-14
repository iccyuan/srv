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
	"os"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/jobnotify"
	"srv/internal/jobs"
	"srv/internal/remote"
	"srv/internal/runwrap"
	"strings"
	"time"
)

// RunOpts mirrors the subset of globalOpts that affects how the
// detached command is wrapped before nohup. Kept narrow so jobcli
// doesn't pull in package main's flag types.
type RunOpts struct {
	RestartOnFail int
	RestartDelay  time.Duration
	CPULimit      string
	MemLimit      string
}

// CmdDetach implements `srv -d <cmd>` (and the explicit `srv detach`
// alias if the dispatcher exposes one).
func CmdDetach(args []string, cfg *config.Config, profileOverride string, ro RunOpts) error {
	if len(args) == 0 {
		return clierr.Errf(1, "error: srv -d needs a command.")
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}
	userCmd := strings.Join(args, " ")
	// Apply wrappers (resource limits / restart-on-fail) before nohup
	// sees the command. The restart loop runs inside the nohup wrapper
	// so a crashed-and-respawned process inherits the same log file.
	wrapped := runwrap.Wrap(userCmd, runwrap.Opts{
		RestartOnFail: ro.RestartOnFail,
		RestartDelay:  ro.RestartDelay,
		CPULimit:      ro.CPULimit,
		MemLimit:      ro.MemLimit,
	})
	rec, err := remote.SpawnDetached(name, profile, wrapped)
	if err != nil {
		return clierr.Errf(1, "error: %v", err)
	}
	// Echo the user's original command in the job record so `srv jobs`
	// shows what they actually asked for, not the post-wrap shell soup.
	rec.Cmd = userCmd
	jf := jobs.Load()
	for i, j := range jf.Jobs {
		if j.ID == rec.ID {
			jf.Jobs[i].Cmd = userCmd
			_ = jobs.Save(jf)
			break
		}
	}
	fmt.Printf("job   %s  pid=%d  profile=%s\n", rec.ID, rec.Pid, rec.Profile)
	fmt.Printf("log   %s\n", rec.Log)
	fmt.Printf("tail  srv logs %s -f\n", rec.ID)
	fmt.Printf("kill  srv kill %s\n", rec.ID)
	if ro.RestartOnFail != 0 {
		bound := "unlimited retries"
		if ro.RestartOnFail > 0 {
			bound = fmt.Sprintf("max %d retries", ro.RestartOnFail)
		}
		fmt.Printf("supervisor: restart-on-fail (%s)\n", bound)
	}
	if ro.CPULimit != "" || ro.MemLimit != "" {
		fmt.Printf("limits: cpu=%s mem=%s (via systemd-run if available)\n",
			defaultIfEmpty(ro.CPULimit, "-"), defaultIfEmpty(ro.MemLimit, "-"))
	}
	return nil
}

func defaultIfEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// cmdJobsNotify manages JobNotify config:
//
//	srv jobs notify              show current state
//	srv jobs notify on           enable local OS toasts
//	srv jobs notify off          disable everything
//	srv jobs notify webhook URL  set/replace webhook url
//	srv jobs notify webhook -    clear webhook url
//	srv jobs notify test         fire a sample notification using current settings
func cmdJobsNotify(args []string, cfg *config.Config) error {
	if len(args) == 0 {
		printNotifyState(cfg)
		return nil
	}
	switch args[0] {
	case "on":
		if cfg.JobNotify == nil {
			cfg.JobNotify = &config.JobNotifyConfig{}
		}
		cfg.JobNotify.Local = true
		if err := config.Save(cfg); err != nil {
			return clierr.Errf(1, "error: %v", err)
		}
		printNotifyState(cfg)
		return nil
	case "off":
		cfg.JobNotify = nil
		if err := config.Save(cfg); err != nil {
			return clierr.Errf(1, "error: %v", err)
		}
		fmt.Println("job notifications: disabled")
		return nil
	case "webhook":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv jobs notify webhook <URL|->")
		}
		url := args[1]
		if cfg.JobNotify == nil {
			cfg.JobNotify = &config.JobNotifyConfig{}
		}
		if url == "-" || url == "" {
			cfg.JobNotify.Webhook = ""
		} else {
			cfg.JobNotify.Webhook = url
		}
		// If everything is now empty, drop the whole block so the
		// daemon's watcher knows there's nothing to do.
		if !cfg.JobNotify.Local && cfg.JobNotify.Webhook == "" {
			cfg.JobNotify = nil
		}
		if err := config.Save(cfg); err != nil {
			return clierr.Errf(1, "error: %v", err)
		}
		printNotifyState(cfg)
		return nil
	case "test":
		p := jobnotify.Payload{
			ID:       "test",
			Profile:  "(none)",
			Cmd:      "srv jobs notify test",
			Started:  time.Now().Format(time.RFC3339),
			Finished: time.Now().Format(time.RFC3339),
		}
		anyFired := false
		if cfg.JobNotify != nil && cfg.JobNotify.Local {
			if err := jobnotify.LocalToast(p); err != nil {
				fmt.Fprintf(os.Stderr, "local toast failed: %v\n", err)
			} else {
				fmt.Println("local toast: fired")
				anyFired = true
			}
		}
		if cfg.JobNotify != nil && cfg.JobNotify.Webhook != "" {
			if err := jobnotify.Webhook(cfg.JobNotify.Webhook, p); err != nil {
				fmt.Fprintf(os.Stderr, "webhook failed: %v\n", err)
			} else {
				fmt.Println("webhook: posted ok")
				anyFired = true
			}
		}
		if !anyFired {
			fmt.Println("(nothing configured; try `srv jobs notify on` or `srv jobs notify webhook <URL>`)")
		}
		return nil
	}
	return clierr.Errf(2, "usage: srv jobs notify [on|off|webhook URL|test]")
}

func printNotifyState(cfg *config.Config) {
	if cfg.JobNotify == nil {
		fmt.Println("job notifications: disabled")
		fmt.Println("enable with: srv jobs notify on")
		return
	}
	fmt.Printf("local toast: %v\n", cfg.JobNotify.Local)
	wh := cfg.JobNotify.Webhook
	if wh == "" {
		wh = "(none)"
	}
	fmt.Printf("webhook:     %s\n", wh)
	fmt.Println("test with:   srv jobs notify test")
}

// CmdJobs prints the active job table. profileOverride filters
// the list down to one profile when set. With --watch (or -w) the
// command flips into a TUI loop -- see CmdJobsWatch. The first
// positional `notify` switches into the notification-config CLI.
func CmdJobs(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) > 0 && args[0] == "notify" {
		return cmdJobsNotify(args[1:], cfg)
	}
	for i, a := range args {
		if a == "--watch" || a == "-w" {
			// Remove the flag so the watch helper sees the remainder
			// (e.g. -n 2s) untouched.
			rest := append([]string{}, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return CmdJobsWatch(rest, cfg, profileOverride)
		}
	}
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
