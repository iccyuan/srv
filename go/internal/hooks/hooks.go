// Package hooks fires user-defined local commands on srv lifecycle
// events (pre-cd, post-cd, pre-sync, post-sync, pre-run, post-run,
// pre-push, post-push, pre-pull, post-pull).
//
// Storage: ~/.srv/config.json `hooks` map: { eventName: [shell-cmd, ...] }.
// Each command is shelled out (`sh -c` on unix, `cmd /c` on windows),
// inheriting srv's stdin/stdout/stderr but with stdout redirected to
// stderr so a hook's chatter never corrupts a pipe like `srv ls | xargs`.
//
// Hooks are advisory: failures are reported but never block the
// underlying command. The contract is intentionally "best-effort
// notifier", not "veto authority" -- a hook framework that can break
// the user's `srv push` mid-deploy because their notify-send glitched
// is worse than no hooks at all.
//
// Each hook receives SRV_* env vars describing the event (profile,
// host, cwd, target, exit code for post-*). See Env() for the full set.
package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"srv/internal/config"
	"srv/internal/platform"
	"strings"
)

// KnownEvents enumerates the lifecycle hook points srv fires.
// Keep this list in sync with the call sites in cmdCd / syncx /
// cmdRun / cmdPush / cmdPull and with the help text in i18n/help.go.
var KnownEvents = []string{
	"pre-cd", "post-cd",
	"pre-sync", "post-sync",
	"pre-run", "post-run",
	"pre-push", "post-push",
	"pre-pull", "post-pull",
}

// Event packages the context delivered to a single hook firing.
// Optional fields stay zero / empty; Env() only emits the ones that
// have values so hooks can detect "this came from cd" vs "this came
// from sync" by checking which vars are set.
type Event struct {
	Name    string // pre-cd | post-cd | ...
	Profile string
	Host    string
	User    string
	Port    int
	Cwd     string // remote cwd at hook firing time
	Target  string // command target: new cwd (cd), remote root (sync), path (push/pull), full cmd (run)
	Local   string // local path for push/pull
	Exit    int    // for post-*: remote exit code (0 when unknown)
}

// Run fires every command configured for ev.Name in order. Errors are
// logged but never returned -- hooks are best-effort. Missing config
// or no hooks for the event is a no-op.
func Run(ev Event) {
	cmds := lookup(ev.Name)
	if len(cmds) == 0 {
		return
	}
	env := append(os.Environ(), Env(ev)...)
	for _, c := range cmds {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		cmd := buildShell(c)
		cmd.Env = env
		cmd.Stdout = os.Stderr // never collide with a piped stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "srv: hook %s: %v\n", ev.Name, err)
		}
	}
}

// Env returns the SRV_* var=value pairs for ev, suitable for appending
// to os.Environ(). Order is stable so hooks observing env-diff tools
// see deterministic output.
func Env(ev Event) []string {
	pairs := map[string]string{}
	pairs["SRV_HOOK"] = ev.Name
	if ev.Profile != "" {
		pairs["SRV_PROFILE"] = ev.Profile
	}
	if ev.Host != "" {
		pairs["SRV_HOST"] = ev.Host
	}
	if ev.User != "" {
		pairs["SRV_USER"] = ev.User
	}
	if ev.Port != 0 {
		pairs["SRV_PORT"] = fmt.Sprintf("%d", ev.Port)
	}
	if ev.Cwd != "" {
		pairs["SRV_CWD"] = ev.Cwd
	}
	if ev.Target != "" {
		pairs["SRV_TARGET"] = ev.Target
	}
	if ev.Local != "" {
		pairs["SRV_LOCAL"] = ev.Local
	}
	if strings.HasPrefix(ev.Name, "post-") {
		pairs["SRV_EXIT_CODE"] = fmt.Sprintf("%d", ev.Exit)
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+pairs[k])
	}
	return out
}

// IsKnownEvent reports whether name is a recognised lifecycle event.
// Used by `srv hooks set` to reject typos.
func IsKnownEvent(name string) bool {
	for _, k := range KnownEvents {
		if k == name {
			return true
		}
	}
	return false
}

func lookup(event string) []string {
	cfg, err := config.Load()
	if err != nil || cfg == nil || len(cfg.Hooks) == 0 {
		return nil
	}
	return cfg.Hooks[event]
}

func buildShell(cmd string) *exec.Cmd {
	// Per-OS shell selection (COMSPEC vs SHELL, /C vs -c) is
	// platform.Sh's job. This wrapper exists so the hooks package
	// stays a thin user of the abstraction; nothing here cares
	// which shell actually runs.
	return platform.Sh.Command(cmd)
}
