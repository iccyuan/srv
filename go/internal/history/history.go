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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"srv/internal/atrest"
	"srv/internal/srvio"
	"srv/internal/srvpath"
	"strings"
	"time"
)

// Path returns the on-disk history file. Honors $SRV_HOME via srvpath.
func Path() string { return filepath.Join(srvpath.Dir(), "history.jsonl") }

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
	release, _ := srvio.FileLock(path)
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

func maybeRotate(path string) {
	entries, err := readAll(path)
	if err != nil || len(entries) <= rotateThreshold {
		return
	}
	keep := entries[len(entries)-MaxEntries:]
	encrypt := atrest.Enabled()
	var buf strings.Builder
	for _, e := range keep {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		// Honour the current encryption setting on the freshly-written
		// rotated file. Mixing plaintext + ciphertext is fine for
		// reads (auto-detect), but rotating to plaintext when the
		// flag is on would leak just-archived history every time
		// the size threshold tripped.
		if encrypt {
			if enc, encErr := atrest.EncryptLine(b); encErr == nil {
				b = enc
			}
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	_ = srvio.WriteFileAtomic(path, []byte(buf.String()), 0o600)
}

// ReadAll loads every entry in chronological order. Used by the CLI
// `srv history` viewer and (eventually) the UI history panel.
func ReadAll() ([]Entry, error) {
	return readAll(Path())
}

func readAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var out []Entry
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			// atrest.DecryptLine returns the input unchanged when
			// the line isn't in our wrapped format, so old plaintext
			// rows continue to read fine alongside encrypted ones.
			plain, decErr := atrest.DecryptLine([]byte(trimmed))
			if decErr != nil {
				// Tampered or undecryptable -- skip the row rather
				// than aborting the whole listing.
				if err == io.EOF {
					break
				}
				continue
			}
			var e Entry
			if jerr := json.Unmarshal(plain, &e); jerr == nil {
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

// Clear truncates the history file. Used by `srv history clear`.
func Clear() error {
	path := Path()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Truncate(path, 0)
}
