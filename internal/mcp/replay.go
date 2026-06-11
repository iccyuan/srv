package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"srv/internal/atrest"
	"srv/internal/srvutil"
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

// replayPath is the JSONL file. Honors $SRV_HOME via srvutil.
func replayPath() string { return filepath.Join(srvutil.Dir(), "mcp-replay.jsonl") }

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
	// MCP replay entries carry the full tool args + result blob,
	// which is the most sensitive thing srv puts on disk -- creds in
	// command strings, paths in pull results, etc. When
	// SRV_AT_REST_ENCRYPT=1 each row is wrapped via internal/atrest
	// so a casual `cat ~/.srv/mcp-replay.jsonl` won't leak any of
	// it. Reader auto-detects so mixed plaintext+encrypted files
	// stay queryable.
	if atrest.Enabled() {
		if enc, encErr := atrest.EncryptLine(b); encErr == nil {
			b = enc
		}
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
	return srvutil.WriteFileAtomic(path, data[nl:], 0o600)
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
			// Plaintext lines pass through; encrypted ones get
			// AES-GCM-opened against the key file. Bad rows skip
			// rather than abort the whole listing.
			plain, decErr := atrest.DecryptLine([]byte(t))
			if decErr == nil {
				var e replayEntry
				if jerr := json.Unmarshal(plain, &e); jerr == nil {
					out = append(out, e)
				}
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
		case "diff", "--diff":
			if len(args) < 3 {
				return fmt.Errorf("usage: srv mcp replay diff <index_a> <index_b>")
			}
			return replayDiff(args[1], args[2])
		}
	}
	return replayList(args)
}

func replayShow(idxStr string) error {
	entries, err := ReadReplay()
	if err != nil {
		return err
	}
	idx, err := parseReplayIndex(idxStr, len(entries))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries[idx], "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// parseReplayIndex parses a non-negative integer and bounds-checks it
// against the entry count.
func parseReplayIndex(s string, count int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("index must be a non-negative integer (got %q)", s)
	}
	idx := 0
	for i, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("index must be a non-negative integer (got %q at byte %d)", s, i)
		}
		idx = idx*10 + int(r-'0')
	}
	if idx >= count {
		return 0, fmt.Errorf("index %d out of range [0,%d)", idx, count)
	}
	return idx, nil
}

// replayDiff renders a unified line-diff of two replay entries' args
// and result text. Use case: the model called the same tool twice and
// the user wants to see precisely what changed between calls. Diffing
// the full JSON (including dur_ms) would surface noise; sectioning
// args and result keeps the signal where it matters.
//
// Uses `git diff --no-index` for hunked, colored output when git is on
// PATH; falls back to a side-by-side block dump otherwise.
func replayDiff(aStr, bStr string) error {
	entries, err := ReadReplay()
	if err != nil {
		return err
	}
	ai, err := parseReplayIndex(aStr, len(entries))
	if err != nil {
		return err
	}
	bi, err := parseReplayIndex(bStr, len(entries))
	if err != nil {
		return err
	}
	ea, eb := entries[ai], entries[bi]

	fmt.Printf("[%d] %s\n", ai, replayHeader(ea))
	fmt.Printf("[%d] %s\n", bi, replayHeader(eb))
	if ea.Tool != eb.Tool {
		fmt.Printf("\n(note: different tools — %s vs %s)\n", ea.Tool, eb.Tool)
	}

	argsA, _ := json.MarshalIndent(ea.Args, "", "  ")
	argsB, _ := json.MarshalIndent(eb.Args, "", "  ")
	fmt.Println("\n--- args ---")
	if err := printUnified(argsA, argsB, fmt.Sprintf("[%d].args", ai), fmt.Sprintf("[%d].args", bi)); err != nil {
		return err
	}

	fmt.Println("\n--- result ---")
	return printUnified([]byte(joinResultText(ea.Result)), []byte(joinResultText(eb.Result)),
		fmt.Sprintf("[%d].result", ai), fmt.Sprintf("[%d].result", bi))
}

// replayHeader formats one replay entry as a single-line summary
// (ts / tool / ok-err / duration). Same shape as replayList's rows
// so the diff header reads like two list rows stacked.
func replayHeader(e replayEntry) string {
	ok := "ok"
	if e.Result.IsError {
		ok = "err"
	}
	return fmt.Sprintf("%s  %-12s %s  %dms", e.TS.Format("15:04:05"), e.Tool, ok, e.DurMs)
}

// joinResultText joins all text Content blocks (vs the test helper
// `resultText` which takes just Content[0].Text). The MCP toolResult
// shape is a slice of {type, text}; for diff purposes we only want
// the model-visible text, not the structured/metadata blobs (those
// are in the JSON dump from `replay show`).
func joinResultText(r toolResult) string {
	var sb strings.Builder
	for i, c := range r.Content {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// printUnified writes a unified diff of a vs b to stdout, preferring
// `git diff --no-index` for the standard hunked+colored format and
// falling back to a side-by-side block dump when git isn't available.
// Equal payloads short-circuit to "(no change)" so the reader sees an
// explicit "this section is identical" rather than blank space.
func printUnified(a, b []byte, labelA, labelB string) error {
	if bytes.Equal(a, b) {
		fmt.Println("(no change)")
		return nil
	}
	if git, err := exec.LookPath("git"); err == nil {
		tmp, err := os.MkdirTemp("", "srv-replay-diff-")
		if err == nil {
			defer os.RemoveAll(tmp)
			pa := filepath.Join(tmp, sanitizeLabel(labelA))
			pb := filepath.Join(tmp, sanitizeLabel(labelB))
			if os.WriteFile(pa, a, 0o600) == nil && os.WriteFile(pb, b, 0o600) == nil {
				cmd := exec.Command(git, "diff", "--no-index", "--", pa, pb)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				_ = cmd.Run() // exit=1 is "files differ", expected here
				return nil
			}
		}
	}
	fmt.Printf("(git not available; raw blocks)\n--- %s ---\n%s\n--- %s ---\n%s\n",
		labelA, string(a), labelB, string(b))
	return nil
}

// sanitizeLabel turns `[12].args` into a tmp-filename-safe form so the
// git diff header shows "[12].args" instead of a temp path collision.
func sanitizeLabel(s string) string {
	rep := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	out := rep.Replace(s)
	if out == "" {
		return "x"
	}
	return out
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
