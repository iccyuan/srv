package diff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseRemoteTreeLines_GNUFormat covers the Linux/GNU shape:
// `find -printf '%P\t%s\t%T@\n'`. %T@ carries fractional seconds, and
// the relative path is bare (no "./") because %P is relative to the
// starting point.
func TestParseRemoteTreeLines_GNUFormat(t *testing.T) {
	in := "foo.txt\t123\t1700000000.123\nsub/bar.go\t4096\t1700000001\n"
	got := parseRemoteTreeLines(in)
	if got["foo.txt"].size != 123 || got["foo.txt"].mtime != 1700000000 {
		t.Errorf("foo.txt parsed wrong: %+v", got["foo.txt"])
	}
	if got["sub/bar.go"].size != 4096 || got["sub/bar.go"].mtime != 1700000001 {
		t.Errorf("sub/bar.go parsed wrong: %+v", got["sub/bar.go"])
	}
}

// TestParseRemoteTreeLines_BSDFormat covers the macOS shape:
// `find . ... -exec stat -f '%N\t%z\t%m' {} +`. %N echoes whatever
// the find passed in -- since we cd into the dir and walk `.`, every
// path is prefixed with "./" that we strip before keying.
func TestParseRemoteTreeLines_BSDFormat(t *testing.T) {
	in := "./foo.txt\t123\t1700000000\n./sub/bar.go\t4096\t1700000001\n"
	got := parseRemoteTreeLines(in)
	if _, ok := got["foo.txt"]; !ok {
		t.Errorf("BSD ./foo.txt not normalized to foo.txt; have: %v", keys(got))
	}
	if got["sub/bar.go"].size != 4096 {
		t.Errorf("sub/bar.go size: got %d want 4096", got["sub/bar.go"].size)
	}
}

// TestParseRemoteTreeLines_TolerantToJunk: blank lines, lines with
// fewer than 3 tab-fields, and lines with non-numeric size/mtime drop
// silently rather than aborting the whole listing. A flaky remote
// that prints one weird warning shouldn't blow up the whole diff.
func TestParseRemoteTreeLines_TolerantToJunk(t *testing.T) {
	in := "\n" + // blank
		"oneonly\n" + // missing fields
		"two\tfields\n" +
		"name\tnotanumber\t123\n" + // bad size
		"name\t123\tnotanumber\n" + // bad mtime
		"good.txt\t10\t1700000000\n"
	got := parseRemoteTreeLines(in)
	if len(got) != 1 {
		t.Errorf("expected 1 valid entry, got %d (%v)", len(got), keys(got))
	}
	if _, ok := got["good.txt"]; !ok {
		t.Errorf("good.txt missing; got %v", keys(got))
	}
}

// TestRenderTreeDiff_Categories asserts the action character for each
// case (only-local, only-remote, size-differs, mtime-newer on either
// side, identical). Same vocabulary as `srv sync --diff`.
func TestRenderTreeDiff_Categories(t *testing.T) {
	local := map[string]fileStat{
		"only-local.txt": {size: 10, mtime: 1700000000},
		"identical.txt":  {size: 100, mtime: 1700000000},
		"local-newer.go": {size: 200, mtime: 1700001000}, // local mtime > remote
		"diff-size.txt":  {size: 50, mtime: 1700000000},
	}
	remoteTree := map[string]fileStat{
		"only-remote.txt": {size: 20, mtime: 1700000000},
		"identical.txt":   {size: 100, mtime: 1700000000},
		"local-newer.go":  {size: 200, mtime: 1700000000}, // remote older
		"diff-size.txt":   {size: 60, mtime: 1700000000},  // same mtime, size differs
	}
	out := renderTreeDiff("/local", "/remote", local, remoteTree, true)
	mustContainTree(t, out, "+ only-remote.txt") // remote-only
	mustContainTree(t, out, "- only-local.txt")  // local-only
	mustContainTree(t, out, "= identical.txt")   // identical, -v rendered
	mustContainTree(t, out, "< local-newer.go")  // local newer
	mustContainTree(t, out, "~ diff-size.txt")   // sizes differ but mtimes match
	// Summary line tallies match.
	mustContainTree(t, out, "summary: +1  -1  ~2  =1")
}

