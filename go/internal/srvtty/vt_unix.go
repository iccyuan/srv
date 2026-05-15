//go:build !windows

package srvtty

// EnableLocalVTOutput is a no-op on Unix terminals -- ANSI escape
// processing is the native behaviour; there's no equivalent of
// Windows's ENABLE_VIRTUAL_TERMINAL_PROCESSING flag to toggle.
// Returns a no-op restore so the cross-platform call site can defer
// it uniformly.
func EnableLocalVTOutput() func() {
	return func() {}
}
