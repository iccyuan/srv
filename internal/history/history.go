// Package history records every remote command srv runs from the CLI
// path so users (and the upcoming `srv ui` history panel) can query
// what was executed, where, and with what outcome.
//
// Storage: append-only JSONL at ~/.srv/history.jsonl. JSONL was picked
// over a single JSON file so concurrent shells writing at the same
// instant don't have to read-modify-write the whole array, and so
// `tail -n` is a one-syscall lookup.
//
// Append() is best-effort: a missing or unwritable history file is
// reported once to stderr and then silently dropped. We never want
// history bookkeeping to break a real command -- if the disk is full,
// `srv ls` still has to work.
//
// MCP runs deliberately bypass this -- the model has its own
// observation channel via mcp-stats; the history file is a CLI tool
// for the user.
package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"srv/internal/atrest"
	"srv/internal/srvutil"
	"time"
)

// Path returns the on-disk history file. Honors $SRV_HOME via srvutil.
func Path() string { return filepath.Join(srvutil.Dir(), "history.jsonl") }

// MaxEntries caps the JSONL file size by rotating once it exceeds
// the limit (oldest entries dropped). 20k lines is roughly 2-4 MB,
// well under any human-scale tail/grep workload.
const MaxEntries = 20000

// rotateThreshold triggers compaction at 1.25 * MaxEntries so we
// don't pay the rotation cost on every append once we cross the cap.
const rotateThreshold = MaxEntries + MaxEntries/4

// Entry is the on-disk record. Keep the field set narrow -- this is
// append-mostly and users skim it with grep, so wider rows hurt more
// than they help.
type Entry struct {
	Time    string `json:"time"`              // RFC3339 local
	Session string `json:"session,omitempty"` // shell session id (best-effort)
	Profile string `json:"profile"`
	Host    string `json:"host,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Cmd     string `json:"cmd"`
	Exit    int    `json:"exit"`
}

// Append writes one entry to ~/.srv/history.jsonl. Errors are reported
// to stderr but not returned so the caller's command isn't disturbed.
// Auto-fills Time if the caller left it blank.
func Append(e Entry) {
	if e.Cmd == "" {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "srv: history mkdir: %v\n", err)
		return
	}
	// Append-only mode + file lock keeps two shells from interleaving
	// half-written JSON lines. The lock has a 1s budget; we'd rather
	// drop the entry than block a `srv ls` call.
	release, _ := srvutil.FileLock(path)
	if release != nil {
		defer release()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srv: history open: %v\n", err)
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		f.Close()
		return
	}
	// At-rest encryption is opt-in: when SRV_AT_REST_ENCRYPT=1 each
	// line gets wrapped in AES-GCM via internal/atrest. Reads
	// auto-detect, so flipping the env mid-stream doesn't corrupt
	// the file. Encryption failures fall back to plaintext so a
	// missing key file can't break history bookkeeping silently.
	if atrest.Enabled() {
		if enc, encErr := atrest.EncryptLine(b); encErr == nil {
			b = enc
		} else {
			fmt.Fprintf(os.Stderr, "srv: history encrypt failed (writing plain): %v\n", encErr)
		}
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "srv: history write: %v\n", err)
	}
	f.Close()

	// Cheap line-count check; if we cross the rotate threshold, drop
	// the oldest entries. Skipped most of the time -- a quick stat is
	// the typical fast path.
	if st, err := os.Stat(path); err == nil && st.Size() > 256*1024 {
		maybeRotate(path)
	}
}

// maybeRotate trims the head of the JSONL file when entry count crosses
// rotateThreshold, keeping the last MaxEntries rows. It operates on raw
// bytes -- counting and slicing on '\n' -- rather than decoding and
// re-encoding each entry. This is ~5-10x cheaper than the old parse/
// remarshal path and, importantly, preserves each row's original
// encoding: encrypted rows stay encrypted, plaintext stays plaintext,
// so the "rotating to plaintext when the flag is on would leak just-
// archived history" risk is gone by construction.
//
// Called under the Append() file lock so there's no concurrent writer
// to race against.
func maybeRotate(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	total := bytes.Count(data, []byte{'\n'})
	if total <= rotateThreshold {
		return
	}
	skip := total - MaxEntries
	cut := 0
	for range skip {
		idx := bytes.IndexByte(data[cut:], '\n')
		if idx < 0 {
			return
		}
		cut += idx + 1
	}
	_ = srvutil.WriteFileAtomic(path, data[cut:], 0o600)
}

// ReadAll loads every entry in chronological order. Used by the CLI
// `srv history` viewer and (eventually) the UI history panel.
func ReadAll() ([]Entry, error) {
	return readAll(Path())
}

func readAll(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Pre-size the output slice exactly to the newline count so
	// append() doesn't double-and-copy as the ledger grows. One
	// extra-cheap byte scan over ~4 MiB is well under a JSON parse.
	approx := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		approx++
	}
	out := make([]Entry, 0, approx)
	rest := data
	for len(rest) > 0 {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i]
			rest = rest[i+1:]
		} else {
			line = rest
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// atrest.DecryptLine returns the input unchanged when the line
		// isn't in our wrapped format, so old plaintext rows continue
		// to read fine alongside encrypted ones.
		plain, decErr := atrest.DecryptLine(line)
		if decErr != nil {
			// Tampered or undecryptable -- skip the row rather than
			// aborting the whole listing.
			continue
		}
		var e Entry
		if jerr := json.Unmarshal(plain, &e); jerr == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// Clear truncates the history file. Used by `srv history clear`.
func Clear() error {
	path := Path()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Truncate(path, 0)
}
