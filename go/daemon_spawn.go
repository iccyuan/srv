package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"srv/internal/srvpath"
	"time"
)

// ensureDaemon checks whether a daemon is reachable; if not, spawns one
// in the background and waits up to ~1.5s for its socket to appear.
// Returns true if the caller can talk to a daemon afterwards.
//
// Auto-spawn is racy by design: two parallel srv invocations might both
// try to start a daemon. The second one's listen() will fail because the
// socket is already bound; Cmd detects this and exits cleanly. Net
// result: exactly one daemon survives.
func ensureDaemon() bool {
	if daemonPing() {
		return true
	}
	if err := spawnDaemonDetached(); err != nil {
		fmt.Fprintln(os.Stderr, "srv: failed to spawn daemon:", err)
		return false
	}
	// Poll for socket. The daemon has to: bind socket, start listening,
	// answer a ping. ~50ms is typical; budget 1500ms.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if daemonPing() {
			return true
		}
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

// spawnDaemonDetached starts `srv daemon` as a background process that
// outlives the parent. Stdio is detached so the parent can exit cleanly.
// Platform-specific attrs (Setsid on Unix, DETACHED_PROCESS on Windows)
// in daemon_spawn_unix.go / daemon_spawn_windows.go.
func spawnDaemonDetached() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdin = nil
	_ = os.MkdirAll(srvpath.Dir(), 0o755)
	if f, err := os.OpenFile(daemonLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	} else {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}
	applyDetachAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Don't Wait(); the OS will reap. On Unix, with Setsid the child is
	// a new session and survives parent exit. On Windows, DETACHED_PROCESS
	// + breakaway-from-job achieves the same.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

func daemonLogPath() string {
	return filepath.Join(srvpath.Dir(), "daemon.log")
}
