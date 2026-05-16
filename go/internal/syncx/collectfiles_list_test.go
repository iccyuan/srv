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

// The guard must not false-trigger when files ARE supplied.
func TestCollectFiles_ListWithFilesOK(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	o := &Options{Mode: "list", Files: []string{"x.txt"}}
	if _, err := CollectFiles(o, root, nil); err != nil {
		t.Fatalf("CollectFiles(mode=list, files=[x.txt]) err = %v; want nil", err)
	}
}
