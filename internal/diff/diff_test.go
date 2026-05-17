package diff

import (
	"path/filepath"
	"strings"
	"testing"

	"srv/internal/config"
)

// Compare must report a missing local file with a friendly,
// tool-consistent message instead of leaking the raw OS stat error
// (on Windows: "GetFileAttributesEx ...: The system cannot find the
// file specified."). Mirrors handlePush's "local path missing: ...".
// The os.Stat guard runs before config.Resolve, so a zero Config and
// no profile are fine -- it never reaches the network.
func TestCompare_MissingLocalIsFriendly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does_not_exist.txt")
	text, rc, err := Compare(&config.Config{}, "", missing, "")
	if err == nil {
		t.Fatalf("Compare(missing local) err = nil; want error")
	}
	if !strings.Contains(err.Error(), "local path missing") {
		t.Errorf("error %q should say 'local path missing'", err)
	}
	if rc != 1 || text != "" {
		t.Errorf("Compare(missing local) = (%q, %d); want (\"\", 1)", text, rc)
	}
}

// A directory `local` must be rejected up front. Regression: it fell
// through to a git-diff that joined the Windows temp path under the
// dir ("<dir>/C:\\Users\\..\\.remote") and failed with a baffling
// "Could not access" error (Windows-path-leak family). The IsDir
// guard runs before config.Resolve -- no network.
func TestCompare_DirectoryLocalRejected(t *testing.T) {
	dir := t.TempDir()
	text, rc, err := Compare(&config.Config{}, "", dir, "")
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Compare(dir local) err = %v; want an 'is a directory' error", err)
	}
	if rc != 1 || text != "" {
		t.Errorf("Compare(dir local) = (%q, %d); want (\"\", 1)", text, rc)
	}
}
