package syncx

import (
	"reflect"
	"srv/internal/remote"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"2h", 2 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"90", 90 * time.Second, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseDuration(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseDuration(%q) expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDuration(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestGlobToRegex(t *testing.T) {
	// `**` matches any sequence including /; `*` matches any non-/ chars.
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "foo/bar.go", false}, // single * doesn't cross /
		{"**/*.go", "foo/bar.go", true},
		{"**/*.go", "foo/baz/bar.go", true},
		{"src/**/*.py", "src/a/b/c.py", true},
		{"src/**/*.py", "lib/a.py", false},
		{"foo?bar", "fooXbar", true},
		{"foo?bar", "foo/bar", false}, // ? doesn't match /
	}
	for _, tc := range cases {
		got := srvutil.RegexMatch(globToRegex(tc.pat), tc.path)
		if got != tc.want {
			t.Errorf("glob %q vs %q = %v; want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

func TestMatchesAnyExclude(t *testing.T) {
	excludes := []string{"node_modules", ".git", "*.pyc", "dist/"}
	cases := []struct {
		path string
		want bool
	}{
		{"src/main.py", false},
		{"node_modules/foo", true}, // top-level dir match
		{"a/node_modules/b", true}, // component match deep in tree
		{".git/config", true},
		{"src/main.pyc", true}, // glob match
		{"dist/index.html", true},
		{"distinct/x.html", false}, // dist/ shouldn't match distinct/
	}
	for _, tc := range cases {
		got := matchesAnyExclude(tc.path, excludes)
		if got != tc.want {
			t.Errorf("matchesAnyExclude(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

func TestSplitRemotePrefix(t *testing.T) {
	cases := []struct {
		in                string
		wantDir, wantBase string
	}{
		{"", "", ""},
		{"foo", "", "foo"},
		{"/", "/", ""},
		{"/opt/", "/opt/", ""},
		{"/opt/al", "/opt/", "al"},
		{"~/", "~/", ""},
		{"~/foo", "~/", "foo"},
	}
	for _, tc := range cases {
		d, b := sshx.SplitRemotePrefix(tc.in)
		if d != tc.wantDir || b != tc.wantBase {
			t.Errorf("sshx.SplitRemotePrefix(%q) = (%q, %q); want (%q, %q)",
				tc.in, d, b, tc.wantDir, tc.wantBase)
		}
	}
}

func TestResolveRemotePath(t *testing.T) {
	cases := []struct {
		remote, cwd, want string
	}{
		{"", "/opt", "/opt"},
		{"/abs", "/opt", "/abs"},   // absolute pass-through
		{"~/foo", "/opt", "~/foo"}, // ~ pass-through
		{"file.txt", "/opt", "/opt/file.txt"},
		{"dir", "/opt/", "/opt/dir"}, // trailing slash on cwd handled
	}
	for _, tc := range cases {
		got := remote.ResolvePath(tc.remote, tc.cwd)
		if got != tc.want {
			t.Errorf("remote.ResolvePath(%q, %q) = %q; want %q",
				tc.remote, tc.cwd, got, tc.want)
		}
	}
}

func TestParseSyncOpts(t *testing.T) {
	o := ParseOptions([]string{"--staged", "--exclude", "*.log", "/opt/app"})
	if o.Mode != "git" || o.GitScope != "staged" {
		t.Errorf("--staged didn't set git/staged: %+v", o)
	}
	if !reflect.DeepEqual(o.Exclude, []string{"*.log"}) {
		t.Errorf("--exclude not captured: %v", o.Exclude)
	}
	if o.RemoteRoot != "/opt/app" {
		t.Errorf("remoteRoot not captured: %q", o.RemoteRoot)
	}

	o = ParseOptions([]string{"--include", "src/**/*.go", "--include=*.md"})
	if o.Mode != "glob" || len(o.Include) != 2 {
		t.Errorf("--include capture: %+v", o)
	}

	o = ParseOptions([]string{"--since", "2h"})
	if o.Mode != "mtime" || o.Since != "2h" {
		t.Errorf("--since: %+v", o)
	}

	o = ParseOptions([]string{"--", "a.go", "b.go"})
	if o.Mode != "list" || !reflect.DeepEqual(o.Files, []string{"a.go", "b.go"}) {
		t.Errorf("`--` files: %+v", o)
	}

	o = ParseOptions([]string{"--delete", "--yes", "--delete-limit", "50"})
	if !o.Delete || !o.Yes || o.DeleteLimit != 50 {
		t.Errorf("--delete safety options: %+v", o)
	}

	o = ParseOptions([]string{"--delete-limit=7"})
	if o.DeleteLimit != 7 {
		t.Errorf("--delete-limit= capture: %+v", o)
	}
}
