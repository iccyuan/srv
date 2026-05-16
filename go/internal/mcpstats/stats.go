// Package mcpstats records and queries per-call MCP token-budget
// telemetry. Every tools/call the stdio MCP server handles appends
// one JSON line to ~/.srv/mcp-stats.jsonl with:
//
//	ts             ISO-8601 timestamp when the call started
//	tool           tool name (e.g. "run", "journal", "tail")
//	dur_ms         wall-clock duration
//	in_bytes       size of the args JSON the client sent
//	out_bytes      size of the result JSON the server sent back
//	                  (the model spends tokens to read all of this)
//	progress_bytes total bytes streamed as notifications/progress
//	                  during the call -- NOT capped by the
//	                  out_bytes truncation marker
//	ok             whether the tool returned IsError=false
//
// "Bytes" not "tokens" is the honest unit -- we don't tokenize
// here. The CLI reports both bytes (authoritative) and an estimated
// token count (bytes/4, useful for spot-comparing against Claude's
// context budget).
//
// File is JSON-Lines (one record per line), append-only. A typical
// session writes ~50-200 lines; a long-running deployment can
// accumulate thousands. `srv stats --clear` wipes it.
package mcpstats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"srv/internal/srvpath"
	"strings"
	"sync"
	"time"
)

// Call is one row in the stats log: a single tools/call invocation.
type Call struct {
	TS            time.Time `json:"ts"`
	Tool          string    `json:"tool"`
	Cmd           string    `json:"cmd,omitempty"`
	DurMs         int64     `json:"dur_ms"`
	InBytes       int       `json:"in_bytes"`
	OutBytes      int       `json:"out_bytes"`
	ProgressBytes int       `json:"progress_bytes,omitempty"`
	OK            bool      `json:"ok"`
}

// CmdMaxLen bounds the per-record Cmd field so a 50 KB shell script
// passed as args["command"] doesn't bloat the JSONL file. Truncation
// is best-effort identification, not a faithful replay.
const CmdMaxLen = 200

// DescribeArgs returns a one-line human-readable summary of the most
// informative arg for `tool`. Priority list per tool:
//
//	run / detach                       args["command"]
//	run_group                          "[group=X] " + args["command"]
//	tail / list_dir / cd               args["path"]
//	tail_log / wait_job / kill_job     args["id"]
//	journal                            "unit=X since=Y" (whichever set)
//	use                                args["profile"]
//	diff                               args["local"]
//	push                               args["local"]
//	pull                               args["remote"]
//	env                                "action key" when action != list
//	everything else                    "" (no meaningful arg)
//
// Output is truncated to CmdMaxLen so log rows stay bounded. Empty
// return means the tool has no naturally identifying arg (status /
// pwd / list_profiles / doctor / daemon_status / list_jobs /
// sync_delete_dry_run). The CLI just leaves the column blank for
// those rows.
func DescribeArgs(tool string, args map[string]any) string {
	get := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	var out string
	switch tool {
	case "run", "detach":
		out = get("command")
	case "run_group":
		cmd := get("command")
		grp := get("group")
		if grp != "" {
			out = "[" + grp + "] " + cmd
		} else {
			out = cmd
		}
	case "tail", "list_dir", "cd":
		out = get("path")
	case "tail_log", "wait_job", "kill_job":
		out = get("id")
	case "journal":
		var parts []string
		if v := get("unit"); v != "" {
			parts = append(parts, "unit="+v)
		}
		if v := get("since"); v != "" {
			parts = append(parts, "since="+v)
		}
		if v := get("priority"); v != "" {
			parts = append(parts, "priority="+v)
		}
		if v := get("grep"); v != "" {
			parts = append(parts, "grep="+v)
		}
		out = strings.Join(parts, " ")
	case "use":
		out = get("profile")
	case "diff", "push":
		out = get("local")
	case "pull":
		out = get("remote")
	case "env":
		action := get("action")
		if action == "" {
			action = "list"
		}
		if action == "list" || action == "clear" {
			out = action
		} else {
			out = action + " " + get("key")
		}
	}
	if len(out) > CmdMaxLen {
		out = out[:CmdMaxLen-3] + "..."
	}
	return out
}

