// Package atrest provides opt-in at-rest encryption for JSON-line
// state files srv keeps under ~/.srv/. The threat model is "casual
// snooping": someone with read access to your home dir (a backup
// snapshot you uploaded, a Dropbox-sync'd ~/.srv/, a `cat
// ~/.srv/history.jsonl` from another user on a misconfigured shared
// box) should not see the raw command history or MCP tool-call
// payloads. The model is NOT "motivated local attacker": the
// encryption key lives at ~/.srv/secret/key with 0600 perms, and
// anyone who can read that file alongside the data file can decrypt
// just as the daemon does. Bringing back a platform-keystore-backed
// keying would be a separate effort -- the keyring package was
// deliberately removed earlier.
//
// Activation is by env var: set SRV_AT_REST_ENCRYPT=1 before
// invoking any srv binary, and from that point new history /
// mcp-replay rows are encrypted. Reads auto-detect format so
// existing plaintext files keep working; the file is gradually
// migrated as new rows append. Disabling the env keeps reading the
// mixed file fine (encrypted rows still decode as long as the key
// is in place).
//
// Wire format per line: base64( magic[4] | nonce[12] | ciphertext ).
// magic = "srv1" identifies our encryption (4 ASCII bytes); JSON
// payloads never start with that, and base64 of a JSON object would
// start with "ey..." or "W..." so the prefix check is unambiguous.
package atrest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"srv/internal/platform"
	"srv/internal/srvutil"
	"sync"
)

const (
	envEnable  = "SRV_AT_REST_ENCRYPT"
	magicBytes = "srv1" // four ASCII bytes prefix our encrypted lines
	keyLen     = 32     // AES-256
	nonceLen   = 12     // GCM standard
)

var (
	keyOnce   sync.Once
	cachedKey []byte
	keyErr    error
)

// ResetForTest clears the sync.Once-cached key so a test that flips
// SRV_HOME mid-suite picks up the fresh directory's key file
// instead of the stale one from a previous test in the same binary.
// Production code MUST NOT call this; it's exported only because Go
// has no other way to expose state to a sibling package's tests.
func ResetForTest() {
	keyOnce = sync.Once{}
	cachedKey = nil
	keyErr = nil
}

// Enabled reports whether at-rest encryption is on for *writes*.
// Reads auto-detect regardless, so a user can flip this off
// temporarily and still cat their history (as long as the key file
// is intact). Inversely, a user can flip it on and the old plaintext
// lines stay readable.
func Enabled() bool {
	return os.Getenv(envEnable) == "1"
}

// Key returns the 32-byte master key, loading it from disk on first
// call (or creating it on first run). The key file lives at
// ~/.srv/secret/key with 0600 perms inside an 0700 directory. The
// loadOnce semantics + sync.Once mean re-reading the file is a
// single I/O cost per process even when many writers ask for the
// key.
//
// Returns an error when the key file is unreadable AND we can't
// create one (e.g. read-only ~/.srv). Callers must treat the absence
// of a key as "encryption unavailable" and fall back to plaintext
// writes; refusing to write at all would silently break history.
func Key() ([]byte, error) {
	keyOnce.Do(func() {
		cachedKey, keyErr = loadOrCreateKey()
	})
	return cachedKey, keyErr
}

func keyPath() string {
	return filepath.Join(srvutil.Dir(), "secret", "key")
}

func loadOrCreateKey() ([]byte, error) {
	path := keyPath()
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != keyLen {
			return nil, fmt.Errorf("atrest: key file %s has wrong length %d (want %d)", path, len(b), keyLen)
		}
		return b, nil
	}
	// Doesn't exist (or unreadable as ENOENT) -- create with crypto/rand.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("atrest: mkdir secret dir: %v", err)
	}
	buf := make([]byte, keyLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("atrest: rand: %v", err)
	}
	// Open with O_EXCL so two srv processes racing to create the
	// key file can't both end up writing different keys (one wins
	// the race; the loser re-reads).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// EEXIST means a peer raced us. Re-read whatever they wrote.
		if os.IsExist(err) {
			return os.ReadFile(path)
		}
		return nil, fmt.Errorf("atrest: create key: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		return nil, fmt.Errorf("atrest: write key: %v", err)
	}
	// Tighten the file ACL beyond what 0600 conveys on this platform.
	// On Unix that's a no-op (POSIX mode already covers it); on
	// Windows the platform impl applies an explicit DACL granting
	// only the current user's SID. Failure is non-fatal so a missing
	// privilege (e.g. a stripped-down service account) doesn't break
	// key creation entirely.
	if err := platform.Sec.HardenKeyFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "srv: atrest: harden key acl: %v\n", err)
	}
	return buf, nil
}

// EncryptLine wraps `plain` in AES-GCM with a fresh nonce and emits
// the base64-of-magic+nonce+ciphertext frame DecryptLine expects.
// Trailing newline is the caller's responsibility (each frame is a
// single JSONL line, so the writer typically appends '\n' after).
func EncryptLine(plain []byte) ([]byte, error) {
	key, err := Key()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plain, nil)
	// magic + nonce + ciphertext, then base64 it all so the resulting
	// line is a single printable token easy to grep through.
	frame := make([]byte, 0, len(magicBytes)+nonceLen+len(ct))
	frame = append(frame, magicBytes...)
	frame = append(frame, nonce...)
	frame = append(frame, ct...)
	encoded := base64.StdEncoding.EncodeToString(frame)
	return []byte(encoded), nil
}

// DecryptLine returns `line` as-is when it doesn't look like our
// frame (plaintext fallback for files that pre-date encryption or
// have been re-cleared). When the magic prefix matches, the GCM tag
// has to validate or the line is dropped via error -- a tampered
// row should NOT silently re-flatten to plaintext.
//
// The "plaintext passthrough" path is what makes auto-detection
// work: existing JSONL files keep being readable after the env is
// flipped on, and reads keep working after the env is flipped off
// (as long as the key still exists at its standard path).
func DecryptLine(line []byte) ([]byte, error) {
	// Try base64 first; if it doesn't decode, this isn't ours --
	// just hand back the original bytes (caller treats as plaintext).
	decoded, err := base64.StdEncoding.DecodeString(string(line))
	if err != nil {
		return line, nil
	}
	if len(decoded) < len(magicBytes)+nonceLen+16 {
		return line, nil
	}
	if string(decoded[:len(magicBytes)]) != magicBytes {
		return line, nil
	}
	key, err := Key()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := decoded[len(magicBytes) : len(magicBytes)+nonceLen]
	ct := decoded[len(magicBytes)+nonceLen:]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("atrest: gcm open failed (key changed or tampered)")
	}
	return plain, nil
}
