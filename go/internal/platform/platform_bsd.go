//go:build freebsd || netbsd || openbsd || dragonfly

package platform

func init() {
	Proc = bsdProcess{}
	Term = unixConsole{}
	Sec = unixCrypto{}
	Stats = bsdStats{}
	Notif = bsdNotifier{}
	// BSDs that have x11/freedesktop ports installed get xdg-open;
	// headless OpenBSD without it returns an exec error which the
	// caller already logs. Same fallback as Linux.
	Open = xdgOpener{}
}

// bsdProcess extends unixProcessBase. FreeBSD has /compat/linux/proc
// in some configurations, but the standard kernel exposes process
// data via kvm_proc / sysctl with KERN_PROC_PID rather than /proc
// in the Linux sense. Until we plumb that through, PIDStartTime is
// a soft fallback to (0, false) and pidAliveMatch degrades to
// PID-only liveness on these targets -- documented and acceptable.
type bsdProcess struct {
	unixProcessBase
}

func (bsdProcess) PIDStartTime(int) (int64, bool) {
	return 0, false
}

// bsdStats and bsdNotifier are similar fallbacks. Adding real
// implementations means writing sysctl wrappers for vm.loadavg /
// vm.stats.vm.* on FreeBSD and equivalents on the OpenBSD / NetBSD
// kernels; the per-OS divergence is wide enough that consolidating
// here would be wrong. Keep them as fallback stubs and split into
// platform_freebsd.go etc. when someone actually wants the data.
type bsdStats struct{}

func (bsdStats) Sample() Sample {
	return Sample{Err: "stats not implemented on BSD"}
}

type bsdNotifier struct{}

func (bsdNotifier) Toast(_, _ string) error {
	// FreeBSD has `xmessage`/`notify-send` if libnotify-bin is
	// installed; OpenBSD has neither by default. Returning an
	// error lets the caller log "no local notifier" and skip; the
	// webhook side keeps working unaffected.
	return errUnsupportedNotifier
}
