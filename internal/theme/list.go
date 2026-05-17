package theme

import (
	"os"
	"path/filepath"
	"strings"

	"srv/internal/srvutil"
)

// ListPresets enumerates user-supplied theme files in
// srvutil.ColorPresetsDir() and returns their basenames (extension
// stripped). Accepts .sh / .itermcolors / .toml. When several files
// share a basename, only one entry appears -- LoadPresetBody
// resolves the precedence at use time.
//
// Returns nil + nil when the dir doesn't exist; the directory is
// created on demand by the user. Lives next to color.go because that
// is the only caller; previously it shared a file with the session
// helpers, which had nothing to do with theme assets.
func ListPresets() ([]string, error) {
	dir := srvutil.ColorPresetsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	accepted := map[string]bool{}
	for _, ext := range SupportedExts {
		accepted[ext] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !accepted[ext] {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if base == "" || seen[base] {
			continue
		}
		out = append(out, base)
		seen[base] = true
	}
	return out, nil
}
