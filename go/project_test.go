package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// resetProjectCache wipes the per-process lookup cache so each test
// starts from a clean state. Plain map clears under the lock match the
// production access pattern.
func resetProjectCache() {
	projectCacheMu.Lock()
	defer projectCacheMu.Unlock()
	projectCache = map[string]*ProjectFile{}
	projectCacheNo = map[string]bool{}
}

func TestFindProjectFile_FoundAtCurrent(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	path := filepath.Join(dir, projectFileName)
	if err := os.WriteFile(path, []byte(`{"profile":"prod","cwd":"/srv/app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := findProjectFile(dir)
	if err != nil {
		t.Fatalf("findProjectFile: %v", err)
	}
	if pf == nil {
		t.Fatal("expected project file, got nil")
	}
	if pf.Profile != "prod" {
		t.Errorf("Profile=%q, want prod", pf.Profile)
	}
	if pf.Cwd != "/srv/app" {
		t.Errorf("Cwd=%q, want /srv/app", pf.Cwd)
	}
	if pf.Path != path {
		t.Errorf("Path=%q, want %q", pf.Path, path)
	}
}

func TestFindProjectFile_WalksUpFromSubdir(t *testing.T) {
	resetProjectCache()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, projectFileName), []byte(`{"profile":"staging"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	pf, err := findProjectFile(deep)
	if err != nil {
		t.Fatalf("findProjectFile: %v", err)
	}
	if pf == nil || pf.Profile != "staging" {
		t.Fatalf("expected staging pin from walk-up, got %+v", pf)
	}
}

func TestFindProjectFile_NestedDeepestWins(t *testing.T) {
	resetProjectCache()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, projectFileName), []byte(`{"profile":"outer"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(root, "sub")
	if err := os.MkdirAll(mid, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mid, projectFileName), []byte(`{"profile":"inner"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := findProjectFile(mid)
	if err != nil {
		t.Fatalf("findProjectFile: %v", err)
	}
	if pf == nil || pf.Profile != "inner" {
		t.Fatalf("nested deepest should win, got %+v", pf)
	}
}

func TestFindProjectFile_None(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	pf, err := findProjectFile(dir)
	if err != nil {
		t.Fatalf("findProjectFile: %v", err)
	}
	if pf != nil {
		t.Errorf("expected nil, got %+v", pf)
	}
}

func TestFindProjectFile_EmptyFileTreatedAsNone(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFileName), []byte("   \n\t  "), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := findProjectFile(dir)
	if err != nil {
		t.Fatalf("empty file should not error: %v", err)
	}
	if pf != nil {
		t.Errorf("empty file should yield nil, got %+v", pf)
	}
}

func TestFindProjectFile_MalformedSurfacesError(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFileName), []byte("{ this is not JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := findProjectFile(dir)
	if err == nil {
		t.Error("expected parse error, got nil")
	}
}

func TestFindProjectFile_CachesHits(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	path := filepath.Join(dir, projectFileName)
	if err := os.WriteFile(path, []byte(`{"profile":"a"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pf1, _ := findProjectFile(dir)
	// Mutate the file on disk; cached value should still be returned.
	if err := os.WriteFile(path, []byte(`{"profile":"b"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pf2, _ := findProjectFile(dir)
	if pf1 == nil || pf2 == nil {
		t.Fatal("nil project file from cache test")
	}
	if pf1.Profile != "a" || pf2.Profile != "a" {
		t.Errorf("cache miss: pf1=%q pf2=%q, want both 'a'", pf1.Profile, pf2.Profile)
	}
}

func TestFindProjectFile_CachesMisses(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	// First call -- no file. Should cache the miss.
	if pf, _ := findProjectFile(dir); pf != nil {
		t.Fatal("expected nil first call")
	}
	// Now create the file; the cached miss should keep us at nil.
	if err := os.WriteFile(filepath.Join(dir, projectFileName), []byte(`{"profile":"late"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if pf, _ := findProjectFile(dir); pf != nil {
		t.Errorf("expected nil from cached miss, got %+v", pf)
	}
}

func TestFindProjectFile_ConcurrentSafe(t *testing.T) {
	resetProjectCache()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, projectFileName), []byte(`{"profile":"p"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pf, err := findProjectFile(dir)
			if err != nil || pf == nil || pf.Profile != "p" {
				t.Errorf("race: %v %+v", err, pf)
			}
		}()
	}
	wg.Wait()
}
