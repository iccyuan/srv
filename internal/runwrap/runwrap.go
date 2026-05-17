// Package runwrap composes the small shell wrappers srv prepends to
// remote commands: resource limits (--cpu-limit / --mem-limit, via
// systemd-run when available) and supervisor-style restart loops
// (--restart-on-fail).
//
// Lives in its own package so cmdRun (foreground) and cmdDetach
// (background nohup) can both call into it without duplicating the
// shell-string assembly. The output is one POSIX-sh-safe string
// designed to be handed to a remote shell as-is.
package runwrap

import (
	"fmt"
	"strings"
	"time"
)

// Opts bundles the user-facing flags that change how a remote command
// is executed. All fields default to zero-values that disable their
// respective wrapping, so callers pass a fully-zero Opts when none
// of these features are wanted.
type Opts struct {
	// CPULimit accepts systemd-style values: "50%" (half a core),
	// "200%" (two cores), or a bare number ("75" treated as "75%").
	// Empty disables. Applied via systemd-run CPUQuota; falls back to
	// no-op (with a stderr warning) when systemd-run isn't on the
	// remote.
	CPULimit string
	// MemLimit accepts systemd MemoryMax values: "512M", "2G", "1024K".
	// Empty disables. Applied via systemd-run MemoryMax.
	MemLimit string
	// RestartOnFail:
	//   0  off (no restart wrapping)
	//  -1  unlimited retries
	//  >0  max retries
	// A run is considered "successful" (no further retry) only when
	// the wrapped command exits 0. SIGTERM / SIGINT propagate -- the
	// loop respects 130/143 and breaks instead of looping forever.
	RestartOnFail int
	// RestartDelay is the backoff between retries. <=0 falls back to
	// 5s (a sane default that doesn't hammer a misbehaving service).
	RestartDelay time.Duration
}

// Wrap returns the user's command with any configured wrappers
// prepended. The output is a single POSIX-sh-safe string. When opts
// is the zero value, the input command is returned untouched so
// existing call sites pay no cost.
//
// Wrapper order, outside-in:
//
//	restart loop  ->  systemd-run resource wrapper  ->  user command
//
// The restart loop is outermost so a crash inside systemd-run's setup
// is itself retryable; the resource wrapper sits next to the user
// command so each restart re-applies the limits cleanly.
func Wrap(cmd string, o Opts) string {
	if cmd == "" {
		return cmd
	}
	inner := cmd
	if needsResourceWrap(o) {
		inner = resourceWrap(inner, o)
	}
	if o.RestartOnFail != 0 {
		inner = restartWrap(inner, o)
	}
	return inner
}

func needsResourceWrap(o Opts) bool {
	return o.CPULimit != "" || o.MemLimit != ""
}

// resourceWrap embeds the command in a small shell snippet that uses
// systemd-run when it's on $PATH (the typical Linux server case) and
// silently falls back to running the command bare otherwise. The
// fallback prints one stderr warning so the user knows their limit
// didn't take effect rather than silently overrunning a budget.
func resourceWrap(cmd string, o Opts) string {
	props := ""
	if o.CPULimit != "" {
		val := normalizeCPU(o.CPULimit)
		props += fmt.Sprintf(" --property=CPUQuota=%s", val)
	}
	if o.MemLimit != "" {
		props += fmt.Sprintf(" --property=MemoryMax=%s", o.MemLimit)
	}
	// Quote-aware here-doc style; the inner command is run via `sh -c`
	// because systemd-run's argv split would otherwise mangle pipes.
	// The fallback branch runs the command in the current shell so
	// stdin/stdout still inherit normally.
	return fmt.Sprintf(
		`if command -v systemd-run >/dev/null 2>&1; then systemd-run --user --scope --quiet%s -- sh -c %s; else echo "srv: systemd-run not found, running without resource limits" 1>&2; sh -c %s; fi`,
		props, shQuote(cmd), shQuote(cmd),
	)
}

// restartWrap wraps a command in a retry loop. Exits 0 propagate
// immediately; non-zero exits trigger a sleep + retry up to max.
// SIGINT/SIGTERM exits (130 / 143) break out so Ctrl-C doesn't
// trap the user in an infinite restart loop.
func restartWrap(cmd string, o Opts) string {
	delay := o.RestartDelay
	if delay <= 0 {
		delay = 5 * time.Second
	}
	maxStr := ""
	if o.RestartOnFail > 0 {
		maxStr = fmt.Sprintf("%d", o.RestartOnFail)
	} else {
		maxStr = "" // empty = unlimited (loop check skips when max is unset)
	}
	delaySec := int(delay.Seconds())
	if delaySec < 1 {
		delaySec = 1
	}
	// POSIX sh loop. The `case` filters out signal-induced exits so
	// the user can still Ctrl-C / SIGTERM out of a stubborn loop.
	return fmt.Sprintf(`__srv_max=%s; __srv_n=0; __srv_delay=%d; while :; do sh -c %s; __srv_rc=$?; if [ "$__srv_rc" = 0 ]; then break; fi; case "$__srv_rc" in 130|143) echo "srv: stopped by signal (exit $__srv_rc)" 1>&2; break ;; esac; __srv_n=$((__srv_n+1)); if [ -n "$__srv_max" ] && [ "$__srv_n" -ge "$__srv_max" ]; then echo "srv: gave up after $__srv_n attempts" 1>&2; break; fi; echo "srv: command failed (exit $__srv_rc); retry $__srv_n in ${__srv_delay}s" 1>&2; sleep $__srv_delay; done; exit $__srv_rc`,
		maxStr, delaySec, shQuote(cmd))
}

// normalizeCPU accepts bare numbers ("75") and number+% ("75%").
// systemd's CPUQuota wants a percentage suffix; we always emit one.
func normalizeCPU(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		return s
	}
	return s + "%"
}

// shQuote wraps s in single quotes, escaping any embedded single quote
// the standard POSIX way (close, escape, reopen). Mirrors srvtty's
// helper but kept local so this package has zero internal deps.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
