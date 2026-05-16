package syncx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CollectFiles mode=list with no files must error, the same way
// mode=glob without --include does. Regression: it used to fall
// through to an empty file list, which the sync handler then
// reported as a silent "(nothing to sync)" instead of a usage error.
func TestCollectFiles_ListWithoutFilesErrors(t *testing.T) {
	o := &Options{Mode: "list"}
	files, err := CollectFiles(o, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("CollectFiles(mode=list, no files) = (%v, nil); want error", files)
	}
	if !strings.Contains(err.Error(), "files") {
		t.Errorf("error %q should mention --files", err)
	}
}

// Relative `files` must resolve against `root`, not the process cwd
// (t.TempDir() is never the cwd). Regression for the silent-drop bug:
// `sync mode=list files=[rel] root=<dir>` used to Abs() each file
// against the cwd, judge it "outside root", skip it, and return
// "(nothing to sync)" with no error. Nested + absolute-in-root must
// also collect.
func TestCollectFiles_ListResolvesRelativeToRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"x.txt", "sub/c.txt"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte("hi"), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	abs := filepath.Join(root, "x.txt")
	o := &Options{Mode: "list", Files: []string{"x.txt", "sub/c.txt", abs}}
	files, err := CollectFiles(o, root, nil)
	if err != nil {
		t.Fatalf("CollectFiles err = %v; want nil", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["x.txt"] || !got["sub/c.txt"] {
		t.Fatalf("relative files not collected against root: got %v", files)
	}
}
