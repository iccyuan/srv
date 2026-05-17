package syncx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name           string
		local, remote  RemoteStat
		expectedAction byte
	}{
		{
			"new on remote",
			RemoteStat{Size: 100, MtimeUnix: 200},
			RemoteStat{Missing: true},
			'+',
		},
		{
			"identical size + mtime",
			RemoteStat{Size: 100, MtimeUnix: 200},
			RemoteStat{Size: 100, MtimeUnix: 200},
			'=',
		},
		{
			"identical size + mtime within 2s",
			RemoteStat{Size: 100, MtimeUnix: 200},
			RemoteStat{Size: 100, MtimeUnix: 201},
			'=',
		},
		{
			"same size, local newer",
			RemoteStat{Size: 100, MtimeUnix: 300},
			RemoteStat{Size: 100, MtimeUnix: 200},
			'>',
		},
		{
			"same size, local older (would clobber)",
			RemoteStat{Size: 100, MtimeUnix: 100},
			RemoteStat{Size: 100, MtimeUnix: 300},
			'<',
		},
		{
			"size differs, local newer",
			RemoteStat{Size: 500, MtimeUnix: 300},
			RemoteStat{Size: 100, MtimeUnix: 200},
			'>',
		},
	}
	for _, tc := range cases {
		got := classify("file", tc.local, tc.remote)
		if got.Action != tc.expectedAction {
			t.Errorf("%s: got %c; want %c", tc.name, got.Action, tc.expectedAction)
		}
	}
}

func TestBuildDiffOrderingAndDeletes(t *testing.T) {
	dir := t.TempDir()
	mkfile := func(name string, content []byte, mtime time.Time) {
		full := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if !mtime.IsZero() {
			_ = os.Chtimes(full, mtime, mtime)
		}
	}
	mkfile("a.txt", []byte("hello"), time.Unix(500, 0))
	mkfile("b.txt", []byte("world"), time.Unix(500, 0))

	stats := map[string]RemoteStat{
		"a.txt": {Size: 5, MtimeUnix: 500}, // identical
		// b.txt missing -> new
	}
	got := BuildDiff(dir, []string{"a.txt", "b.txt"}, stats, []string{"old.txt"})
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].Path != "a.txt" || got[0].Action != '=' {
		t.Errorf("entry 0 unexpected: %+v", got[0])
	}
	if got[1].Path != "b.txt" || got[1].Action != '+' {
		t.Errorf("entry 1 unexpected: %+v", got[1])
	}
	if got[2].Path != "old.txt" || got[2].Action != '-' {
		t.Errorf("entry 2 (delete) unexpected: %+v", got[2])
	}
}

func TestPrintDiffSummary(t *testing.T) {
	entries := []DiffEntry{
		{Action: '+', Path: "new.txt", Local: RemoteStat{Size: 100}},
		{Action: '>', Path: "mod.txt", Local: RemoteStat{Size: 200}, Remote: RemoteStat{Size: 100}},
		{Action: '=', Path: "same.txt"},
		{Action: '-', Path: "del.txt"},
	}
	out := PrintDiff(entries, false)
	if strings.Contains(out, "same.txt") {
		t.Errorf("non-verbose mode should hide '=' rows: %s", out)
	}
	if !strings.Contains(out, "1 new, 1 modified, 0 local-older, 1 unchanged, 1 to delete") {
		t.Errorf("summary mismatch: %s", out)
	}

	verbose := PrintDiff(entries, true)
	if !strings.Contains(verbose, "same.txt") {
		t.Errorf("verbose mode should show '=' rows: %s", verbose)
	}
}