// TestRenderTreeDiff_OmitsIdenticalWithoutVerbose: the default mode
// hides "=" rows -- otherwise a 10k-file tree with 5 actual diffs
// drowns in noise. Verbose flips them back on (covered by the test
// above).
func TestRenderTreeDiff_OmitsIdenticalWithoutVerbose(t *testing.T) {
	local := map[string]fileStat{"same.txt": {size: 1, mtime: 1700000000}}
	remoteTree := map[string]fileStat{"same.txt": {size: 1, mtime: 1700000000}}
	out := renderTreeDiff("/l", "/r", local, remoteTree, false)
	if strings.Contains(out, "= same.txt") {
		t.Errorf("identical row leaked into non-verbose output:\n%s", out)
	}
	// But the summary still tallies it.
	mustContainTree(t, out, "=1")
	// And the "no differences" branch fires since the rows slice is empty.
	mustContainTree(t, out, "(no differences)")
}

// TestRenderTreeDiff_MtimeWithinSlackIsIdentical: a 2-second slack on
// mtime mirrors syncx pull.go classifyPull -- different filesystems
// can round mtime to seconds vs nanos, and a 1-2 second drift after a
// fresh sync is normal. We must NOT classify those as "differs".
func TestRenderTreeDiff_MtimeWithinSlackIsIdentical(t *testing.T) {
	local := map[string]fileStat{"x": {size: 100, mtime: 1700000000}}
	remoteTree := map[string]fileStat{"x": {size: 100, mtime: 1700000002}} // +2s
	out := renderTreeDiff("/l", "/r", local, remoteTree, true)
	mustContainTree(t, out, "= x")
	if strings.Contains(out, "< x") || strings.Contains(out, "> x") || strings.Contains(out, "~ x") {
		t.Errorf("2s mtime drift should classify as identical:\n%s", out)
	}
}

func TestHumanizeBytes_Boundaries(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1024 * 1024, "1.0M"},
		{1024 * 1024 * 1024, "1.0G"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWalkLocalTree_Basic ensures the WalkDir helper produces
// POSIX-relative keys (so they compare equal to the remote walker's
// keys) and captures size+mtime accurately enough that an identical
// file shows up under the slack window. Hidden files are included --
// see the comment in walkLocalTree on why we don't skip them.
func TestWalkLocalTree_Basic(t *testing.T) {
	root := t.TempDir()
	must := func(name string, body []byte) {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("plain.txt", []byte("hello"))
	must("sub/deep.go", []byte("package main"))
	must(".hidden", []byte("h"))

	now := time.Now().Unix()
	got, err := walkLocalTree(root)
	if err != nil {
		t.Fatalf("walkLocalTree: %v", err)
	}
	for _, want := range []string{"plain.txt", "sub/deep.go", ".hidden"} {
		s, ok := got[want]
		if !ok {
			t.Errorf("missing %q; have %v", want, keys(got))
			continue
		}
		if s.size <= 0 {
			t.Errorf("%q has zero size: %+v", want, s)
		}
		if s.mtime < now-60 || s.mtime > now+60 {
			t.Errorf("%q mtime %d outside ±60s of now=%d", want, s.mtime, now)
		}
	}
	// POSIX slashes only -- no `sub\\deep.go` leakage on Windows.
	if _, bad := got["sub\\deep.go"]; bad {
		t.Errorf("Windows-style key leaked: %v", keys(got))
	}
}

func keys(m map[string]fileStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustContainTree(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output missing %q\n--- full ---\n%s", needle, haystack)
	}
}
