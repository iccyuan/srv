package srvutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileLock acquires an advisory cross-shell lock for `path` and
// returns a release func. Other srv processes calling FileLock(path)
// concurrently will block until the holder releases (or until the
// retry budget elapses).
//
// Implementation: a sentinel sibling file `<path>.lock` created with
// O_EXCL. Cross-platform (no syscall.Flock / LockFileEx). Stale
// locks (older than staleAfter) are stolen so a crashed holder
// can't deadlock the next caller forever.
//
// Used by read-modify-write paths on small shared JSON state
// (sessions.json, jobs.json) where two shells issuing `srv use`
// simultaneously would otherwise race and silently drop one update.
//
// Best-effort: if the lock can't be acquired within retryBudget, an
// error is returned and the caller decides whether to bail or
// proceed unlocked (the file's WriteFileAtomic at least guarantees
// no corruption -- just lost updates).
func FileLock(path string) (release func(), err error) {
	const (
		retryBudget = time.Second
		retryEvery  = 25 * time.Millisecond
		staleAfter  = 5 * time.Second
	)
	lockPath := path + ".lock"
	deadline := time.Now().Add(retryBudget)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// Write our pid so a future stale-lock-stealer can tell
			// us apart from a crashed holder if it ever wants to.
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		// Couldn't get it; see if the holder is stale.
		if info, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(info.ModTime()) > staleAfter {
				_ = os.Remove(lockPath)
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("could not acquire %s within %v", filepath.Base(lockPath), retryBudget)
		}
		time.Sleep(retryEvery)
	}
}
