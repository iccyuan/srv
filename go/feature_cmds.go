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
	"sort"
	"strings"
)

func cmdDoctor(args []string, cfg *Config, profileOverride string) int {
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
		return 0
	}
	return 1
}

func doctorChecks(cfg *Config, profileOverride string) ([]map[string]any, bool) {
	ok := true
	rows := []map[string]any{}
	check := func(name string, pass bool, detail string) {
		rows = append(rows, map[string]any{"name": name, "ok": pass, "detail": detail})
		if !pass {
			ok = false
		}
	}
	check("version", true, Version)
	check("config", true, ConfigFile())
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
	if _, err := os.Stat(filepath.Join(ConfigDir(), "cache")); err == nil {
		check("completion cache", true, filepath.Join(ConfigDir(), "cache"))
	} else {
		check("completion cache", true, "will be created on demand")
	}
	if daemonPing() {
		check("daemon", true, "running")
	} else {
		check("daemon", true, "not running; will auto-spawn for hot paths")
	}
	if _, _, err := ResolveProfile(cfg, profileOverride); err != nil {
		check("active profile", false, err.Error())
	}
	return rows, ok
}

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

func cmdOpen(args []string, cfg *Config, profileOverride string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv open <remote_file>")
		return 2
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cwd := GetCwd(name, profile)
	remote := resolveRemotePath(args[0], cwd)
	tmpDir, err := os.MkdirTemp("", "srv-open-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return 1
	}
	local := filepath.Join(tmpDir, path.Base(strings.TrimRight(remote, "/")))
	if rc, err := pullPath(profile, remote, local, false); err != nil || rc != 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "srv open:", err)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "opened local copy:", local)
	if err := openLocal(local); err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return 1
	}
	return 0
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

func cmdCode(args []string, cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cwd := GetCwd(name, profile)
	target := cwd
	if len(args) > 0 {
		target = resolveRemotePath(args[0], cwd)
	}
	host := profile.Host
	if profile.User != "" {
		host = profile.User + "@" + profile.Host
	}
	uri := "vscode-remote://ssh-remote+" + host + target
	if code, err := exec.LookPath("code"); err == nil {
		return runLocal(code, "--folder-uri", uri)
	}
	fmt.Println("code --folder-uri", uri)
	return 0
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

func cmdDiff(args []string, cfg *Config, profileOverride string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv diff <local_file> [remote_file]")
		return 2
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
		fmt.Fprintln(os.Stderr, "srv diff:", err)
		return 1
	}
	fmt.Print(text)
	return rc
}

func diffLocalRemote(cfg *Config, profileOverride, local, remoteArg string) (string, int, error) {
	if _, err := os.Stat(local); err != nil {
		return "", 1, err
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return "", 1, err
	}
	if remoteArg == "" {
		remoteArg = local
	}
	remote := resolveRemotePath(remoteArg, GetCwd(name, profile))
	tmpDir, err := os.MkdirTemp("", "srv-diff-")
	if err != nil {
		return "", 1, err
	}
	defer os.RemoveAll(tmpDir)
	remoteLocal := filepath.Join(tmpDir, filepath.Base(local)+".remote")
	if rc, err := pullPath(profile, remote, remoteLocal, false); err != nil || rc != 0 {
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

func cmdDiffChanged(args []string, cfg *Config, profileOverride string) int {
	root := findGitRoot(mustCwd())
	if root == "" {
		fmt.Fprintln(os.Stderr, "srv diff --changed: not in a git repo")
		return 2
	}
	scope := "all"
	if len(args) > 0 {
		scope = strings.TrimPrefix(args[0], "--")
	}
	files, err := gitChangedFiles(root, scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv diff --changed:", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "(no changed files)")
		return 0
	}
	rc := 0
	for _, rel := range files {
		local := filepath.Join(root, filepath.FromSlash(rel))
		if st, err := os.Stat(local); err != nil || st.IsDir() {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n--- %s ---\n", rel)
		if drc := cmdDiff([]string{local, rel}, cfg, profileOverride); drc != 0 {
			rc = drc
		}
	}
	return rc
}

func simpleDiff(a, b string) int {
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if bytes.Equal(ab, bb) {
		return 0
	}
	return 1
}

func cmdEnv(args []string, cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "list":
		keys := make([]string, 0, len(profile.Env))
		for k := range profile.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, profile.Env[k])
		}
	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: srv env set <key> <value>")
			return 2
		}
		if profile.Env == nil {
			profile.Env = map[string]string{}
		}
		profile.Env[args[1]] = strings.Join(args[2:], " ")
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s.%s=%s\n", name, args[1], profile.Env[args[1]])
	case "unset":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: srv env unset <key>")
			return 2
		}
		delete(profile.Env, args[1])
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "clear":
		profile.Env = nil
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: srv env [list|set|unset|clear]")
		return 2
	}
	return 0
}

func applyRemoteEnv(profile *Profile, cmd string) string {
	if profile == nil || len(profile.Env) == 0 {
		return cmd
	}
	keys := make([]string, 0, len(profile.Env))
	for k := range profile.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		parts = append(parts, k+"="+shQuote(profile.Env[k]))
	}
	if len(parts) == 0 {
		return cmd
	}
	return strings.Join(parts, " ") + " " + cmd
}

// cmdGuard implements `srv guard [on|off|status]`. Toggles or shows the
// per-session high-risk-op confirmation guard; off by default. Honors
// the SRV_GUARD env override (which the `status` form prints alongside
// the session record's value so users can tell where a yes/no is coming
// from). Output is intentionally one-line so it pipes cleanly.
func cmdGuard(args []string) int {
	envHint := func() string {
		if v := os.Getenv("SRV_GUARD"); v != "" {
			return fmt.Sprintf("  [SRV_GUARD=%s]", v)
		}
		return ""
	}
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	switch action {
	case "on", "enable":
		sid, err := SetGuard(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard on:", err)
			return 1
		}
		fmt.Printf("guard: on  (session=%s)%s\n", sid, envHint())
		return 0
	case "off", "disable":
		sid, err := SetGuard(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard off:", err)
			return 1
		}
		fmt.Printf("guard: off (session=%s)%s\n", sid, envHint())
		return 0
	case "status", "":
		sid := SessionID()
		state := "off"
		if GuardOn() {
			state = "on"
		}
		fmt.Printf("guard: %s (session=%s)%s\n", state, sid, envHint())
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: srv guard [on|off|status]")
	return 2
}

// cmdColor implements `srv color [on|off|status]`. Toggles the
// per-session colour-output flag (off by default). When on, the MCP
// `run` tool prepends a shell prologue that makes colour-aware tools
// (ls, grep) emit ANSI escapes even though the SSH session lacks a TTY.
func cmdColor(args []string) int {
	envHint := func() string {
		if v := os.Getenv("SRV_COLOR"); v != "" {
			return fmt.Sprintf("  [SRV_COLOR=%s]", v)
		}
		return ""
	}
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	switch action {
	case "on", "enable":
		sid, err := SetColor(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "color on:", err)
			return 1
		}
		fmt.Printf("color: on  (session=%s)%s\n", sid, envHint())
		return 0
	case "off", "disable":
		sid, err := SetColor(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "color off:", err)
			return 1
		}
		fmt.Printf("color: off (session=%s)%s\n", sid, envHint())
		return 0
	case "status", "":
		sid := SessionID()
		state := "off"
		if ColorOn() {
			state = "on"
		}
		fmt.Printf("color: %s (session=%s)%s\n", state, sid, envHint())
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: srv color [on|off|status]")
	return 2
}
