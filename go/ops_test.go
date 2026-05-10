package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalHashFirstN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(p, []byte("abcdef"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Hash of first 3 bytes ("abc") -- compare against the std-lib
	// computed value so a regression in our impl is caught even if both
	// callsites happen to drift the same way.
	want := sha256.Sum256([]byte("abc"))
	wantHex := hex.EncodeToString(want[:])
	got, err := localHashFirstN(p, 3)
	if err != nil {
		t.Fatalf("localHashFirstN: %v", err)
	}
	if got != wantHex {
		t.Errorf("hash mismatch: got=%s want=%s", got, wantHex)
	}
}

func TestLocalHashFirstNZero(t *testing.T) {
	// n=0 short-circuits to the empty-string sha256 constant without
	// touching the filesystem -- file may not even exist.
	got, err := localHashFirstN("/nonexistent", 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != sha256EmptyHex {
		t.Errorf("zero-n: got=%s want=%s", got, sha256EmptyHex)
	}
}

func TestLocalHashFirstNShort(t *testing.T) {
	// File has 2 bytes; asking for 3 should surface the io.CopyN short
	// read so callers (samePrefix) know to fall back to fresh upload.
	dir := t.TempDir()
	p := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(p, []byte("ab"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := localHashFirstN(p, 3); err == nil {
		t.Fatal("expected short-read error, got nil")
	}
}