// EstTokens is a coarse byte→token estimate. The real tokenizer
// (tiktoken-like) varies with content; bytes/4 is a serviceable
// proxy for English-ish output and JSON-ish results. Useful for
// "this tool's calls cost roughly N tokens" comparisons, NOT for
// billing.
func (c Call) EstTokens() int {
	return (c.InBytes + c.OutBytes + c.ProgressBytes) / 4
}

// appendMu serializes JSONL writes so concurrent MCP servers (a
// rarity but not impossible -- one Claude Code window per project)
// don't interleave partial lines.
var appendMu sync.Mutex

// pathFn returns the stats-file path. Overridable in tests so the
// round-trip suite can write to t.TempDir without touching the
// user's actual ~/.srv/mcp-stats.jsonl.
var pathFn = func() string { return srvpath.MCPStats() }

// maxFileBytes is the soft cap before AppendCall rotates. Picked at
// 10 MiB: at ~200 B/record that's roughly 50k calls -- a deeply
// active Claude Code user might hit this every few weeks. Rotation
// keeps one historical generation as `<path>.1`; older history is
// dropped. Users wanting full history can pre-copy the file before
// rotation fires.
const maxFileBytes int64 = 10 * 1024 * 1024

// AppendCall writes one Call as a JSON line to the stats file. The
// MCP loop calls this synchronously after each tools/call returns;
// the write is small (~200 bytes) and rare (per-call) so the disk
// cost is invisible compared to the call itself.
//
// Best-effort: errors (read-only home, full disk, etc.) are
// returned but the loop's caller ignores them -- stats are
// observability, not authoritative state.
func AppendCall(c Call) error {
	if c.TS.IsZero() {
		c.TS = time.Now()
	}
	line, err := json.Marshal(c)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	appendMu.Lock()
	defer appendMu.Unlock()
	path := pathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rotateIfLarge(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// rotateIfLarge renames `path` to `path.1` (overwriting any prior
// `.1`) when the current file has grown past maxFileBytes. Caller
// must hold appendMu. Best-effort -- a failed rename leaves the
// file in place and the next AppendCall just keeps writing past
// the cap. No multi-generation rotation: one historical slot is
// enough for "I just want to see what I had before the rotation."
func rotateIfLarge(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() < maxFileBytes {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

// LoadCalls reads the stats file and returns every Call whose TS
// is >= `since`. Pass time.Time{} to include all records.
//
// Malformed lines are skipped silently (the file is best-effort
// telemetry -- one bad record shouldn't tank the report). Missing
// file returns (nil, nil).
func LoadCalls(since time.Time) ([]Call, error) {
	f, err := os.Open(pathFn())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Call
	sc := bufio.NewScanner(f)
	// Long-line buffer: a single Call line should be <1KB but a
	// pathological tool returning a giant structuredContent could
	// push higher. 1 MiB is generous; anything larger hits the
	// JSONL-corruption path and we skip it.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			continue
		}
		if !since.IsZero() && c.TS.Before(since) {
			continue
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// Clear deletes the stats file (and its rotated `.1` sibling +
// the checkpoint file). Idempotent on missing files.
func Clear() error {
	path := pathFn()
	for _, p := range []string{path, path + ".1", checkpointPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// checkpointPath returns the location of the "last viewed at"
// marker that drives `--since-last`. Lives next to the stats file
// so a single `--clear` resets both.
func checkpointPath() string { return pathFn() + ".checkpoint" }

// LoadCheckpoint returns the timestamp the user last viewed stats
// (via `srv mcp stats --since-last`). Zero time when no checkpoint
// has been written yet.
func LoadCheckpoint() time.Time {
	data, err := os.ReadFile(checkpointPath())
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, string(data))
	if err != nil {
		return time.Time{}
	}
	return t
}

// SaveCheckpoint stamps `now` as the new "last viewed at" anchor.
// Called by `srv mcp stats --since-last` on success so the next
// invocation shows the delta since this one.
func SaveCheckpoint(now time.Time) error {
	return os.WriteFile(checkpointPath(), []byte(now.Format(time.RFC3339Nano)), 0o600)
}

// Aggregate is the per-tool rollup produced by Aggregate(). Sorted
// by TotalOutBytes descending so the largest token-consumers float
// to the top of `srv stats`.
type Aggregate struct {
	Tool string
	// Cmd is populated only by AggregateByToolCmd. Empty in the
	// by-tool rollup since one tool's rows fan out across many
	// commands.
	Cmd            string
	Calls          int
	TotalInBytes   int64
	TotalOutBytes  int64
	TotalProgress  int64
	AvgOutBytes    int
	MaxOutBytes    int
	P50OutBytes    int
	P95OutBytes    int
	AvgDurMs       float64
	Errors         int
	EstTotalTokens int
}

// AggregateByTool rolls a slice of Calls up to one Aggregate per
// tool. Percentiles are computed off OutBytes (the field that maps
// to tokens spent reading the result -- the most actionable signal
// for "this tool needs tighter truncation").
func AggregateByTool(calls []Call) []Aggregate {
	return aggregateBy(calls, func(c Call) string { return c.Tool }, func(key string, a *Aggregate) {
		a.Tool = key
	})
}

// AggregateByToolCmd rolls Calls up to one Aggregate per (tool, cmd)
// pair. Surfaces "this specific invocation is the token hog"
// patterns that AggregateByTool flattens: a single `run` row in the
// by-tool view might collapse 50 small ls calls + one giant
// `make -j build` -- by-cmd splits them so the build's cost stands
// out.
//
// Calls with empty Cmd (tools where DescribeArgs has no meaningful
// arg, e.g. `pwd`) keep their tool name as the key so they don't
// vanish from the report.
func AggregateByToolCmd(calls []Call) []Aggregate {
	return aggregateBy(calls,
		func(c Call) string {
			if c.Cmd == "" {
				return c.Tool + "\x00"
			}
			return c.Tool + "\x00" + c.Cmd
		},
		func(key string, a *Aggregate) {
			// Split tool / cmd back out. The \x00 separator can't
			// occur in either of the source strings (tool names are
			// ASCII alpha, Cmd is truncated user input but JSON
			// already strips control chars).
			parts := strings.SplitN(key, "\x00", 2)
			a.Tool = parts[0]
			if len(parts) > 1 {
				a.Cmd = parts[1]
			}
		})
}

// aggregateBy is the shared body of the by-tool / by-(tool,cmd)
// rollups. keyFn maps each Call to a grouping key; finalize is
// called once per group to populate identifying fields on the
// finished Aggregate.
func aggregateBy(calls []Call, keyFn func(Call) string, finalize func(string, *Aggregate)) []Aggregate {
	byKey := map[string]*Aggregate{}
	outsByKey := map[string][]int{}
	for _, c := range calls {
		k := keyFn(c)
		a, ok := byKey[k]
		if !ok {
			a = &Aggregate{}
			byKey[k] = a
		}
		a.Calls++
		a.TotalInBytes += int64(c.InBytes)
		a.TotalOutBytes += int64(c.OutBytes)
		a.TotalProgress += int64(c.ProgressBytes)
		a.AvgDurMs += float64(c.DurMs)
		if c.OutBytes > a.MaxOutBytes {
			a.MaxOutBytes = c.OutBytes
		}
		if !c.OK {
			a.Errors++
		}
		outsByKey[k] = append(outsByKey[k], c.OutBytes)
	}
	out := make([]Aggregate, 0, len(byKey))
	for k, a := range byKey {
		finalize(k, a)
		if a.Calls > 0 {
			a.AvgOutBytes = int(a.TotalOutBytes / int64(a.Calls))
			a.AvgDurMs /= float64(a.Calls)
		}
		a.P50OutBytes = percentile(outsByKey[k], 50)
		a.P95OutBytes = percentile(outsByKey[k], 95)
		a.EstTotalTokens = int((a.TotalInBytes + a.TotalOutBytes + a.TotalProgress) / 4)
		out = append(out, *a)
	}
	return out
}

// percentile picks the p-th percentile from `xs` (0 ≤ p ≤ 100).
// Nearest-rank method; returns 0 on empty input. Mutates xs (sorts
// in-place) -- callers that need the input intact must pass a copy.
func percentile(xs []int, p int) int {
	if len(xs) == 0 {
		return 0
	}
	// Sort ascending. insertion sort is fast enough; per-tool
	// vectors are typically < 200 entries.
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
	idx := (p * (len(xs) - 1)) / 100
	if idx < 0 {
		idx = 0
	}
	if idx >= len(xs) {
		idx = len(xs) - 1
	}
	return xs[idx]
}
