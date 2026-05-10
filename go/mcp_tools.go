package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MCP tool registry. ONE source of truth: mcpToolDefs() (advertised to
// the client via tools/list) and mcpHandleTool() (dispatch on
// tools/call) both read from `mcpTools`. Adding a tool means appending
// a single entry below; the def-list and the dispatch switch can never
// drift. Same OCP pattern as the CLI subcommand registry in commands.go.

// mcpRunTextMax caps the combined stdout+stderr the `run` tool returns
// to the MCP client. Beyond this, output is truncated with a marker
// pointing the caller at remote-side filtering.
//
// Rationale: the MCP client keeps every tool result in its conversation
// history, so a single `cat /var/log/...` or `journalctl -n 100000`
// permanently inflates the client's memory by the full payload. 64 KiB is
// enough for typical command output while drawing a hard line against
// runaway dumps.
const (
	mcpRunTextMax            = 64 * 1024
	mcpWaitJobDefaultSeconds = 8
	mcpWaitJobMaxSeconds     = 15
)

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolResult struct {
	Content           []toolContent `json:"content"`
	IsError           bool          `json:"isError,omitempty"`
	StructuredContent any           `json:"structuredContent,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// runRiskyTokens are substrings that flag a remote command as
// destructive enough to require confirm=true when the session guard is
// on. Matched case-insensitively against the full command string. The
// list is intentionally short and conservative -- false-positive on a
// real `rm -rf` is fine (the model can re-issue with confirm=true);
// false-negatives are the real concern, so we cover the canonical
// "oh no" set: recursive force delete, raw-disk writes, mkfs, system
// halt, SQL drops, and explicit truncates.
var runRiskyTokens = []string{
	"rm -rf", "rm -fr", "rm -rF", "rm -Rf", "rm -fR", "rm -RF", "rm -FR",
	"rm --recursive --force", "rm --force --recursive",
	"rm -r --force", "rm -f --recursive",
	"dd of=", "dd if=/dev/zero", "dd if=/dev/random", "dd if=/dev/urandom",
	"mkfs.", "mkfs ",
	"shutdown ", "shutdown\n", "shutdown;",
	" reboot", ";reboot", "&&reboot",
	" halt", "; halt", "&& halt",
	" poweroff", ";poweroff", "&&poweroff",
	"drop database", "drop table", "drop schema",
	"truncate table", "truncate -",
	":>/", ":> /",
	"chattr -i",
	"> /dev/sd", "> /dev/nvme", "> /dev/disk", "> /dev/hd",
}

// runRiskyMatch reports the first risky token contained in `command`,
// or "" if none match. Match is case-insensitive.
func runRiskyMatch(command string) string {
	if command == "" {
		return ""
	}
	lc := strings.ToLower(command)
	for _, t := range runRiskyTokens {
		if strings.Contains(lc, strings.ToLower(t)) {
			return t
		}
	}
	return ""
}

// mcpTextErr wraps a plain string as an isError tool result. Used by
// every handler for pre-flight validation failures (missing args,
// profile not found, etc.).
func mcpTextErr(s string) toolResult {
	return toolResult{
		IsError: true,
		Content: []toolContent{{Type: "text", Text: s}},
	}
}

// runLongWaitSleep matches `sleep N` or `sleep N.X` where N > 5. Doesn't
// catch `sleep 5m` / `sleep 1h` (rare in AI-generated commands; would
// match if we tried, with false-positive risk on `sleep ${VAR}`).
var runLongWaitSleep = regexp.MustCompile(`\bsleep\s+(\d+(?:\.\d+)?)\b`)

// runForeverPatterns are commands that don't terminate on their own.
// `tail -f`, `watch`, and `journalctl -f` are the canonical sins.
var runForeverPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\btail\s+(?:-[^|>;&\s]*)?(?:-f|--follow)\b`),
	regexp.MustCompile(`\bwatch\s+`),
	regexp.MustCompile(`\bjournalctl\s+(?:-[^|>;&\s]*)?(?:-f|--follow)\b`),
}

// runRejectSync inspects a command planned for synchronous execution and
// returns a non-empty hint if it would block the MCP turn for too long.
// AI clients reach for sleep+poll loops by reflex, but those tie up the
// MCP per-tool timeout and produce the "tools no longer available" red
// dot. We catch the patterns here and route the model toward
// background=true / wait_job. Empty return = command is fine to run sync.
func runRejectSync(cmd string) string {
	if m := runLongWaitSleep.FindStringSubmatch(cmd); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil && n > 5 {
			return fmt.Sprintf("contains `sleep %s` which would block %ss synchronously", m[1], m[1])
		}
	}
	for _, re := range runForeverPatterns {
		if loc := re.FindStringIndex(cmd); loc != nil {
			return fmt.Sprintf("contains a never-terminating pattern (%s)", cmd[loc[0]:loc[1]])
		}
	}
	return ""
}

