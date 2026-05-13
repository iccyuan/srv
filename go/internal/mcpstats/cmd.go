package mcpstats

import (
	"encoding/json"
	"fmt"
	"sort"
	"srv/internal/ansi"
	"srv/internal/clierr"
	"strings"
	"time"
)

// Cmd implements `srv mcp stats [tool] [flags]`.
//
//	srv mcp stats                      top tools by total out bytes (7d default)
//	srv mcp stats <tool>               drill into one tool's recent calls
//	srv mcp stats --since 24h          adjust time window
//	srv mcp stats --json               JSON output (for scripts)
//	srv mcp stats --clear              wipe ~/.srv/mcp-stats.jsonl
//	srv mcp stats --top N              limit aggregate rows (default 20)
//	srv mcp stats --calls N            limit per-tool drill rows (default 20)
//
// The default view is the aggregate table: one row per MCP tool,
// sorted by total output bytes (descending) so the biggest token
// hogs surface at the top. Per-tool drill is a chronological list
// of recent calls so you can see which specific invocation was
// expensive.
func Cmd(args []string) error {
	var (
		toolFilter string
		since      = 7 * 24 * time.Hour
		jsonOut    bool
		clearFlag  bool
		sinceLast  bool
		byCmd      bool
		topLimit   = 20
		callsLimit = 20
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--clear":
			clearFlag = true
		case a == "--since-last":
			sinceLast = true
		case a == "--by-cmd":
			byCmd = true
		case a == "--since" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return clierr.Errf(2, "bad --since %q: %v", args[i+1], err)
			}
			since = d
			i++
		case strings.HasPrefix(a, "--since="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return clierr.Errf(2, "bad --since %q: %v", a, err)
			}
			since = d
		case a == "--top" && i+1 < len(args):
			n, err := strconvPositive(args[i+1])
			if err != nil {
				return clierr.Errf(2, "bad --top %q: %v", args[i+1], err)
			}
			topLimit = n
			i++
		case a == "--calls" && i+1 < len(args):
			n, err := strconvPositive(args[i+1])
			if err != nil {
				return clierr.Errf(2, "bad --calls %q: %v", args[i+1], err)
			}
			callsLimit = n
			i++
		case a == "-h" || a == "--help":
			fmt.Println(helpText)
			return nil
		case strings.HasPrefix(a, "-"):
			return clierr.Errf(2, "unknown flag %q (try --help)", a)
		default:
			toolFilter = a
		}
	}

	if clearFlag {
		if err := Clear(); err != nil {
			return clierr.Errf(1, "clear: %v", err)
		}
		fmt.Println("stats cleared")
		return nil
	}

	now := time.Now()
	cutoff := now.Add(-since)
	windowLabel := fmtDuration(since)
	if sinceLast {
		cp := LoadCheckpoint()
		if cp.IsZero() {
			// Treat first-ever --since-last as "everything", since
			// we have nothing to compare against. The user will get
			// a baseline; subsequent calls show the delta.
			cutoff = time.Time{}
			windowLabel = "all time (first --since-last)"
		} else {
			cutoff = cp
			windowLabel = "since " + cp.Format("2006-01-02 15:04:05")
		}
	}
	calls, err := LoadCalls(cutoff)
	if err != nil {
		return clierr.Errf(1, "load: %v", err)
	}
	if len(calls) == 0 {
		if jsonOut {
			fmt.Println("[]")
		} else {
			fmt.Printf("(no MCP calls recorded in %s)\n", windowLabel)
			fmt.Println("the stats file is written by the MCP server; if you've never run")
			fmt.Println("`srv mcp` (i.e. never used srv from Claude Code), nothing is logged yet.")
		}
		if sinceLast {
			_ = SaveCheckpoint(now)
		}
		return nil
	}

	var renderErr error
	switch {
	case toolFilter != "":
		renderErr = renderTool(toolFilter, calls, windowLabel, callsLimit, jsonOut)
	case byCmd:
		renderErr = renderAggregateByCmd(calls, windowLabel, topLimit, jsonOut)
	default:
		renderErr = renderAggregate(calls, windowLabel, topLimit, jsonOut)
	}
	if renderErr == nil && sinceLast {
		_ = SaveCheckpoint(now)
	}
	return renderErr
}

