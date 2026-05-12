package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/remote"
	"srv/internal/srvpath"
	"srv/internal/syncx"
	"srv/internal/transfer"
	"strings"
)

func cmdDoctor(args []string, cfg *config.Config, profileOverride string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	rows, ok := doctorChecks(cfg, profileOverride)
	for _, row := range rows {
		if asJSON {
			continue
		}
		pass, _ := row["ok"].(bool)
		name, _ := row["name"].(string)
		detail, _ := row["detail"].(string)
		mark := "OK"
		if !pass {
			mark = "FAIL"
		}
		if detail != "" {
			fmt.Printf("%-6s %-18s %s\n", mark, name, detail)
		} else {
			fmt.Printf("%-6s %s\n", mark, name)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"ok": ok, "checks": rows,
		}, "", "  ")
		fmt.Println(string(b))
	}
	if ok {
		return nil
	}
	return exitCode(1)
}

func doctorChecks(cfg *config.Config, profileOverride string) ([]map[string]any, bool) {
	ok := true
	rows := []map[string]any{}
	check := func(name string, pass bool, detail string) {
		rows = append(rows, map[string]any{"name": name, "ok": pass, "detail": detail})
		if !pass {
			ok = false
		}
	}
	check("version", true, Version)
	check("config", true, srvpath.Config())
	check("profiles", len(cfg.Profiles) > 0, fmt.Sprintf("%d configured", len(cfg.Profiles)))
	if cfg.DefaultProfile != "" {
		check("default profile", true, cfg.DefaultProfile)
	} else {
		check("default profile", false, "run `srv config use <name>`")
	}
	if _, err := exec.LookPath("git"); err == nil {
		check("git", true, "available")
	} else {
		check("git", false, "needed for git-mode sync")
	}
	if _, err := os.Stat(filepath.Join(srvpath.Dir(), "cache")); err == nil {
		check("completion cache", true, filepath.Join(srvpath.Dir(), "cache"))
	} else {
		check("completion cache", true, "will be created on demand")
	}
	if daemon.Ping() {
		check("daemon", true, "running")
	} else {
		check("daemon", true, "not running; will auto-spawn for hot paths")
	}
	if _, _, err := config.Resolve(cfg, profileOverride); err != nil {
		check("active profile", false, err.Error())
	}
	return rows, ok
}

// editJSONValue marshals v to a temp file, opens it in $EDITOR, and
// returns the (possibly modified) bytes after the editor exits. Used by
// `srv config edit` so users can hand-tweak a profile without escaping
// JSON on the command line.
func editJSONValue(v any, pattern string) ([]byte, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	editor, leadArgs, err := pickEditor()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(editor, append(leadArgs, tmpPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpPath)
}

func cmdOpen(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv open <remote_file>")
		return exitCode(2)
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
	}
	cwd := config.GetCwd(name, profile)
	remote := remote.ResolvePath(args[0], cwd)
	tmpDir, err := os.MkdirTemp("", "srv-open-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return exitCode(1)
	}
	local := filepath.Join(tmpDir, path.Base(strings.TrimRight(remote, "/")))
	if rc, _, err := transfer.PullPath(profile, remote, local, false); err != nil || rc != 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "srv open:", err)
		}
		return exitCode(1)
	}
	fmt.Fprintln(os.Stderr, "opened local copy:", local)
	if err := openLocal(local); err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return exitCode(1)
	}
	return nil
}

func openLocal(p string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", p).Start()
	case "darwin":
		return exec.Command("open", p).Start()
	default:
		return exec.Command("xdg-open", p).Start()
	}
}

func cmdCode(args []string, cfg *config.Config, profileOverride string) error {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
	}
	cwd := config.GetCwd(name, profile)
	target := cwd
	if len(args) > 0 {
		target = remote.ResolvePath(args[0], cwd)
	}
	host := profile.Host
	if profile.User != "" {
		host = profile.User + "@" + profile.Host
	}
	uri := "vscode-remote://ssh-remote+" + host + target
	if code, err := exec.LookPath("code"); err == nil {
		return exitCode(runLocal(code, "--folder-uri", uri))
	}
	fmt.Println("code --folder-uri", uri)
	return nil
}

func runLocal(name string, args ...string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdDiff(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return exitErr(2, "usage: srv diff <local_file> [remote_file]")
	}
	if args[0] == "--changed" {
		return cmdDiffChanged(args[1:], cfg, profileOverride)
	}
	local := args[0]
	remoteArg := args[0]
	if len(args) > 1 {
		remoteArg = args[1]
	}
	text, rc, err := diffLocalRemote(cfg, profileOverride, local, remoteArg)
	if err != nil {
		return exitErr(1, "srv diff: %v", err)
	}
	fmt.Print(text)
	return exitCode(rc)
}

func diffLocalRemote(cfg *config.Config, profileOverride, local, remoteArg string) (string, int, error) {
	if _, err := os.Stat(local); err != nil {
		return "", 1, err
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return "", 1, err
	}
	if remoteArg == "" {
		remoteArg = local
	}
	remote := remote.ResolvePath(remoteArg, config.GetCwd(name, profile))
	tmpDir, err := os.MkdirTemp("", "srv-diff-")
	if err != nil {
		return "", 1, err
	}
	defer os.RemoveAll(tmpDir)
	remoteLocal := filepath.Join(tmpDir, filepath.Base(local)+".remote")
	if rc, _, err := transfer.PullPath(profile, remote, remoteLocal, false); err != nil || rc != 0 {
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
	rc := simpleDiff(local, remoteLocal)
	if rc == 0 {
		return "", 0, nil
	}
	return fmt.Sprintf("files differ: %s %s\n", local, remote), rc, nil
}

func cmdDiffChanged(args []string, cfg *config.Config, profileOverride string) error {
	root := syncx.FindGitRoot(syncx.MustCwd())
	if root == "" {
		fmt.Fprintln(os.Stderr, "srv diff --changed: not in a git repo")
		return exitCode(2)
	}
	scope := "all"
	if len(args) > 0 {
		scope = strings.TrimPrefix(args[0], "--")
	}
	files, err := syncx.GitChangedFiles(root, scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv diff --changed:", err)
		return exitCode(1)
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
		if derr := cmdDiff([]string{local, rel}, cfg, profileOverride); derr != nil {
			lastErr = derr
		}
	}
	return lastErr
}

func simpleDiff(a, b string) int {
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if bytes.Equal(ab, bb) {
		return 0
	}
	return 1
}
