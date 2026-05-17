package daemon

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// underGoTest reports whether this process is a `go test` binary.
//
// SAFETY-CRITICAL. It gates daemon auto-spawn (see Ensure). Inside a
// test binary os.Executable() is the compiled <pkg>.test[.exe] -- NOT
// `srv`. spawnDaemonDetached would exec `<pkg>.test daemon`; a Go test
// binary ignores the unknown positional arg and RE-RUNS THE WHOLE TEST
// SUITE. exec.Command inherits the environment, so any gate env (e.g.
// SRV_LIVE_PROFILE) carries over and the suite does NOT skip -- it
// calls RunCapture -> TryRunCapture -> Ensure -> spawnDaemonDetached
// again, once per RunCapture, recursively. The result is an
// exponential fork bomb of test processes that locks up the machine
// (it has, twice). The per-process liveMu in the live suite cannot
// help: the bomb is multi-process.
//
// Detected WITHOUT importing `testing` -- that would register -test.*
// flags and pull test machinery into the production `srv` binary. Two
// independent signals, either conclusive:
//
//   - The testing package registers `-test.v` on flag.CommandLine
//     before any test runs (and spawnDaemonDetached only ever runs
//     mid-test), so flag.Lookup("test.v") != nil iff we are a test.
//   - `go test` / `go test -c` binaries are named `<pkg>.test[.exe]`
//     -- a backstop for a custom TestMain that hasn't parsed flags.
//
// A real `srv` binary matches neither, so production is unaffected.
func underGoTest() bool {
	if flag.Lookup("test.v") != nil {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	base := strings.ToLower(filepath.Base(exe))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}