const helpText = `srv mcp stats -- inspect MCP token-budget telemetry

USAGE:
  srv mcp stats [tool] [flags]

EXAMPLES:
  srv mcp stats                      top tools by total output bytes (last 7d)
  srv mcp stats --by-cmd             top (tool, cmd) pairs -- splits one tool's
                                     budget across the specific invocations
  srv mcp stats --since 24h          last 24 hours
  srv mcp stats journal              recent calls to the 'journal' tool,
                                     with each call's cmd / path / unit shown
  srv mcp stats journal --calls 50   last 50 'journal' calls
  srv mcp stats --json               machine-readable output
  srv mcp stats --clear              wipe ~/.srv/mcp-stats.jsonl

FLAGS:
  --since DURATION   time window (e.g. 1h, 30m, 7d). default 7d (168h).
  --since-last       only calls since the last --since-last view
                     (writes a checkpoint on success).
  --by-cmd           aggregate by (tool, cmd) instead of just tool.
                     Surfaces "this specific invocation is the hog" patterns.
  --top N            number of aggregate rows. default 20.
  --calls N          number of per-tool drill rows. default 20.
  --json             JSON output.
  --clear            delete the stats file (and rotated history).

The "out" columns are the bytes the model has to read; "progress" is
streamed during the call and is NOT capped by the result-text truncation.
EST TOKENS is bytes/4 -- approximate, useful for relative comparisons.`

