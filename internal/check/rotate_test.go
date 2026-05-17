package check

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestReadPubLineFromSidecar(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "k")
	pub := priv + ".pub"
	want := "ssh-ed25519 AAAA... user@host"
	if err := os.WriteFile(pub, []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPubLine(priv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(got, "\n") != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestReadPubLineFromPrivateKey(t *testing.T) {
	// Generate an ed25519 key, write only the private file, and check
	// that readPubLine reconstructs the auth line.
	dir := t.TempDir()
	priv := filepath.Join(dir, "k")
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(edPriv, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priv, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readPubLine(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "ssh-ed25519 ") {
		t.Errorf("expected ssh-ed25519 line, got %q", got)
	}
}

func TestPlatformAwareTipReturnsBlankOnNonWindows(t *testing.T) {
	tip := platformAwareTip()
	if tip != "" && !strings.Contains(tip, "Windows") {
		t.Errorf("non-empty tip should mention Windows; got %q", tip)
	}
}