// runRejectMessage builds the educational error returned when sync run
// hits a long-blocking pattern. Tells the model exactly what to swap to.
func runRejectMessage(cmd, why string) string {
	return fmt.Sprintf(
		"rejected: %s. Synchronous `run` is bound by the MCP per-tool timeout (default 60s); long blocks tank the connection.\n\nUse the background pattern instead:\n  run { command: %q, background: true }   -> returns job_id immediately\n  wait_job { id: <returned id> }           -> short polls (default 8s, cap 15s)\n\nFor commands that legitimately need their full output streamed back synchronously, restructure them to finish in <60s (e.g. cap with `head`/`timeout 30`).",
		why, cmd,
	)
}

func mcpDetachedResult(rec *JobRecord) toolResult {
	info := map[string]any{
		"job_id":    rec.ID,
		"status":    "running",
		"profile":   rec.Profile,
		"pid":       rec.Pid,
		"log":       rec.Log,
		"cwd":       rec.Cwd,
		"started":   rec.Started,
		"next_tool": "wait_job",
	}
	text := fmt.Sprintf(
		"started job %s pid=%d profile=%s\npoll with wait_job id=%s max_wait_seconds=%d",
		rec.ID, rec.Pid, rec.Profile, rec.ID, mcpWaitJobDefaultSeconds,
	)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: info,
	}
}

// mcpToolHandler is the uniform handler signature. The dispatcher
// extracts profileOverride from args once and passes it explicitly so
// each handler doesn't repeat the extraction.
type mcpToolHandler func(args map[string]any, cfg *Config, profileOverride string) toolResult

type mcpTool struct {
	def     toolDef
	handler mcpToolHandler
}

// strSchema builds a string-type JSON schema fragment. Empty desc maps
// to a bare {"type": "string"} -- shaving "description":"" off every
// passthrough field keeps the tools/list payload compact.
func strSchema(desc string) map[string]any {
	if desc == "" {
		return map[string]any{"type": "string"}
	}
	return map[string]any{"type": "string", "description": desc}
}

