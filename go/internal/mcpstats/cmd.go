package mcpstats

import (
	"encoding/json"
	"fmt"
	"sort"
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

	cutoff := time.Now().Add(-since)
	calls, err := LoadCalls(cutoff)
	if err != nil {
		return clierr.Errf(1, "load: %v", err)
	}
	if len(calls) == 0 {
		if jsonOut {
			fmt.Println("[]")
		} else {
			fmt.Printf("(no MCP calls recorded in the last %s)\n", fmtDuration(since))
			fmt.Println("the stats file is written by the MCP server; if you've never run")
			fmt.Println("`srv mcp` (i.e. never used srv from Claude Code), nothing is logged yet.")
		}
		return nil
	}

	if toolFilter != "" {
		return renderTool(toolFilter, calls, since, callsLimit, jsonOut)
	}
	return renderAggregate(calls, since, topLimit, jsonOut)
}

const helpText = `srv mcp stats -- inspect MCP token-budget telemetry

USAGE:
  srv mcp stats [tool] [flags]

EXAMPLES:
  srv mcp stats                      top tools by total output bytes (last 7d)
  srv mcp stats --since 24h          last 24 hours
  srv mcp stats journal              recent calls to the 'journal' tool
  srv mcp stats journal --calls 50   last 50 'journal' calls
  srv mcp stats --json               machine-readable output
  srv mcp stats --clear              wipe ~/.srv/mcp-stats.jsonl

FLAGS:
  --since DURATION   time window (e.g. 1h, 30m, 7d). default 7d (168h).
  --top N            number of aggregate rows. default 20.
  --calls N          number of per-tool drill rows. default 20.
  --json             JSON output.
  --clear            delete the stats file.

The "out" columns are the bytes the model has to read; "progress" is
streamed during the call and is NOT capped by the result-text truncation.
EST TOKENS is bytes/4 -- approximate, useful for relative comparisons.`

func renderAggregate(calls []Call, since time.Duration, topLimit int, jsonOut bool) error {
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
	fmt.Printf("MCP token stats -- last %s, %d calls across %d tools\n\n",
		fmtDuration(since), len(calls), len(aggs))
	fmt.Printf("%-18s %6s %10s %10s %10s %10s %12s %6s\n",
		"TOOL", "CALLS", "AVG OUT", "P95 OUT", "MAX OUT", "TOTAL OUT", "EST TOKENS", "ERRS")
	fmt.Println(strings.Repeat("-", 100))
	for _, a := range aggs {
		fmt.Printf("%-18s %6d %10s %10s %10s %10s %12s %6d\n",
			truncateRight(a.Tool, 18),
			a.Calls,
			fmtBytes(int64(a.AvgOutBytes)),
			fmtBytes(int64(a.P95OutBytes)),
			fmtBytes(int64(a.MaxOutBytes)),
			fmtBytes(a.TotalOutBytes),
			fmtCount(int64(a.EstTotalTokens)),
			a.Errors,
		)
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
	return nil
}

func renderTool(tool string, calls []Call, since time.Duration, callsLimit int, jsonOut bool) error {
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
			fmt.Printf("(no calls to %q in the last %s)\n", tool, fmtDuration(since))
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
	fmt.Printf("recent calls to %q -- last %s, %d total (showing %d)\n\n",
		tool, fmtDuration(since), len(filtered), len(display))
	fmt.Printf("%-19s %8s %10s %10s %10s %4s\n",
		"WHEN", "DUR", "IN", "OUT", "PROGRESS", "OK")
	fmt.Println(strings.Repeat("-", 70))
	for _, c := range display {
		okMark := "ok"
		if !c.OK {
			okMark = "err"
		}
		fmt.Printf("%-19s %7dms %10s %10s %10s %4s\n",
			c.TS.Format("2006-01-02 15:04:05"),
			c.DurMs,
			fmtBytes(int64(c.InBytes)),
			fmtBytes(int64(c.OutBytes)),
			fmtBytes(int64(c.ProgressBytes)),
			okMark,
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
