package completion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePathOutputDedupSortFilter(t *testing.T) {
	in := strings.Join([]string{
		"ls",
		"cat",
		"ls", // dup
		"",
		"   bash  ", // padded
		"my prog",   // contains space -> dropped
		"awk;rm -rf /",
		"htop",
	}, "\n")
	got := parsePathOutput(in)
	want := []string{"bash", "cat", "htop", "ls"}
	if !equalSlice(got, want) {
		t.Errorf("parsePathOutput: got %v want %v", got, want)
	}
}

func TestSanitizeProfile(t *testing.T) {
	cases := map[string]string{
		"prod":          "prod",
		"user@host":     "user_host",
		"":              "_",
		"weird/profile": "weird_profile",
		"a.b-c_d":       "a.b-c_d",
	}
	for in, want := range cases {
		if got := sanitizeProfile(in); got != want {
			t.Errorf("sanitizeProfile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathCacheRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rp.txt")
	if err := writePathCache(path, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	got, ok := readPathCache(path)
	if !ok || !equalSlice(got, []string{"a", "b", "c"}) {
		t.Errorf("roundtrip: ok=%v got=%v", ok, got)
	}

	// Backdate mtime past the TTL so the reader treats it as stale.
	stale := time.Now().Add(-2 * remotePathTTL)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, ok := readPathCache(path); ok {
		t.Errorf("expected stale cache to be rejected")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
