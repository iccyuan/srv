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
