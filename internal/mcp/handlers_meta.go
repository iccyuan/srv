package mcp

import (
	"encoding/json"
	"fmt"
	"srv/internal/check"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/remote"
	"srv/internal/session"
	"strings"
	"time"
)

func handleCd(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	newCwd, err := remote.ChangeCwd(profName, prof, path)
	if err != nil {
		return textErr(err.Error())
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: newCwd}},
		StructuredContent: map[string]any{"cwd": newCwd, "profile": profName},
	}
}

func handlePwd(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := config.GetCwd(profName, prof)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: cwd}},
		StructuredContent: map[string]any{"cwd": cwd, "profile": profName},
	}
}

func handleUse(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	clear, _ := args["clear"].(bool)
	if clear {
		sid, _ := config.SetSessionProfile("")
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: unpinned", sid)}},
			StructuredContent: map[string]any{"session": sid, "profile": nil},
		}
	}
	target, _ := args["profile"].(string)
	if target == "" {
		sid, rec := session.Touch()
		info := map[string]any{
			"session": sid,
			"pinned":  nil,
			"default": cfg.DefaultProfile,
		}
		if rec.Profile != nil {
			info["pinned"] = *rec.Profile
		}
		b, _ := json.MarshalIndent(info, "", "  ")
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: string(b)}},
			StructuredContent: info,
		}
	}
	if _, ok := cfg.Profiles[target]; !ok {
		return textErr(fmt.Sprintf("profile %q not found", target))
	}
	sid, _ := config.SetSessionProfile(target)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: pinned to %q", sid, target)}},
		StructuredContent: map[string]any{"session": sid, "profile": target},
	}
}

func handleStatus(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	sid, rec := session.Touch()
	var pinned any
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	multiplex := prof.Multiplex == nil || *prof.Multiplex
	return jsonResult(map[string]any{
		"profile":       profName,
		"pinned":        pinned,
		"host":          prof.Host,
		"user":          prof.User,
		"port":          prof.GetPort(),
		"identity_file": prof.IdentityFile,
		"cwd":           config.GetCwd(profName, prof),
		"session":       sid,
		"multiplex":     multiplex,
		"compression":   prof.GetCompression(),
		"guard":         session.GuardOn(),
	})
}

func handleListProfiles(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	sid, rec := session.Touch()
	var pinned any
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	return jsonResult(map[string]any{
		"default":  cfg.DefaultProfile,
		"pinned":   pinned,
		"session":  sid,
		"profiles": names,
	})
}

func handleCheck(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	res := check.Run(prof)
	var advice []string
	if !res.OK {
		advice = check.Advice(res.Diagnosis, prof, profName)
	}
	info := map[string]any{
		"profile":   profName,
		"host":      prof.Host,
		"user":      prof.User,
		"port":      prof.GetPort(),
		"ok":        res.OK,
		"diagnosis": res.Diagnosis,
		"exit_code": res.ExitCode,
	}
	var text string
	if res.OK {
		target := prof.Host
		if prof.User != "" {
			target = prof.User + "@" + prof.Host
		}
		text = fmt.Sprintf("OK -- %s: %s key auth works.", profName, target)
	} else {
		text = fmt.Sprintf("FAIL (%s): %s\n\n%s", res.Diagnosis,
			strings.TrimSpace(res.Stderr), strings.Join(advice, "\n"))
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           !res.OK,
		StructuredContent: info,
	}
}

func handleDoctor(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	checks, ok := check.Checks(cfg, profileOverride, version)
	res := jsonResult(map[string]any{"ok": ok, "checks": checks})
	res.IsError = !ok
	return res
}

func handleDaemonStatus(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	conn := daemon.DialSock(time.Second)
	if conn == nil {
		return jsonResult(map[string]any{"running": false})
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "status"}, 2*time.Second)
	if err != nil || resp == nil {
		return textErr(fmt.Sprintf("daemon status failed: %v", err))
	}
	return jsonResult(map[string]any{
		"running":         true,
		"uptime_sec":      resp.Uptime,
		"profiles_pooled": resp.Profiles,
		"protocol":        resp.V,
	})
}

func handleEnv(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	action, _ := args["action"].(string)
	if action == "" {
		action = "list"
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	key, _ := args["key"].(string)
	value, _ := args["value"].(string)
	switch action {
	case "list":
	case "set":
		if key == "" {
			return textErr("key is required")
		}
		if prof.Env == nil {
			prof.Env = map[string]string{}
		}
		prof.Env[key] = value
		if err := config.Save(cfg); err != nil {
			return textErr(err.Error())
		}
	case "unset":
		if key == "" {
			return textErr("key is required")
		}
		delete(prof.Env, key)
		// Collapse an emptied map to nil so `unset` of the last key
		// matches `clear`'s representation (env: null, not env: {}).
		if len(prof.Env) == 0 {
			prof.Env = nil
		}
		if err := config.Save(cfg); err != nil {
			return textErr(err.Error())
		}
	case "clear":
		prof.Env = nil
		if err := config.Save(cfg); err != nil {
			return textErr(err.Error())
		}
	default:
		return textErr("unknown env action")
	}
	return jsonResult(map[string]any{"profile": profName, "env": prof.Env})
}
