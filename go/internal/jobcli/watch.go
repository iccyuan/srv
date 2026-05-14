package jobcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"srv/internal/ansi"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CmdJobsWatch is `srv jobs --watch`: an in-place refreshing job
// table. Polls jobs.json + remote `.exit` markers on each tick.
// Non-TTY callers fall back to a single render and exit so pipes /
// CI don't get spammed with ANSI noise.
//
// Keys: q / Esc / Ctrl-C exit cleanly. Default tick 2s; --interval
// overrides.
func CmdJobsWatch(args []string, cfg *config.Config, profileOverride string) error {
	interval := 2 * time.Second
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--interval":
			if i+1 >= len(args) {
				return clierr.Errf(2, "%s requires a duration like 2s / 500ms", a)
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < 100*time.Millisecond {
				return clierr.Errf(2, "bad interval %q (min 100ms)", args[i+1])
			}
			interval = d
			i++
		case strings.HasPrefix(a, "--interval="):
			v := strings.TrimPrefix(a, "--interval=")
			d, err := time.ParseDuration(v)
			if err != nil || d < 100*time.Millisecond {
				return clierr.Errf(2, "bad interval %q (min 100ms)", v)
			}
			interval = d
		}
	}

	// Not a TTY -> render once and exit (CI, pipes, IDE preview).
	if !srvtty.IsStdinTTY() {
		fmt.Print(renderJobsTable(cfg, profileOverride, false))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	// Raw-mode stdin so we can read 'q' without waiting for Enter.
	// Restore() runs even on panic; the deferred order matters because
	// signal.Stop must come BEFORE we restore (caller-released sigCh)
	// to avoid a stray Ctrl-C arriving after restore but before exit.
	restore, _ := srvtty.MakeStdinRaw()
	if restore != nil {
		defer restore()
	}
	kr := srvtty.NewKeyReader()

	fmt.Print(ansi.Hide)
	defer fmt.Print(ansi.Show)
	prevLines := 0
	for {
		frame := renderJobsTable(cfg, profileOverride, true)
		srvtty.RedrawInPlace(frame, prevLines)
		prevLines = strings.Count(frame, "\n")

		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case <-time.After(interval):
		}
		// Drain pending keys between ticks. Non-blocking read with a
		// tiny budget so a key entered in the previous interval is
		// observed but we don't sit here forever.
		drainDeadline := time.Now().Add(20 * time.Millisecond)
		for time.Now().Before(drainDeadline) {
			b, ok := kr.ReadWithTimeout(5 * time.Millisecond)
			if !ok {
				break
			}
			if b == 'q' || b == 'Q' || b == 0x1b /*Esc*/ || b == 0x03 /*Ctrl-C*/ {
				fmt.Println()
				return nil
			}
		}
	}
}

// renderJobsTable composes the watch frame. Liveness probe runs
// concurrently per profile (jobs.CheckLiveness). The "live" indicator
// is the same column `srv ui` uses: alive ✓, exited ✗, unknown ?.
func renderJobsTable(cfg *config.Config, profileOverride string, ttyColor bool) string {
	rs := jobs.Load().Jobs
	if profileOverride != "" {
		filtered := rs[:0]
		for _, j := range rs {
			if j.Profile == profileOverride {
				filtered = append(filtered, j)
			}
		}
		rs = filtered
	}
	sort.Slice(rs, func(i, k int) bool { return rs[i].ID < rs[k].ID })

	header := fmt.Sprintf("srv jobs --watch  (%s, %d jobs)  q/Esc/Ctrl-C to quit\n",
		time.Now().Format("15:04:05"), len(rs))
	if !ttyColor {
		header = fmt.Sprintf("srv jobs  (%s, %d jobs)\n", time.Now().Format("15:04:05"), len(rs))
	}
	if len(rs) == 0 {
		return header + "(no jobs)\n"
	}

	// Liveness probe per profile. Done synchronously per render -- 2s
	// tick budget covers a typical pooled-SSH ls round-trip per host.
	lister := func(profName string) (map[string]bool, bool) {
		prof, ok := cfg.Profiles[profName]
		if !ok {
			return nil, false
		}
		capture := func(cmd string) (string, int, bool) {
			res, err := remote.RunCapture(prof, "", cmd)
			if err != nil || res == nil {
				return "", 0, false
			}
			return res.Stdout, res.ExitCode, true
		}
		markers := jobs.RemoteExitMarkers(capture)
		return markers, markers != nil
	}
	live := jobs.CheckLiveness(rs, lister)

	var b strings.Builder
	b.WriteString(header)
	b.WriteString(fmt.Sprintf("%-2s  %-10s  %-7s  %-10s  %-19s  %s\n",
		"", "ID", "PID", "PROFILE", "STARTED", "CMD"))
	for _, j := range rs {
		mark := "?"
		col := ansi.Yellow
		if alive, ok := live[j.ID]; ok {
			if alive {
				mark = "+"
				col = ansi.Green
			} else {
				mark = "x"
				col = ansi.Red
			}
		}
		cmd := j.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		started := j.Started
		if len(started) > 19 {
			started = started[:19]
		}
		if ttyColor {
			b.WriteString(fmt.Sprintf("%s%-2s%s  %-10s  %-7s  %-10s  %-19s  %s\n",
				col, mark, ansi.Reset, j.ID, strconv.Itoa(j.Pid), j.Profile, started, cmd))
		} else {
			b.WriteString(fmt.Sprintf("%-2s  %-10s  %-7s  %-10s  %-19s  %s\n",
				mark, j.ID, strconv.Itoa(j.Pid), j.Profile, started, cmd))
		}
	}
	b.WriteString("\nlegend: + alive   x exited   ? unreachable\n")
	return b.String()
}
