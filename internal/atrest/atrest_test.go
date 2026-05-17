package atrest

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// withSrvHome rewires SRV_HOME to a fresh temp directory and resets
// the sync.Once-cached key, so each test gets an independent keying
// state. Without the reset, tests that share the binary would observe
// the first test's key on every subsequent call.
func withSrvHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	keyOnce = sync.Once{}
	cachedKey = nil
	keyErr = nil
	return dir
}

func TestRoundtripEncryptDecrypt(t *testing.T) {
	withSrvHome(t)
	plain := []byte(`{"cmd":"ls","secret":"hunter2"}`)
	enc, err := EncryptLine(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(enc, []byte("hunter2")) {
		t.Errorf("ciphertext leaks the plaintext: %s", enc)
	}
	got, err := DecryptLine(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip mismatch: got %q want %q", got, plain)
	}
}

func TestDecryptLinePassesThroughPlaintext(t *testing.T) {
	withSrvHome(t)
	// Plain JSON is what existing pre-encryption files look like.
	// DecryptLine should return it verbatim, no error -- that's the
	// migration story: old rows keep being readable forever.
	in := []byte(`{"profile":"prod","cmd":"ls"}`)
	got, err := DecryptLine(in)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Errorf("expected plaintext passthrough; got %q want %q", got, in)
	}
}

func TestDecryptLineTamperingRejected(t *testing.T) {
	withSrvHome(t)
	enc, err := EncryptLine([]byte(`{"big":"enough to span past the GCM tag region"}`))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Tamper *inside* the decoded frame, not on the base64 text:
	// XOR'ing a base64 character can yield a non-alphabet byte
	// (A->@, z->{, +->*, ...). base64 decode then fails and
	// DecryptLine correctly falls back to plaintext passthrough --
	// which is NOT the GCM-tag path this test means to exercise, and
	// whether it happens is random per nonce. Decode, flip a byte in
	// the ciphertext region (past magic+nonce), and re-encode so we
	// deterministically reach aead.Open.
	frame, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	frame[len(magicBytes)+nonceLen] ^= 0x01
	tampered := []byte(base64.StdEncoding.EncodeToString(frame))
	if _, err := DecryptLine(tampered); err == nil {
		t.Error("expected GCM tag check to reject tampered ciphertext")
	}
}

func TestKeyPersistsAcrossSyncOnceReset(t *testing.T) {
	dir := withSrvHome(t)
	k1, err := Key()
	if err != nil {
		t.Fatalf("key #1: %v", err)
	}
	// Wipe the cache (simulating a new process) and re-read; the
	// returned key MUST match because it persisted to disk.
	keyOnce = sync.Once{}
	cachedKey = nil
	keyErr = nil
	k2, err := Key()
	if err != nil {
		t.Fatalf("key #2: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("expected same key from disk on second read")
	}
	// Sanity: file must exist at the standard path inside SRV_HOME.
	// Skip the perms assertion on Windows -- the Go runtime always
	// reports 0666 there regardless of how we opened the file, and
	// NTFS ACLs (which DO restrict access correctly) aren't visible
	// through os.FileMode.
	st, err := os.Stat(filepath.Join(dir, "secret", "key"))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Errorf("key file perms %o, want 0600", mode)
		}
	}
}

func TestEnabledHonoursEnv(t *testing.T) {
	t.Setenv("SRV_AT_REST_ENCRYPT", "")
	if Enabled() {
		t.Error("Enabled true with env unset")
	}
	t.Setenv("SRV_AT_REST_ENCRYPT", "0")
	if Enabled() {
		t.Error("Enabled true with env=0")
	}
	t.Setenv("SRV_AT_REST_ENCRYPT", "1")
	if !Enabled() {
		t.Error("Enabled false with env=1")
	}
}