// boolSchema with default value.
func boolSchema(def bool, desc string) map[string]any {
	out := map[string]any{"type": "boolean", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// intSchema with default + description.
func intSchema(def int, desc string) map[string]any {
	out := map[string]any{"type": "integer", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

var mcpTools = []mcpTool{
	{
		def: toolDef{
			Name:        "run",
			Description: "Run a remote shell command. Synchronous by default (blocks until completion).\n\nREJECTED in synchronous mode (use background=true instead):\n  - `sleep N` where N > 5\n  - `tail -f`, `watch`, `journalctl -f` and similar never-terminating patterns\n\nFor anything expected to take more than ~10s (builds, installs, tests, big greps, sleep+poll loops), set background=true. The command starts as a detached job and returns a job_id immediately; then poll with wait_job in short (<=15s) chunks. Synchronous mode is bound by the client's per-tool timeout (Claude Code default 60s).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    strSchema("Remote shell command."),
					"profile":    strSchema(""),
					"background": boolSchema(false, "Start as a detached job and return immediately. Required for long commands and for any sleep/wait/follow pattern."),
					"confirm":    boolSchema(false, "Required when guard is on AND command hits a high-risk pattern (rm -rf, dd, mkfs, drop ...)."),
				},
				"required": []string{"command"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return mcpTextErr("error: command is required")
			}
			confirm, _ := args["confirm"].(bool)
			if GuardOn() && !confirm {
				if pat := runRiskyMatch(cmd); pat != "" {
					return guardBlocked("run",
						fmt.Sprintf("command contains a high-risk pattern %q", pat))
				}
			}
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			background, _ := args["background"].(bool)
			if background {
				rec, err := spawnDetached(profName, prof, cmd)
				if err != nil {
					return mcpTextErr(err.Error())
				}
				return mcpDetachedResult(rec)
			}
			// Hard-reject sync calls that would block the MCP turn for too
			// long. Description tells the model what to do instead; this
			// catches the case where it ignored that and went with the
			// reflex sleep+poll pattern anyway.
			if why := runRejectSync(cmd); why != "" {
				return mcpTextErr(runRejectMessage(cmd, why))
			}
			cwd := GetCwd(profName, prof)
			res, _ := runRemoteCapture(prof, cwd, cmd)
			text, truncatedBytes := buildMCPRunText(res, cwd)
			structured := map[string]any{
				"exit_code": res.ExitCode,
				"cwd":       cwd,
			}
			if truncatedBytes > 0 {
				structured["truncated_bytes"] = truncatedBytes
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           res.ExitCode != 0,
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "cd",
			Description: "Set remote cwd.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    strSchema("Path."),
					"profile": strSchema(""),
				},
				"required": []string{"path"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			path, _ := args["path"].(string)
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			newCwd, err := changeRemoteCwd(profName, prof, path)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: newCwd}},
				StructuredContent: map[string]any{"cwd": newCwd, "profile": profName},
			}
		},
	},
	{
		def: toolDef{
			Name:        "pwd",
			Description: "Get remote cwd.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			cwd := GetCwd(profName, prof)
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: cwd}},
				StructuredContent: map[string]any{"cwd": cwd, "profile": profName},
			}
		},
	},
	{
		def: toolDef{
			Name:        "use",
			Description: "Pin or clear profile.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"profile": strSchema(""),
					"clear":   boolSchema(false, ""),
				},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			clear, _ := args["clear"].(bool)
			if clear {
				sid, _ := SetSessionProfile("")
				return toolResult{
					Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: unpinned", sid)}},
					StructuredContent: map[string]any{"session": sid, "profile": nil},
				}
			}
			target, _ := args["profile"].(string)
			if target == "" {
				sid, rec := TouchSession()
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
				return mcpTextErr(fmt.Sprintf("profile %q not found", target))
			}
			sid, _ := SetSessionProfile(target)
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: pinned to %q", sid, target)}},
				StructuredContent: map[string]any{"session": sid, "profile": target},
			}
		},
	},
	{
		def: toolDef{
			Name:        "status",
			Description: "Show active profile.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			sid, rec := TouchSession()
			var pinned any
			if rec.Profile != nil {
				pinned = *rec.Profile
			}
			multiplex := prof.Multiplex == nil || *prof.Multiplex
			return mcpJSONResult(map[string]any{
				"profile":       profName,
				"pinned":        pinned,
				"host":          prof.Host,
				"user":          prof.User,
				"port":          prof.GetPort(),
				"identity_file": prof.IdentityFile,
				"cwd":           GetCwd(profName, prof),
				"session":       sid,
				"multiplex":     multiplex,
				"compression":   prof.GetCompression(),
				"guard":         GuardOn(),
			})
		},
	},
	{
		def: toolDef{
			Name:        "list_profiles",
			Description: "List profiles.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			sid, rec := TouchSession()
			var pinned any
			if rec.Profile != nil {
				pinned = *rec.Profile
			}
			names := make([]string, 0, len(cfg.Profiles))
			for n := range cfg.Profiles {
				names = append(names, n)
			}
			return mcpJSONResult(map[string]any{
				"default":  cfg.DefaultProfile,
				"pinned":   pinned,
				"session":  sid,
				"profiles": names,
			})
		},
	},
	{
		def: toolDef{
			Name:        "check",
			Description: "Probe SSH connectivity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			res := runCheck(prof)
			var advice []string
			if !res.OK {
				advice = checkAdvice(res.Diagnosis, prof, profName)
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
		},
	},
	{
		def: toolDef{
			Name:        "doctor",
			Description: "Run local diagnostics.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			checks, ok := doctorChecks(cfg, profileOverride)
			res := mcpJSONResult(map[string]any{"ok": ok, "checks": checks})
			res.IsError = !ok
			return res
		},
	},
	{
		def: toolDef{
			Name:        "daemon_status",
			Description: "Show daemon status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			conn := daemonDial(time.Second)
			if conn == nil {
				return mcpJSONResult(map[string]any{"running": false})
			}
			defer conn.Close()
			resp, err := daemonCall(conn, daemonRequest{Op: "status"}, 2*time.Second)
			if err != nil || resp == nil {
				return mcpTextErr(fmt.Sprintf("daemon status failed: %v", err))
			}
			return mcpJSONResult(map[string]any{
				"running":         true,
				"uptime_sec":      resp.Uptime,
				"profiles_pooled": resp.Profiles,
				"protocol":        resp.V,
			})
		},
	},
	{
		def: toolDef{
			Name:        "env",
			Description: "Manage remote env.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": []string{"list", "set", "unset", "clear"}, "default": "list"},
					"key":     strSchema("Env var name."),
					"value":   strSchema("Env var value."),
					"profile": strSchema(""),
				},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			action, _ := args["action"].(string)
			if action == "" {
				action = "list"
			}
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			key, _ := args["key"].(string)
			value, _ := args["value"].(string)
			switch action {
			case "list":
			case "set":
				if key == "" {
					return mcpTextErr("key is required")
				}
				if prof.Env == nil {
					prof.Env = map[string]string{}
				}
				prof.Env[key] = value
				if err := SaveConfig(cfg); err != nil {
					return mcpTextErr(err.Error())
				}
			case "unset":
				if key == "" {
					return mcpTextErr("key is required")
				}
				delete(prof.Env, key)
				if err := SaveConfig(cfg); err != nil {
					return mcpTextErr(err.Error())
				}
			case "clear":
				prof.Env = nil
				if err := SaveConfig(cfg); err != nil {
					return mcpTextErr(err.Error())
				}
			default:
				return mcpTextErr("unknown env action")
			}
			return mcpJSONResult(map[string]any{"profile": profName, "env": prof.Env})
		},
	},
	{
		def: toolDef{
			Name:        "diff",
			Description: "Diff local vs remote file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema("Local file."),
					"remote":  strSchema("Remote file."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			local, _ := args["local"].(string)
			if local == "" {
				return mcpTextErr("local is required")
			}
			remote, _ := args["remote"].(string)
			text, rc, err := diffLocalRemote(cfg, profileOverride, local, remote)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           rc != 0,
				StructuredContent: map[string]any{"exit_code": rc, "local": local, "remote": remote},
			}
		},
	},
	{
		def: toolDef{
			Name:        "push",
			Description: "Upload file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema(""),
					"remote":  strSchema("Remote path."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			local, _ := args["local"].(string)
			if local == "" {
				return mcpTextErr("local is required")
			}
			if _, err := os.Stat(local); err != nil {
				return mcpTextErr(fmt.Sprintf("local path missing: %q", local))
			}
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			cwd := GetCwd(profName, prof)
			remote, _ := args["remote"].(string)
			if remote == "" {
				remote = filepath.Base(local)
			}
			abs := resolveRemotePath(remote, cwd)
			st, _ := os.Stat(local)
			recursive := false
			if rb, ok := args["recursive"].(bool); ok {
				recursive = rb
			}
			if st != nil && st.IsDir() {
				recursive = true
			}
			start := time.Now()
			rc, finalRemote, perr := pushPath(prof, local, abs, recursive)
			duration := time.Since(start)
			var bytes int64
			if rc == 0 {
				bytes = sumLocalSize(local)
			}
			var text string
			if rc == 0 {
				text = fmt.Sprintf("uploaded %s -> %s [exit 0]%s", local, finalRemote, fmtRate(bytes, duration))
			} else {
				text = fmt.Sprintf("upload FAILED %s -> %s [exit %d]", local, finalRemote, rc)
				if perr != nil {
					text += ": " + perr.Error()
				}
			}
			structured := map[string]any{
				"exit_code":        rc,
				"remote":           finalRemote,
				"local":            local,
				"duration_seconds": duration.Seconds(),
			}
			if rc == 0 {
				structured["bytes_transferred"] = bytes
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           rc != 0,
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "pull",
			Description: "Download file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote":    strSchema(""),
					"local":     strSchema("Local path."),
					"recursive": boolSchema(false, ""),
					"profile":   strSchema(""),
				},
				"required": []string{"remote"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			remote, _ := args["remote"].(string)
			if remote == "" {
				return mcpTextErr("remote is required")
			}
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			cwd := GetCwd(profName, prof)
			local, _ := args["local"].(string)
			if local == "" {
				local = "."
			}
			abs := resolveRemotePath(remote, cwd)
			recursive := false
			if rb, ok := args["recursive"].(bool); ok {
				recursive = rb
			}
			start := time.Now()
			rc, finalLocal, perr := pullPath(prof, abs, local, recursive)
			duration := time.Since(start)
			var bytes int64
			if rc == 0 {
				bytes = sumLocalSize(finalLocal)
			}
			var text string
			if rc == 0 {
				text = fmt.Sprintf("downloaded %s -> %s [exit 0]%s", abs, finalLocal, fmtRate(bytes, duration))
			} else {
				text = fmt.Sprintf("download FAILED %s -> %s [exit %d]", abs, finalLocal, rc)
				if perr != nil {
					text += ": " + perr.Error()
				}
			}
			structured := map[string]any{
				"exit_code":        rc,
				"remote":           abs,
				"local":            finalLocal,
				"duration_seconds": duration.Seconds(),
			}
			if rc == 0 {
				structured["bytes_transferred"] = bytes
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           rc != 0,
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "sync",
			Description: "Sync local changes to remote.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote_root":  strSchema("Remote root."),
					"mode":         map[string]any{"type": "string", "enum": []string{"git", "mtime", "glob", "list"}},
					"git_scope":    map[string]any{"type": "string", "enum": []string{"all", "staged", "modified", "untracked"}, "default": "all"},
					"since":        strSchema("Duration, e.g. 2h."),
					"include":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"exclude":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"root":         strSchema(""),
					"dry_run":      boolSchema(false, ""),
					"delete":       boolSchema(false, ""),
					"yes":          boolSchema(false, ""),
					"delete_limit": intSchema(20, "Max deletes without yes=true."),
					"profile":      strSchema(""),
					"confirm":      boolSchema(false, "Required when guard is on AND delete=true (non-dry-run)."),
				},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			o := &syncOpts{gitScope: "all"}
			if v, ok := args["remote_root"].(string); ok {
				o.remoteRoot = v
			}
			if v, ok := args["mode"].(string); ok {
				o.mode = v
			}
			if v, ok := args["git_scope"].(string); ok {
				o.gitScope = v
			}
			if v, ok := args["since"].(string); ok {
				o.since = v
			}
			if v, ok := args["root"].(string); ok {
				o.root = v
			}
			if v, ok := args["dry_run"].(bool); ok {
				o.dryRun = v
			}
			if v, ok := args["delete"].(bool); ok {
				o.delete = v
			}
			if v, ok := args["yes"].(bool); ok {
				o.yes = v
			}
			if o.delete && !o.dryRun && GuardOn() {
				confirm, _ := args["confirm"].(bool)
				if !confirm {
					return guardBlocked("sync",
						"delete=true would remove remote files")
				}
			}
			if v, ok := args["delete_limit"].(float64); ok {
				o.deleteLimit = int(v)
			}
			if v, ok := args["include"].([]any); ok {
				for _, x := range v {
					if s, ok := x.(string); ok {
						o.include = append(o.include, s)
					}
				}
			}
			if v, ok := args["exclude"].([]any); ok {
				for _, x := range v {
					if s, ok := x.(string); ok {
						o.exclude = append(o.exclude, s)
					}
				}
			}
			if v, ok := args["files"].([]any); ok {
				for _, x := range v {
					if s, ok := x.(string); ok {
						o.files = append(o.files, s)
					}
				}
			}
			localRoot := o.root
			if localRoot == "" {
				localRoot = findGitRoot(mustCwd())
				if localRoot == "" {
					localRoot = mustCwd()
				}
			}
			if o.mode == "" {
				if findGitRoot(localRoot) != "" {
					o.mode = "git"
				} else if len(o.include) > 0 {
					o.mode = "glob"
				} else if o.since != "" {
					o.mode = "mtime"
				} else if len(o.files) > 0 {
					o.mode = "list"
				} else {
					return mcpTextErr("no mode resolved (not a git repo and no include/since/files)")
				}
			}
			cwd := GetCwd(profName, prof)
			remoteRoot := cwd
			if o.remoteRoot != "" {
				remoteRoot = resolveRemotePath(o.remoteRoot, cwd)
			} else if prof.SyncRoot != "" {
				remoteRoot = resolveRemotePath(prof.SyncRoot, cwd)
			}
			allExcludes := append([]string{}, o.exclude...)
			allExcludes = append(allExcludes, prof.SyncExclude...)
			allExcludes = append(allExcludes, defaultSyncExcludes...)
			files, err := collectSyncFiles(o, localRoot, allExcludes)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			deletes, err := collectSyncDeletes(o, localRoot, allExcludes)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			limit := o.deleteLimit
			if limit == 0 {
				limit = 20
			}
			if len(deletes) > limit && !o.dryRun && !o.yes {
				return mcpTextErr(fmt.Sprintf("sync delete would remove %d files (limit %d). Re-run dry_run=true, yes=true, or set delete_limit.", len(deletes), limit))
			}
			if len(files) == 0 && len(deletes) == 0 {
				return toolResult{
					Content:           []toolContent{{Type: "text", Text: "(nothing to sync)"}},
					StructuredContent: map[string]any{"files": []string{}, "deletes": deletes, "remote_root": remoteRoot, "exit_code": 0},
				}
			}
			if o.dryRun {
				text := fmt.Sprintf("would sync %d files to %s\n", len(files), remoteRoot)
				lim := len(files)
				if lim > 200 {
					lim = 200
				}
				text += strings.Join(files[:lim], "\n")
				if len(files) > 200 {
					text += fmt.Sprintf("\n... (%d more)", len(files)-200)
				}
				if len(deletes) > 0 {
					text += "\nwould delete:\n" + strings.Join(deletes, "\n")
				}
				return toolResult{
					Content: []toolContent{{Type: "text", Text: text}},
					StructuredContent: map[string]any{
						"files_count":   len(files),
						"deletes_count": len(deletes),
						"remote_root":   remoteRoot,
						"dry_run":       true,
					},
				}
			}
			rc := 0
			var terr error
			start := time.Now()
			if len(files) > 0 {
				rc, terr = tarUploadStream(prof, localRoot, files, remoteRoot)
			}
			if rc == 0 && len(deletes) > 0 {
				rc, terr = deleteRemoteFiles(prof, remoteRoot, deletes)
			}
			duration := time.Since(start)
			var bytes int64
			if rc == 0 {
				for _, f := range files {
					if st, err := os.Stat(filepath.Join(localRoot, f)); err == nil {
						bytes += st.Size()
					}
				}
			}
			var text string
			if rc == 0 {
				text = fmt.Sprintf("synced %d files to %s [exit 0]%s", len(files), remoteRoot, fmtRate(bytes, duration))
			} else {
				text = fmt.Sprintf("sync FAILED to %s [exit %d]; %d files were NOT transferred -- verify with `run \"ls -la %s\"` before assuming",
					remoteRoot, rc, len(files), remoteRoot)
			}
			if terr != nil {
				text += ": " + terr.Error()
			}
			structured := map[string]any{
				"files_count":      len(files),
				"deletes_count":    len(deletes),
				"remote_root":      remoteRoot,
				"exit_code":        rc,
				"duration_seconds": duration.Seconds(),
			}
			if rc == 0 {
				structured["bytes_transferred"] = bytes
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           rc != 0,
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "sync_delete_dry_run",
			Description: "Preview sync deletes.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"root": strSchema("Local root."), "remote_root": strSchema("Remote root."), "profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			root, _ := args["root"].(string)
			if root == "" {
				root = findGitRoot(mustCwd())
			}
			if root == "" {
				return mcpTextErr("not in a git repo")
			}
			root, _ = filepath.Abs(root)
			o := &syncOpts{mode: "git", gitScope: "all", delete: true, dryRun: true}
			allExcludes := append([]string{}, prof.SyncExclude...)
			allExcludes = append(allExcludes, defaultSyncExcludes...)
			deletes, err := collectSyncDeletes(o, root, allExcludes)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			cwd := GetCwd(profName, prof)
			remoteRoot := cwd
			if v, _ := args["remote_root"].(string); v != "" {
				remoteRoot = resolveRemotePath(v, cwd)
			} else if prof.SyncRoot != "" {
				remoteRoot = resolveRemotePath(prof.SyncRoot, cwd)
			}
			text := fmt.Sprintf("would delete %d files from %s\n%s", len(deletes), remoteRoot, strings.Join(deletes, "\n"))
			return toolResult{
				Content: []toolContent{{Type: "text", Text: text}},
				StructuredContent: map[string]any{
					"deletes_count": len(deletes),
					"remote_root":   remoteRoot,
					"dry_run":       true,
				},
			}
		},
	},
	{
		def: toolDef{
			Name:        "detach",
			Description: "Start a remote command in the background and return its job_id immediately (sub-second). Pair with `wait_job` to block on completion in bounded chunks -- the recommended pattern for any command expected to take more than ~30s. The wrapper writes the user command's exit code to ~/.srv-jobs/<id>.exit when it finishes, which `wait_job` polls without keeping an SSH session open the whole time.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": strSchema(""),
					"profile": strSchema(""),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"command"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			cmd, _ := args["command"].(string)
			if cmd == "" {
				return mcpTextErr("command is required")
			}
			confirm, _ := args["confirm"].(bool)
			if GuardOn() && !confirm {
				if pat := runRiskyMatch(cmd); pat != "" {
					return guardBlocked("detach",
						fmt.Sprintf("command contains a high-risk pattern %q", pat))
				}
			}
			profName, prof, err := ResolveProfile(cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			rec, err := spawnDetached(profName, prof, cmd)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			return mcpDetachedResult(rec)
		},
	},
	{
		def: toolDef{
			Name:        "list_jobs",
			Description: "List detached jobs.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			jobs := loadJobsFile().Jobs
			if profileOverride != "" {
				out := jobs[:0]
				for _, j := range jobs {
					if j.Profile == profileOverride {
						out = append(out, j)
					}
				}
				jobs = out
			}
			return mcpJSONResult(map[string]any{"jobs": jobs})
		},
	},
	{
		def: toolDef{
			Name:        "tail_log",
			Description: "Tail job log.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    strSchema(""),
					"lines": intSchema(200, ""),
				},
				"required": []string{"id"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			jid, _ := args["id"].(string)
			lines := 200
			if v, ok := args["lines"].(float64); ok {
				lines = int(v)
			}
			jobs := loadJobsFile()
			j := findJob(jobs, jid)
			if j == nil {
				return mcpTextErr(fmt.Sprintf("no such job %q", jid))
			}
			prof, ok := cfg.Profiles[j.Profile]
			if !ok {
				return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
			}
			res, _ := runRemoteCapture(prof, "", fmt.Sprintf("tail -n %d %s", lines, j.Log))
			text := res.Stdout
			if text == "" {
				text = res.Stderr
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           res.ExitCode != 0,
				StructuredContent: map[string]any{"job_id": j.ID, "exit_code": res.ExitCode},
			}
		},
	},
	{
		def: toolDef{
			Name:        "wait_job",
			Description: "Poll a detached job for completion, returning exit code + log tail when done. Designed to pair with `detach` or `run background=true`: long commands run in the background, and the model loops wait_job until status=completed. Defaults to short 8s polls and caps each call at 15s so Claude Code stays responsive. status=running means \"call wait_job again\"; status=completed means it's done and the local job record has been cleaned up.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":               strSchema("Job id from detach."),
					"max_wait_seconds": intSchema(mcpWaitJobDefaultSeconds, "Upper bound on this call's blocking time. Capped at 15 to keep the MCP UI responsive."),
					"tail_lines":       intSchema(50, "Lines of log to include in the response."),
				},
				"required": []string{"id"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			jid, _ := args["id"].(string)
			maxWait := mcpWaitJobDefaultSeconds
			if v, ok := args["max_wait_seconds"].(float64); ok && v > 0 {
				maxWait = int(v)
			}
			if maxWait > mcpWaitJobMaxSeconds {
				maxWait = mcpWaitJobMaxSeconds
			}
			tailLines := 50
			if v, ok := args["tail_lines"].(float64); ok && v > 0 {
				tailLines = int(v)
			}
			jobs := loadJobsFile()
			j := findJob(jobs, jid)
			if j == nil {
				return mcpTextErr(fmt.Sprintf("no such job %q", jid))
			}
			prof, ok := cfg.Profiles[j.Profile]
			if !ok {
				return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
			}
			// One remote round-trip drives the whole wait loop. Bash spins
			// for up to maxWait seconds, checking each second for either
			// the .exit marker (job finished, capture exit code) or the
			// PID being gone without an .exit (got killed externally).
			// Either resolution prints `STATUS=...` on the first line plus
			// the log tail; if maxWait elapses the same shape is returned
			// with STATUS=running so the model can loop.
			exitFile := fmt.Sprintf("~/.srv-jobs/%s.exit", j.ID)
			script := fmt.Sprintf(`for i in $(seq 1 %d); do
  if [ -f %s ]; then
    code=$(cat %s)
    printf 'STATUS=completed EXIT=%%s\n' "$code"
    tail -n %d %s
    exit 0
  fi
  if ! kill -0 %d 2>/dev/null; then
    echo STATUS=killed
    tail -n %d %s
    exit 0
  fi
  sleep 1
done
echo STATUS=running
tail -n %d %s
`, maxWait, exitFile, exitFile, tailLines, j.Log, j.Pid, tailLines, j.Log, tailLines, j.Log)
			start := time.Now()
			res, _ := runRemoteCapture(prof, "", script)
			waited := time.Since(start).Seconds()

			lines := strings.SplitN(res.Stdout, "\n", 2)
			statusLine := ""
			body := ""
			if len(lines) > 0 {
				statusLine = lines[0]
			}
			if len(lines) > 1 {
				body = lines[1]
			}
			status := "unknown"
			exitCode := -1
			if strings.HasPrefix(statusLine, "STATUS=completed") {
				status = "completed"
				if i := strings.Index(statusLine, "EXIT="); i >= 0 {
					if n, err := strconv.Atoi(strings.TrimSpace(statusLine[i+5:])); err == nil {
						exitCode = n
					}
				}
				// Job finished -- prune from local registry so list_jobs
				// doesn't keep advertising it. The .log / .exit files on
				// the remote stay; users can still tail historical logs
				// manually if they want.
				out := jobs.Jobs[:0]
				for _, x := range jobs.Jobs {
					if x.ID != j.ID {
						out = append(out, x)
					}
				}
				jobs.Jobs = out
				_ = saveJobsFile(jobs)
			} else if strings.HasPrefix(statusLine, "STATUS=killed") {
				status = "killed"
			} else if strings.HasPrefix(statusLine, "STATUS=running") {
				status = "running"
			}

			var hint string
			switch status {
			case "completed":
				hint = fmt.Sprintf("[%s exit=%d after %.1fs]", status, exitCode, waited)
			case "running":
				hint = fmt.Sprintf("[%s after %.1fs -- call wait_job again to keep waiting, or kill_job to stop]", status, waited)
			default:
				hint = fmt.Sprintf("[%s after %.1fs]", status, waited)
			}
			text := hint
			if body != "" {
				text += "\n" + body
			}
			structured := map[string]any{
				"job_id":         j.ID,
				"status":         status,
				"waited_seconds": waited,
			}
			if status == "completed" {
				structured["exit_code"] = exitCode
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           status == "killed" || (status == "completed" && exitCode != 0),
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "list_dir",
			Description: "List remote directory entries (subset of `ls -1Ap`). Use this instead of `run \"ls ...\"` for path discovery -- response is structured, ANSI-clean, and hits the warm daemon cache (sub-100ms on repeat). Pass an empty path for the active cwd. Dirs carry trailing '/'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      strSchema("Remote path prefix. Empty = current cwd. Trailing '/' = list that directory; no trailing '/' = match entries whose name starts with the basename. E.g., '/etc/' lists /etc; '/etc/host' returns host*, hostname, hosts, hosts.allow."),
					"dirs_only": boolSchema(false, "Filter to directories only (entries ending in '/')."),
					"limit":     intSchema(500, "Maximum entries returned. Anything beyond gets dropped; truncated_count surfaces the cut so you know to query a deeper prefix."),
					"profile":   strSchema(""),
				},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			path, _ := args["path"].(string)
			dirsOnly, _ := args["dirs_only"].(bool)
			limit := 500
			if v, ok := args["limit"].(float64); ok && v > 0 {
				limit = int(v)
			}
			entries, err := listRemoteEntries(path, cfg, profileOverride)
			if err != nil {
				return mcpTextErr(err.Error())
			}
			if dirsOnly {
				kept := entries[:0]
				for _, e := range entries {
					if strings.HasSuffix(e, "/") {
						kept = append(kept, e)
					}
				}
				entries = kept
			}
			truncated := 0
			if len(entries) > limit {
				truncated = len(entries) - limit
				entries = entries[:limit]
			}
			text := strings.Join(entries, "\n")
			if text != "" {
				text += "\n"
			}
			structured := map[string]any{
				"entries": entries,
				"count":   len(entries),
			}
			if truncated > 0 {
				structured["truncated_count"] = truncated
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				StructuredContent: structured,
			}
		},
	},
	{
		def: toolDef{
			Name:        "kill_job",
			Description: "Signal detached job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     strSchema(""),
					"signal": map[string]any{"type": "string", "default": "TERM"},
				},
				"required": []string{"id"},
			},
		},
		handler: func(args map[string]any, cfg *Config, profileOverride string) toolResult {
			jid, _ := args["id"].(string)
			sig, _ := args["signal"].(string)
			if sig == "" {
				sig = "TERM"
			}
			jobs := loadJobsFile()
			j := findJob(jobs, jid)
			if j == nil {
				return mcpTextErr(fmt.Sprintf("no such job %q", jid))
			}
			prof, ok := cfg.Profiles[j.Profile]
			if !ok {
				return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
			}
			cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid'", sig, j.Pid)
			res, _ := runRemoteCapture(prof, "", cmd)
			out := jobs.Jobs[:0]
			for _, x := range jobs.Jobs {
				if x.ID != j.ID {
					out = append(out, x)
				}
			}
			jobs.Jobs = out
			_ = saveJobsFile(jobs)
			text := strings.TrimSpace(res.Stdout)
			if text == "" {
				text = strings.TrimSpace(res.Stderr)
			}
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: text}},
				IsError:           res.ExitCode != 0,
				StructuredContent: map[string]any{"job_id": j.ID, "signal": sig, "exit_code": res.ExitCode},
			}
		},
	},
}

