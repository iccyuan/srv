package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"srv/internal/srvio"
	"srv/internal/srvpath"
	"strings"
	"time"
)

// MCP session replay: every tools/call (args + result + duration) is
// appended as one JSON line under ~/.srv/mcp-replay.jsonl. The CLI
// `srv mcp replay` queries / pretty-prints this; the file format is
// designed to be valid JSONL so `jq` / `grep` work too.
//
// Separate from mcp-stats.jsonl: stats is a slim aggregate (durations,
// byte counts, ok/err) safe to ship to dashboards; replay is the full
// args+result blob the model exchanged, which can leak credentials in
// command strings and shouldn't be uploaded by default. Splitting the
// files lets users delete one without losing the other.

// replayEntry is the on-disk shape. Json field names stay short
// because the file is append-mostly and many small saved tokens add
// up over a long conversation.
type replayEntry struct {
	TS            time.Time      `json:"ts"`
	Tool          string         `json:"tool"`
	Args          map[string]any `json:"args"`
	Result        toolResult     `json:"result"`
	DurMs         int64          `json:"dur_ms"`
	ProgressBytes int            `json:"progress_bytes,omitempty"`
}

// replayPath is the JSONL file. Honors $SRV_HOME via srvpath.
func replayPath() string { return filepath.Join(srvpath.Dir(), "mcp-replay.jsonl") }

// replayMaxBytes caps how big the file is allowed to grow before we
// rotate the oldest half off. ~5 MB is roughly 5k average-sized
// records, enough to span a full work session.
const replayMaxBytes = 5 * 1024 * 1024

// appendReplay writes one entry and (best-effort) trims the file when
// it crosses the size cap. Failures are returned but the MCP loop
// ignores them -- the replay log is observability, not authoritative
// state, so a missing entry shouldn't taint the call result the
// client sees.
func appendReplay(e replayEntry) error {
	path := replayPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(b); werr != nil {
		f.Close()
		return werr
	}
	f.Close()
	if st, serr := os.Stat(path); serr == nil && st.Size() > replayMaxBytes {
		_ = trimReplay(path)
	}
	return nil
}

// trimReplay drops the older half of the file when it crosses the
// size cap. JSONL trimming has to start on a complete line so we read
// the whole file, slice in half, then resnap to the first newline.
func trimReplay(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	half := len(data) / 2
	nl := -1
	for i := half; i < len(data); i++ {
		if data[i] == '\n' {
			nl = i + 1
			break
		}
	}
	if nl < 0 {
		nl = half
	}
	return srvio.WriteFileAtomic(path, data[nl:], 0o600)
}

// ReadReplay reads every entry from disk in order. Used by the
// `srv mcp replay` CLI; not called from the hot MCP loop path.
func ReadReplay() ([]replayEntry, error) {
	f, err := os.Open(replayPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var out []replayEntry
	for {
		line, err := br.ReadString('\n')
		if t := strings.TrimSpace(line); t != "" {
			var e replayEntry
			if jerr := json.Unmarshal([]byte(t), &e); jerr == nil {
				out = append(out, e)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// ReplayClear truncates the replay file. Surfaced as `srv mcp replay clear`.
func ReplayClear() error {
	p := replayPath()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil
	}
	return os.Truncate(p, 0)
}

// ReplayPath is exported for `srv mcp replay path`.
func ReplayPath() string { return replayPath() }

// ReplayCmd implements `srv mcp replay [...]` invoked from the
// `srv mcp` subcommand router in commands.go. Kept here so the
// replay file's schema stays one package.
func ReplayCmd(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "path":
			fmt.Println(replayPath())
			return nil
		case "clear":
			if err := ReplayClear(); err != nil {
				return err
			}
			fmt.Println("cleared")
			return nil
		case "show":
			if len(args) < 2 {
				return fmt.Errorf("usage: srv mcp replay show <index>")
			}
			return replayShow(args[1])
		}
	}
	return replayList(args)
}

func replayShow(idxStr string) error {
	entries, err := ReadReplay()
	if err != nil {
		return err
	}
	idx := 0
	for i, r := range idxStr {
		if r < '0' || r > '9' {
			return fmt.Errorf("index must be a non-negative integer (got %q at byte %d)", idxStr, i)
		}
		idx = idx*10 + int(r-'0')
	}
	if idx >= len(entries) {
		return fmt.Errorf("index %d out of range [0,%d)", idx, len(entries))
	}
	b, err := json.MarshalIndent(entries[idx], "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func replayList(args []string) error {
	limit := 20
	tool := ""
	since := time.Duration(0)
	jsonOut := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--limit":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &limit)
				i++
			}
		case a == "--tool":
			if i+1 < len(args) {
				tool = args[i+1]
				i++
			}
		case a == "--since":
			if i+1 < len(args) {
				if d, err := time.ParseDuration(args[i+1]); err == nil {
					since = d
				}
				i++
			}
		case a == "--json":
			jsonOut = true
		case a == "list":
			// no-op (default action)
		}
	}
	entries, err := ReadReplay()
	if err != nil {
		return err
	}
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	filtered := entries[:0]
	for _, e := range entries {
		if tool != "" && e.Tool != tool {
			continue
		}
		if !cutoff.IsZero() && e.TS.Before(cutoff) {
			continue
		}
		filtered = append(filtered, e)
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	if jsonOut {
		for _, e := range filtered {
			b, _ := json.Marshal(e)
			fmt.Println(string(b))
		}
		return nil
	}
	if len(filtered) == 0 {
		fmt.Println("(no replay entries match)")
		return nil
	}
	// Indexes count from 0 across the WHOLE file so `replay show <n>`
	// stays stable across filtered listings.
	start := len(entries) - len(filtered)
	for i, e := range filtered {
		ok := "ok"
		if e.Result.IsError {
			ok = "err"
		}
		when := e.TS.Format("15:04:05")
		fmt.Printf("[%d] %s  %-12s %s  %dms\n", start+i, when, e.Tool, ok, e.DurMs)
	}
	fmt.Println()
	fmt.Println("see one in full:   srv mcp replay show <index>")
	return nil
}
