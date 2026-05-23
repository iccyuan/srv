package remote

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"srv/internal/config"
)

// TestNormalizePlatform_KnownValues pins the case-insensitive,
// whitespace-tolerant mapping from raw strings (Profile.Platform
// override, or `uname -s` output) into the Platform enum. The
// passthrough cases for BSD-family kernels reduce to PlatformOther
// so downstream code only ever sees a small closed set.
func TestNormalizePlatform_KnownValues(t *testing.T) {
	cases := []struct {
		in   string
		want Platform
	}{
		{"linux", PlatformLinux},
		{"Linux", PlatformLinux},
		{"  LINUX\n", PlatformLinux},
		{"darwin", PlatformDarwin},
		{"Darwin\n", PlatformDarwin}, // real `uname -s` shape on macOS
		{"FreeBSD", PlatformOther},
		{"openbsd", PlatformOther},
		{"illumos", PlatformOther},
		{"unknown", PlatformUnknown},
		{"", ""},        // empty -> let caller decide
		{"NotAnOS", ""}, // also empty: caller's signal to re-probe / give up
		{"window", ""},  // typo for "windows"; remote is never windows over SSH anyway
	}
	for _, tc := range cases {
		if got := normalizePlatform(tc.in); got != tc.want {
			t.Errorf("normalizePlatform(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizePlatformProfileName guards against filesystem-hostile
// characters in profile names leaking into cache filenames. Profile
// names that already match [A-Za-z0-9._-]+ pass through; everything
// else (colons, slashes, spaces, Chinese characters) is replaced
// with `_` so the cache write can't escape ~/.srv/cache/.
func TestSanitizePlatformProfileName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"prod", "prod"},
		{"web-1", "web-1"},
		{"v1.0", "v1.0"},
		{"a_b", "a_b"},
		{"with space", "with_space"},
		{"path/escape", "path_escape"},
		{"..", ".."}, // dots themselves are safe; path resolution is the caller's problem
		{"品川", "__"}, // Chinese profile name -> 2 underscores (one per rune)
		{"", "_"},    // empty becomes "_" so we never write platform-.txt
	}
	for _, tc := range cases {
		if got := sanitizePlatformProfileName(tc.in); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGetPlatform_ExplicitOverrideWinsOverCache: when Profile.Platform
// is set, GetPlatform must NOT consult the cache, MUST NOT probe, and
// MUST return the override verbatim. This is the documented escape
// hatch and breaking it would silently override user intent.
func TestGetPlatform_ExplicitOverrideWinsOverCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	// Seed a "stale" cache that disagrees with the override -- if
	// GetPlatform consults the cache despite the override, we'd see
	// "linux" instead of "darwin".
	cachePath := platformCacheFile("p")
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	_ = os.WriteFile(cachePath, []byte("linux\n"), 0o644)

	prof := &config.Profile{Name: "p", Platform: "darwin"}
	if got := GetPlatform(prof); got != PlatformDarwin {
		t.Errorf("explicit darwin override returned %q; cache should be ignored", got)
	}
}

// TestGetPlatform_NilOrBlankNameUnknown: callers may hand in a
// half-constructed profile (config.Resolve hasn't been called, name
// not set). Returning Unknown is safer than writing platform-.txt
// (which would clash across all unnamed profiles) or panicking on
// the nil deref.
func TestGetPlatform_NilOrBlankNameUnknown(t *testing.T) {
	if got := GetPlatform(nil); got != PlatformUnknown {
		t.Errorf("nil profile = %q, want unknown", got)
	}
	if got := GetPlatform(&config.Profile{}); got != PlatformUnknown {
		t.Errorf("blank-name profile = %q, want unknown", got)
	}
}

// TestPlatformCache_FreshAndStale verifies the read path's TTL gate.
// A freshly written cache returns its value; an artificially aged
// one (mtime backdated past the TTL) reads as "not present" so the
// next GetPlatform call would re-probe rather than serve stale data.
func TestPlatformCache_FreshAndStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	path := platformCacheFile("freshprofile")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("darwin\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got, ok := readPlatformCache(path); !ok || got != PlatformDarwin {
		t.Errorf("fresh cache = (%q, %v), want (darwin, true)", got, ok)
	}
	// Backdate well past the TTL.
	old := time.Now().Add(-platformCacheTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got, ok := readPlatformCache(path); ok {
		t.Errorf("stale cache returned (%q, true), want (_, false)", got)
	}
}

// TestPlatformCache_CorruptedReadsAsMiss: a cache file containing
// noise (manually edited, partial write, etc) must be treated as
// "no info" so the next call re-probes -- pinning "unknown" for 24h
// because someone fat-fingered an edit would be a silent footgun.
func TestPlatformCache_CorruptedReadsAsMiss(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	path := platformCacheFile("corrupt")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("this is not a platform\n"), 0o644)
	if _, ok := readPlatformCache(path); ok {
		t.Error("corrupted cache should not be served")
	}
}