// mcpToolMap is built once from the registry so dispatch is O(1).
var mcpToolMap map[string]*mcpTool

func init() {
	mcpToolMap = make(map[string]*mcpTool, len(mcpTools))
	for i := range mcpTools {
		t := &mcpTools[i]
		mcpToolMap[t.def.Name] = t
	}
}

// mcpToolDefs returns the slice of toolDef advertised on tools/list.
// Derived from the registry so the def-list and the dispatcher cannot
// drift -- both come from the same source.
func mcpToolDefs() []toolDef {
	defs := make([]toolDef, 0, len(mcpTools))
	for i := range mcpTools {
		defs = append(defs, mcpTools[i].def)
	}
	return defs
}

// mcpHandleTool dispatches a tools/call request through the registry.
// Unknown names return a textual error -- spec doesn't require a more
// structured "tool not found" form for that case.
func mcpHandleTool(name string, args map[string]any, cfg *Config) toolResult {
	profileOverride, _ := args["profile"].(string)
	if t, ok := mcpToolMap[name]; ok {
		return t.handler(args, cfg, profileOverride)
	}
	return mcpTextErr(fmt.Sprintf("unknown tool %q", name))
}

// buildMCPRunText assembles the textual payload returned by the `run`
// tool, capping the combined stdout+stderr at mcpRunTextMax. Returns
// (text, truncatedBytes); truncatedBytes is 0 when the output fit.
func buildMCPRunText(res *RunCaptureResult, cwd string) (string, int) {
	text := res.Stdout
	if res.Stderr != "" {
		if text != "" {
			text += "\n--- stderr ---\n"
		}
		text += res.Stderr
	}
	truncated := 0
	if len(text) > mcpRunTextMax {
		truncated = len(text) - mcpRunTextMax
		text = text[:mcpRunTextMax] + fmt.Sprintf(
			"\n\n... [%d bytes truncated; pipe through head/tail/grep on the remote to slice the output] ...",
			truncated,
		)
	}
	text += fmt.Sprintf("\n[exit %d cwd %s]", res.ExitCode, cwd)
	return text, truncated
}