func renderAggregate(calls []Call, windowLabel string, topLimit int, jsonOut bool) error {
	aggs := AggregateByTool(calls)
	sort.Slice(aggs, func(i, j int) bool {
		return aggs[i].TotalOutBytes+aggs[i].TotalProgress >
			aggs[j].TotalOutBytes+aggs[j].TotalProgress
	})
	if topLimit > 0 && len(aggs) > topLimit {
		aggs = aggs[:topLimit]
	}
	if jsonOut {
		b, _ := json.MarshalIndent(aggs, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("MCP token stats -- %s, %d calls across %d tools\n\n",
		windowLabel, len(calls), len(aggs))
	fmt.Printf("%-18s %6s %10s %10s %10s %10s %12s %6s\n",
		"TOOL", "CALLS", "AVG OUT", "P95 OUT", "MAX OUT", "TOTAL OUT", "EST TOKENS", "ERRS")
	fmt.Println(strings.Repeat("-", 100))
	for _, a := range aggs {
		line := fmt.Sprintf("%-18s %6d %10s %10s %10s %10s %12s %6d",
			truncateRight(a.Tool, 18),
			a.Calls,
			fmtBytes(int64(a.AvgOutBytes)),
			fmtBytes(int64(a.P95OutBytes)),
			fmtBytes(int64(a.MaxOutBytes)),
			fmtBytes(a.TotalOutBytes),
			fmtCount(int64(a.EstTotalTokens)),
			a.Errors,
		)
		// Highlight outlier-shaped tools: Max >> P95 means a typical
		// call is small but a few are huge -- the worst kind of
		// hidden token spend. 3x is the boundary where "noisy"
		// becomes "investigate now".
		if a.P95OutBytes > 0 && a.MaxOutBytes > a.P95OutBytes*3 {
			line = ansi.Red + line + ansi.Reset
		}
		fmt.Println(line)
	}
	// Progress bytes get a separate summary line below the table --
	// they're often zero for non-streaming tools so a dedicated
	// column would mostly be empty space.
	var totalProgress int64
	for _, a := range aggs {
		totalProgress += a.TotalProgress
	}
	if totalProgress > 0 {
		fmt.Println()
		fmt.Printf("streaming progress notifications: %s across all tools (uncapped by out_bytes)\n",
			fmtBytes(totalProgress))
	}
	// Footer legend so the red rows make sense without consulting docs.
	hasOutlier := false
	for _, a := range aggs {
		if a.P95OutBytes > 0 && a.MaxOutBytes > a.P95OutBytes*3 {
			hasOutlier = true
			break
		}
	}
	if hasOutlier {
		fmt.Println()
		fmt.Println(ansi.Dim + "red rows: MAX OUT > 3x P95 OUT -- the average call is fine but a few are pathological; check `srv mcp stats <tool>` to find them." + ansi.Reset)
	}
	return nil
}

// renderAggregateByCmd is the --by-cmd alternate view: one row per
// distinct (tool, cmd) pair instead of one row per tool. Lets the
// reader see "this specific `make build` invocation accounts for
// 80% of the run-tool budget" that the by-tool view smears.
func renderAggregateByCmd(calls []Call, windowLabel string, topLimit int, jsonOut bool) error {
	aggs := AggregateByToolCmd(calls)
	sort.Slice(aggs, func(i, j int) bool {
		return aggs[i].TotalOutBytes+aggs[i].TotalProgress >
			aggs[j].TotalOutBytes+aggs[j].TotalProgress
	})
	if topLimit > 0 && len(aggs) > topLimit {
		aggs = aggs[:topLimit]
	}
	if jsonOut {
		b, _ := json.MarshalIndent(aggs, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("MCP token stats by (tool, cmd) -- %s, %d calls across %d distinct invocations\n\n",
		windowLabel, len(calls), len(aggs))
	// Same column shape as renderAggregate but with a CMD column
	// instead of putting per-tool stats next to a single name. CMD
	// is fixed at 40 chars so the row stays comparable to the
	// by-tool view's width.
	const cmdW = 40
	fmt.Printf("%-12s %-*s %6s %10s %10s %10s %12s %4s\n",
		"TOOL", cmdW, "CMD", "CALLS", "AVG OUT", "MAX OUT", "TOTAL OUT", "EST TOKENS", "ERR")
	fmt.Println(strings.Repeat("-", 28+cmdW+58))
	for _, a := range aggs {
		line := fmt.Sprintf("%-12s %-*s %6d %10s %10s %10s %12s %4d",
			truncateRight(a.Tool, 12),
			cmdW, truncateRight(a.Cmd, cmdW),
			a.Calls,
			fmtBytes(int64(a.AvgOutBytes)),
			fmtBytes(int64(a.MaxOutBytes)),
			fmtBytes(a.TotalOutBytes),
			fmtCount(int64(a.EstTotalTokens)),
			a.Errors,
		)
		if a.P95OutBytes > 0 && a.MaxOutBytes > a.P95OutBytes*3 {
			line = ansi.Red + line + ansi.Reset
		}
		fmt.Println(line)
	}
	return nil
}

func renderTool(tool string, calls []Call, windowLabel string, callsLimit int, jsonOut bool) error {
	filtered := calls[:0]
	for _, c := range calls {
		if c.Tool == tool {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == 0 {
		if jsonOut {
			fmt.Println("[]")
		} else {
			fmt.Printf("(no calls to %q in %s)\n", tool, windowLabel)
		}
		return nil
	}
	// Newest first so the most recent expensive call is at the top.
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].TS.After(filtered[j].TS)
	})
	display := filtered
	if callsLimit > 0 && len(display) > callsLimit {
		display = display[:callsLimit]
	}
	if jsonOut {
		b, _ := json.MarshalIndent(display, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("recent calls to %q -- %s, %d total (showing %d)\n\n",
		tool, windowLabel, len(filtered), len(display))
	// Column widths sum to 84 visible chars before CMD; the rest of
	// the row goes to the command summary (truncated). Fits on a
	// 120-col terminal with a roomy CMD; narrower terminals will
	// soft-wrap, which is preferable to losing the command entirely.
	const cmdW = 48
	fmt.Printf("%-19s %8s %10s %10s %10s %4s  %-*s\n",
		"WHEN", "DUR", "IN", "OUT", "PROGRESS", "OK", cmdW, "CMD")
	fmt.Println(strings.Repeat("-", 78+cmdW))
	for _, c := range display {
		okMark := "ok"
		if !c.OK {
			okMark = "err"
		}
		fmt.Printf("%-19s %7dms %10s %10s %10s %4s  %s\n",
			c.TS.Format("2006-01-02 15:04:05"),
			c.DurMs,
			fmtBytes(int64(c.InBytes)),
			fmtBytes(int64(c.OutBytes)),
			fmtBytes(int64(c.ProgressBytes)),
			okMark,
			truncateRight(c.Cmd, cmdW),
		)
	}
	return nil
}

// fmtBytes renders n as a human-readable byte count with one
// decimal where the leading digit is < 10 (e.g. "1.2 MiB" but "12
// MiB"). Anchored to KiB/MiB/GiB so "out_bytes" lines up across
// orders of magnitude.
func fmtBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const (
		KiB = 1024
		MiB = 1024 * 1024
		GiB = 1024 * 1024 * 1024
	)
	switch {
	case n >= GiB:
		return fmtUnit(float64(n)/GiB, "GiB")
	case n >= MiB:
		return fmtUnit(float64(n)/MiB, "MiB")
	default:
		return fmtUnit(float64(n)/KiB, "KiB")
	}
}

func fmtUnit(v float64, unit string) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f %s", v, unit)
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// fmtCount renders a token-count estimate the way humans skim: 1234
// → "1.2 K", 1234567 → "1.2 M". Used for the EST TOKENS column.
func fmtCount(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmtUnit(float64(n)/1000, "K")
	case n < 1_000_000_000:
		return fmtUnit(float64(n)/1_000_000, "M")
	default:
		return fmtUnit(float64(n)/1_000_000_000, "G")
	}
}

// fmtDuration renders the --since window in the same units the user
// passed (or close to it). time.Duration.String() would give us
// "168h0m0s" for a 7d window; this strips the trailing zeros.
func fmtDuration(d time.Duration) string {
	s := d.String()
	// Strip trailing "0m0s" / "0s" segments so 168h0m0s -> 168h.
	for _, sfx := range []string{"0s", "0m"} {
		s = strings.TrimSuffix(s, sfx)
	}
	if s == "" {
		return "0s"
	}
	return s
}

func truncateRight(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 3 {
		return s[:w]
	}
	return s[:w-3] + "..."
}

// strconvPositive parses `s` as a positive int. Local to avoid
// pulling strconv into the file's import list just for one call
// site -- our flag values are always small.
func strconvPositive(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("must be > 0: %q", s)
	}
	return n, nil
}
