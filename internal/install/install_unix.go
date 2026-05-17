//go:build !windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// installAddToPath wires srv into the user's PATH on Unix.
//
// Strategy mirrors install.sh:
//   - If $HOME/.local/bin is already on PATH, drop a symlink there
//     (cleanest -- no rc-file edits, picks up future rebuilds).
//   - Otherwise append `export PATH=...` to the right rc file
//     (zshrc / bashrc / bash_profile-on-darwin / fallback .profile),
//     guarded by a marker comment so re-runs are no-ops and uninstall
//     can find the line to remove.
func installAddToPath(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	localBin := filepath.Join(home, ".local", "bin")
	if isOnPath(localBin) {
		if err := os.MkdirAll(localBin, 0o755); err != nil {
			return err
		}
		link := filepath.Join(localBin, "srv")
		_ = os.Remove(link)
		return os.Symlink(filepath.Join(dir, "srv"), link)
	}

	rc := pickRcFile(home)
	marker := fmt.Sprintf("# srv installer (manage with %s/install.sh)", dir)

	contents, err := os.ReadFile(rc)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(contents), marker) {
		return nil // already there
	}

	f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n%s\nexport PATH=\"$PATH:%s\"\n", marker, dir)
	return err
}

// installRemoveFromPath undoes installAddToPath. If a $HOME/.local/bin
// symlink points at our binary, remove it; if our marker line is in the
// shell rc file, drop the marker plus the next line (the export).
func installRemoveFromPath(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	removed := false

	link := filepath.Join(home, ".local", "bin", "srv")
	if dest, err := os.Readlink(link); err == nil {
		if dest == filepath.Join(dir, "srv") {
			if err := os.Remove(link); err == nil {
				removed = true
			}
		}
	}

	rc := pickRcFile(home)
	marker := fmt.Sprintf("# srv installer (manage with %s/install.sh)", dir)
	if data, err := os.ReadFile(rc); err == nil && strings.Contains(string(data), marker) {
		lines := strings.Split(string(data), "\n")
		out := make([]string, 0, len(lines))
		skip := 0
		for _, line := range lines {
			if skip > 0 {
				skip--
				continue
			}
			if line == marker {
				// also drop the next line (the export)
				skip = 1
				continue
			}
			out = append(out, line)
		}
		if err := os.WriteFile(rc, []byte(strings.Join(out, "\n")), 0o644); err == nil {
			removed = true
		}
	}

	if !removed {
		return fmt.Errorf("nothing to remove (no symlink, no marker in %s)", rc)
	}
	return nil
}

// pickRcFile picks the right shell rc file to edit based on $SHELL and
// platform. Falls back to ~/.profile when $SHELL is unrecognized.
func pickRcFile(home string) string {
	shell := os.Getenv("SHELL")
	switch filepath.Base(shell) {
	case "zsh":
		if z := os.Getenv("ZDOTDIR"); z != "" {
			return filepath.Join(z, ".zshrc")
		}
		return filepath.Join(home, ".zshrc")
	case "bash":
		if runtime.GOOS == "darwin" {
			bp := filepath.Join(home, ".bash_profile")
			if _, err := os.Stat(bp); err == nil {
				return bp
			}
		}
		return filepath.Join(home, ".bashrc")
	}
	return filepath.Join(home, ".profile")
}
