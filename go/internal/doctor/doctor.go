// Package doctor implements `srv doctor` -- a quick local-health
// checklist: srv version, config path, profile count, default
// profile, git availability, completion cache, daemon liveness, and
// active-profile resolution.
//
// Reused by the MCP `doctor` tool. The list of probes is
// intentionally lightweight (~10 ms total): doctor should be safe
// to run repeatedly from a tight feedback loop.
package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvpath"
)

// Cmd implements `srv doctor [--json]`. Returns exit 1 when any
// probe failed.
//
// version is the build's main.Version; passed in because the
// version string lives in package main and we'd rather not pull a
// build-info import in here.
func Cmd(args []string, cfg *config.Config, profileOverride, version string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	rows, ok := Checks(cfg, profileOverride, version)
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
	return clierr.Code(1)
}

// Checks runs the doctor probe set and returns the structured rows
// (each row: name / ok / detail) plus an overall pass/fail. Same
// shape the MCP `doctor` tool returns to the model.
func Checks(cfg *config.Config, profileOverride, version string) ([]map[string]any, bool) {
	ok := true
	rows := []map[string]any{}
	check := func(name string, pass bool, detail string) {
		rows = append(rows, map[string]any{"name": name, "ok": pass, "detail": detail})
		if !pass {
			ok = false
		}
	}
	check("version", true, version)
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
