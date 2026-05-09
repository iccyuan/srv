package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const watchDebounce = 250 * time.Millisecond

// runSyncWatch enters a watch loop after the initial sync. It walks
// localRoot once to install fsnotify watchers on every non-excluded
// directory, then on each filesystem event schedules a sync after a
// short debounce. Newly-created directories get watchers added on the
// fly. Loops until SIGINT / SIGTERM.
func runSyncWatch(o *syncOpts, profile *Profile, localRoot, remoteRoot string, allExcludes []string) int {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, "watch: cannot create watcher:", err)
		return 1
	}
	defer w.Close()

	added := watchAddDirs(w, localRoot, allExcludes)
	fmt.Fprintf(os.Stderr, "watching %d directories under %s; Ctrl+C to stop.\n",
		added, localRoot)
	target := profile.Host
	if profile.User != "" {
		target = profile.User + "@" + profile.Host
	}
	fmt.Fprintf(os.Stderr, "profile=%s  target=%s:%s  mode=%s\n",
		profile.Name, target, remoteRoot, o.mode)

	// Trigger / dedup. The timer is reset on every event so we sync once
	// after a quiet period instead of on every tap.
	var (
		mu      sync.Mutex
		pending bool
		running bool
		timer   *time.Timer
	)
	var doSync func()
	doSync = func() {
		mu.Lock()
		pending = false
		if running {
			pending = true
			mu.Unlock()
			return
		}
		running = true
		mu.Unlock()
		defer func() {
			mu.Lock()
			rerun := pending
			if rerun {
				pending = false
				timer = time.AfterFunc(watchDebounce, doSync)
			}
			running = false
			mu.Unlock()
		}()
		// Re-collect each round so we pick up new files / git status changes.
		files, err := collectSyncFiles(o, localRoot, allExcludes)
		if err != nil {
			fmt.Fprintln(os.Stderr, "watch: collect:", err)
			return
		}
		ts := time.Now().Format("15:04:05")
		if len(files) == 0 {
			fmt.Fprintf(os.Stderr, "[%s] (nothing to sync)\n", ts)
			return
		}
		fmt.Fprintf(os.Stderr, "[%s] syncing %d files...\n", ts, len(files))
		rc, err := tarUploadStream(profile, localRoot, files, remoteRoot)
		if err != nil {
			printDiagError(err, profile)
			return
		}
		fmt.Fprintf(os.Stderr, "[%s] done (exit %d)\n", time.Now().Format("15:04:05"), rc)
	}
	schedule := func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		pending = true
		timer = time.AfterFunc(watchDebounce, doSync)
	}

	// Cleanly handle Ctrl+C.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return 0
			}
			rel, _ := filepath.Rel(localRoot, ev.Name)
			rel = filepath.ToSlash(rel)
			if matchesAnyExclude(rel, allExcludes) {
				continue
			}
			// New directory? Add a watcher so we pick up its children.
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					watchAddDirs(w, ev.Name, allExcludes)
				}
			}
			schedule()
		case err, ok := <-w.Errors:
			if !ok {
				return 0
			}
			fmt.Fprintln(os.Stderr, "watch:", err)
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nwatch: stopped.")
			// If something was queued, flush it before exiting.
			mu.Lock()
			needFinal := pending
			if timer != nil {
				timer.Stop()
			}
			pending = false
			mu.Unlock()
			if needFinal {
				doSync()
			}
			return 0
		}
	}
}

// watchAddDirs walks `root` and installs fsnotify watchers on each
// directory that isn't excluded. Returns the count added.
func watchAddDirs(w *fsnotify.Watcher, root string, excludes []string) int {
	added := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel != "." && matchesAnyExclude(rel, excludes) {
			return filepath.SkipDir
		}
		if err := w.Add(p); err == nil {
			added++
		}
		return nil
	})
	return added
}
