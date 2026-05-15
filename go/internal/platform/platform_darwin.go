//go:build darwin

package platform

import "os/exec"

func init() {
	Proc = darwinProcess{}
	Term = unixConsole{}
	Sec = unixCrypto{}
	Stats = darwinStats{}
	Notif = darwinNotifier{}
	Open = darwinOpener{}
	Sh = unixShell{}
}

// darwinOpener uses the system `open` command -- the macOS-native
// way to hand a file or URL to the user's default app. Differs from
// xdg-open in that there's no per-DE indirection; macOS routes
// directly through LaunchServices.
type darwinOpener struct{}

func (darwinOpener) Open(path string) error {
	return exec.Command("open", path).Start()
}

// darwinProcess extends unixProcessBase. macOS has no /proc
// (well, no procfs by default), so PIDStartTime falls back to
// "unknown" -- the caller (pidAliveMatch) handles the (0, false)
// case by degrading to PID-only liveness.
//
// A future implementation could call sysctl with the kern.proc.pid
// MIB to get kinfo_proc.kp_proc.p_starttime (a struct timeval),
// which is the same data ps(1) uses. That requires cgo or some
// syscall plumbing; left as a follow-up. The fallback path is
// correctness-preserving (no false-alive on PID reuse, just less
// information), so this is a soft gap rather than a bug.
type darwinProcess struct {
	unixProcessBase
}

func (darwinProcess) PIDStartTime(int) (int64, bool) {
	return 0, false
}

// darwinNotifier shells out to osascript for a native notification.
// Requires the user's TCC permission for the calling terminal /
// process group to display notifications; on first invocation
// macOS prompts the user.
type darwinNotifier struct{}

func (darwinNotifier) Toast(title, body string) error {
	// AppleScript escapes its string literals by doubling internal
	// quotes; we use %q which produces a Go-quoted string and
	// happens to be valid AppleScript for the case of no embedded
	// backslashes. For long bodies osascript truncates at the
	// system limit silently; nothing we can do about that.
	script := "display notification " + applescriptQuote(body) +
		" with title " + applescriptQuote(title)
	return exec.Command("osascript", "-e", script).Run()
}

// applescriptQuote wraps `s` in double quotes after escaping the
// AppleScript-special characters (only ` and \"). Simpler than
// reaching for a full parser; covers what notification text needs.
func applescriptQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	out = append(out, '"')
	return string(out)
}
