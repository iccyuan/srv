package syncx

import (
	"os"
	"path/filepath"
	"strings"
)

// IgnoreFileName is the per-repo opt-in file: lines are gitignore-style
// patterns (comments + negation supported) merged into `srv sync`'s
// exclude list. Lives at the sync root, picked up automatically.
const IgnoreFileName = ".srvignore"

// LoadIgnoreFile reads `.srvignore` from root and returns the
// non-blank, non-comment patterns in file order. Missing file is not
// an error (returns nil). Order is preserved so negation (`!pattern`)
// can override earlier excludes the same way it does in gitignore.
func LoadIgnoreFile(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, IgnoreFileName))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}
