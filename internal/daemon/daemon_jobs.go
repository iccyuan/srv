package daemon

import (
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/jobnotify"
	"srv/internal/jobs"
	"sync"
	"time"
)

// Daemon-side job-completion watcher. Ticks every jobNotifyInterval,
// probes remote `.exit` markers per profile, and fires notifications
// for newly-completed jobs.
//
// State persistence: each Record carries a Notified flag in jobs.json
// so a daemon restart doesn't re-toast jobs we already announced.

const (
	jobNotifyInterval = 15 * time.Second
	// Skip the watcher entirely until at least one Record exists +
	// JobNotify is enabled; the first tick after enablement primes
	// the per-profile probe cache without surfacing stale "done" rows.
	jobNotifyMinPause = 3 * time.Second
)

// runJobWatcher is the long-lived goroutine that fires job-completion
// notifications. Stops on stopCh.
func (s *daemonState) runJobWatcher() {
	ticker := time.NewTicker(jobNotifyInterval)
	defer ticker.Stop()
	// First tick is short so users testing `srv jobs notify on` don't
	// have to wait 15s to see anything happen on the next completion.
	first := time.NewTimer(jobNotifyMinPause)
	defer first.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-first.C:
			s.jobWatcherTick()
		case <-ticker.C:
			s.jobWatcherTick()
		}
	}
}

// jobWatcherTick is one polling cycle. Cheap when nothing is
// configured: bails out before any SSH activity if the user hasn't
// turned notifications on.
func (s *daemonState) jobWatcherTick() {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.JobNotify == nil {
		return
	}
	jf := jobs.Load()
	if len(jf.Jobs) == 0 {
		return
	}

	// Skip records already marked notified; we only care about ones
	// that might still be alive.
	pending := jf.Jobs[:0:0]
	for _, j := range jf.Jobs {
		if j.Notified {
			continue
		}
		pending = append(pending, j)
	}
	if len(pending) == 0 {
		return
	}

	// CheckLiveness: per-profile fanout via daemon's pooled SSH.
	lister := func(profName string) (map[string]bool, bool) {
		client, _, err := s.getClient(profName)
		if err != nil {
			return nil, false
		}
		capture := func(cmd string) (string, int, bool) {
			res, err := client.RunCapture(cmd, "")
			if err != nil || res == nil {
				return "", 0, false
			}
			return res.Stdout, res.ExitCode, true
		}
		markers := jobs.RemoteExitMarkers(capture)
		return markers, markers != nil
	}
	live := jobs.CheckLiveness(pending, lister)

	// Walk pending → mark newly-done, fire notifications, persist.
	dirty := false
	completed := []*jobs.Record{}
	for _, j := range pending {
		alive, known := live[j.ID]
		if !known || alive {
			continue
		}
		// Newly done.
		j.Finished = time.Now().Format(time.RFC3339)
		j.Notified = true
		dirty = true
		completed = append(completed, j)
	}
	if dirty {
		if err := jobs.Save(jf); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: jobs save failed: %v\n", err)
		}
	}
	if len(completed) == 0 {
		return
	}
	// Fire notifications outside the persistence path so a slow webhook
	// can't delay the next tick.
	go fireNotifications(cfg.JobNotify, completed)
}

// fireNotifications dispatches the configured channels for each
// completed job, sequentially per job (a single user doesn't want N
// toasts in parallel) but in a goroutine off the tick path so the
// watcher loop keeps rolling.
func fireNotifications(cfg *config.JobNotifyConfig, done []*jobs.Record) {
	var wg sync.WaitGroup
	for _, j := range done {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := jobnotify.ForJob(j)
			if cfg.Local {
				if err := jobnotify.LocalToast(p); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: local toast for %s failed: %v\n", j.ID, err)
				}
			}
			if cfg.Webhook != "" {
				if err := jobnotify.Webhook(cfg.Webhook, p); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: webhook for %s failed: %v\n", j.ID, err)
				}
			}
		}()
	}
	wg.Wait()
}
