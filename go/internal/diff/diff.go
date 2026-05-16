// Package diff implements `srv diff` -- compare a local file against
// its remote counterpart. The remote side is pulled into a temp
// path; the comparison uses `git diff --no-index` when git is
// available (colorised, hunks) and falls back to a plain "equal /
// differ" byte compare otherwise.
//
// `--changed` walks the local git repo's modified set and diffs
// each entry sequentially -- same shape as `srv diff <local> <remote>`
// run per-file. Also reused by the MCP `diff` tool, which only
// needs the captured text + exit code.
package diff

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/syncx"
	"srv/internal/transfer"
	"strings"
)

// Cmd implements `srv diff <local> [remote]` and `srv diff --changed
// [scope]`.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return clierr.Errf(2, "usage: srv diff <local_file> [remote_file]")
	}
	if args[0] == "--changed" {
		return cmdChanged(args[1:], cfg, profileOverride)
	}
	local := args[0]
	remoteArg := args[0]
	if len(args) > 1 {
		remoteArg = args[1]
	}
	text, rc, err := Compare(cfg, profileOverride, local, remoteArg)
	if err != nil {
		return clierr.Errf(1, "srv diff: %v", err)
	}
	fmt.Print(text)
	return clierr.Code(rc)
}

// Compare runs the local-vs-remote diff and returns (text, exit
// code, error). Used by the CLI (Cmd) and by the MCP `diff` tool.
func Compare(cfg *config.Config, profileOverride, local, remoteArg string) (string, int, error) {
	if _, err := os.Stat(local); err != nil {
		if os.IsNotExist(err) {
			return "", 1, fmt.Errorf("local path missing: %q", local)
		}
		return "", 1, fmt.Errorf("local %q: %w", local, err)
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return "", 1, err
	}
	if remoteArg == "" {
		remoteArg = local
	}
	remotePath := remote.ResolvePath(remoteArg, config.GetCwd(name, profile))
	tmpDir, err := os.MkdirTemp("", "srv-diff-")
	if err != nil {
		return "", 1, err
	}
	defer os.RemoveAll(tmpDir)
	remoteLocal := filepath.Join(tmpDir, filepath.Base(local)+".remote")
	if rc, _, err := transfer.PullPath(profile, remotePath, remoteLocal, false); err != nil || rc != 0 {
		if err != nil {
			return "", 1, err
		}
		return "", rc, fmt.Errorf("pull failed")
	}
	if git, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command(git, "diff", "--no-index", "--", local, remoteLocal)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		rc := 0
		if err != nil {
			rc = 1
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			}
		}
		return out.String(), rc, nil
	}
	rc := byteDiff(local, remoteLocal)
	if rc == 0 {
		return "", 0, nil
	}
	return fmt.Sprintf("files differ: %s %s\n", local, remotePath), rc, nil
}

// cmdChanged walks the git-changed set under the cwd and runs Cmd
// for each entry, reusing the per-file diff output / exit code.
func cmdChanged(args []string, cfg *config.Config, profileOverride string) error {
	root := syncx.FindGitRoot(syncx.MustCwd())
	if root == "" {
		fmt.Fprintln(os.Stderr, "srv diff --changed: not in a git repo")
		return clierr.Code(2)
	}
	scope := "all"
	if len(args) > 0 {
		scope = strings.TrimPrefix(args[0], "--")
	}
	files, err := syncx.GitChangedFiles(root, scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv diff --changed:", err)
		return clierr.Code(1)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "(no changed files)")
		return nil
	}
	var lastErr error
	for _, rel := range files {
		local := filepath.Join(root, filepath.FromSlash(rel))
		if st, err := os.Stat(local); err != nil || st.IsDir() {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n--- %s ---\n", rel)
		if derr := Cmd([]string{local, rel}, cfg, profileOverride); derr != nil {
			lastErr = derr
		}
	}
	return lastErr
}

// byteDiff is the fallback comparison when git isn't on PATH: just
// reads both files and reports 0 (equal) / 1 (differ). Cheap, no
// hunks, but covers the "do these match at all" question.
func byteDiff(a, b string) int {
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if bytes.Equal(ab, bb) {
		return 0
	}
	return 1
}
