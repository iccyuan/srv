package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const mcpProtocolVersion = "2024-11-05"

// mcpRunTextMax caps the combined stdout+stderr the `run` tool returns
// to the MCP client (Claude Code et al.). Beyond this, output is
// truncated with a marker pointing the caller at remote-side filtering.
//
// Rationale: the MCP client keeps every tool result in its conversation
// history, so a single `cat /var/log/...` or `journalctl -n 100000`
// permanently inflates the client's memory by the full payload. 64 KiB is
// enough for typical command output while drawing a hard line against
// runaway dumps.
const mcpRunTextMax = 64 * 1024

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

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

// guardBlocked builds a uniform error response when the session guard
// refuses an operation. The message tells the model both how to
// proceed (confirm=true) and how to disable the guard entirely (`srv
// guard off`) so it doesn't have to ask the user back-and-forth.
func guardBlocked(tool, reason string) toolResult {
	text := fmt.Sprintf(
		"guard: %s blocked. %s\nRe-run with confirm=true if intentional, or disable via `srv guard off` (or unset SRV_GUARD).",
		tool, reason,
	)
	return toolResult{
		IsError: true,
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"guard_blocked": true,
			"tool":          tool,
			"reason":        reason,
		},
	}
}

// mcpJSONResult returns a tool result whose Content is a *compact* JSON
// rendering of `info`, with no separate StructuredContent. Both fields
// reach the MCP client; duplicating the same JSON in pretty-printed text
// AND a structured payload doubled the tokens many tools were spending
// on every call. Compact text is enough -- the model parses it fine and
// pretty-printing was costing ~30% extra whitespace tokens on top.
func mcpJSONResult(info any) toolResult {
	b, _ := json.Marshal(info)
	return toolResult{Content: []toolContent{{Type: "text", Text: string(b)}}}
}

func mcpToolDefs() []toolDef {
	// Empty descriptions are dropped so the loaded-once tools/list response
	// doesn't carry `,"description":""` on every passthrough field.
	str := func(desc string) map[string]any {
		if desc == "" {
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": "string", "description": desc}
	}
	return []toolDef{
		{
			Name:        "run",
			Description: "Run remote shell command.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": str("Remote shell command."),
					"profile": str(""),
					"confirm": map[string]any{"type": "boolean", "default": false, "description": "Required when guard is on AND command hits a high-risk pattern (rm -rf, dd, mkfs, drop ...)."},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "cd",
			Description: "Set remote cwd.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    str("Path."),
					"profile": str(""),
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "pwd",
			Description: "Get remote cwd.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": str("")},
			},
		},
		{
			Name:        "use",
			Description: "Pin or clear profile.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"profile": str(""),
					"clear":   map[string]any{"type": "boolean", "default": false},
				},
			},
		},
		{
			Name:        "status",
			Description: "Show active profile.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": str("")},
			},
		},
		{
			Name:        "list_profiles",
			Description: "List profiles.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "check",
			Description: "Probe SSH connectivity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": str("")},
			},
		},
		{
			Name:        "doctor",
			Description: "Run local diagnostics.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": str("")},
			},
		},
		{
			Name:        "daemon_status",
			Description: "Show daemon status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "env",
			Description: "Manage remote env.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": []string{"list", "set", "unset", "clear"}, "default": "list"},
					"key":     str("Env var name."),
					"value":   str("Env var value."),
					"profile": str(""),
				},
			},
		},
		{
			Name:        "diff",
			Description: "Diff local vs remote file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   str("Local file."),
					"remote":  str("Remote file."),
					"profile": str(""),
				},
				"required": []string{"local"},
			},
		},
		{
			Name:        "push",
			Description: "Upload file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   str(""),
					"remote":  str("Remote path."),
					"profile": str(""),
				},
				"required": []string{"local"},
			},
		},
		{
			Name:        "pull",
			Description: "Download file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote":    str(""),
					"local":     str("Local path."),
					"recursive": map[string]any{"type": "boolean", "default": false},
					"profile":   str(""),
				},
				"required": []string{"remote"},
			},
		},
		{
			Name:        "sync",
			Description: "Sync local changes to remote.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote_root": str("Remote root."),
					"mode":        map[string]any{"type": "string", "enum": []string{"git", "mtime", "glob", "list"}},
					"git_scope":   map[string]any{"type": "string", "enum": []string{"all", "staged", "modified", "untracked"}, "default": "all"},
					"since":       str("Duration, e.g. 2h."),
					"include":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"exclude":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"root":        str(""),
					"dry_run":     map[string]any{"type": "boolean", "default": false},
					"delete":      map[string]any{"type": "boolean", "default": false},
					"yes":         map[string]any{"type": "boolean", "default": false},
					"delete_limit": map[string]any{
						"type":        "integer",
						"default":     20,
						"description": "Max deletes without yes=true.",
					},
					"profile": str(""),
					"confirm": map[string]any{"type": "boolean", "default": false, "description": "Required when guard is on AND delete=true (non-dry-run)."},
				},
			},
		},
		{
			Name:        "sync_delete_dry_run",
			Description: "Preview sync deletes.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"root": str("Local root."), "remote_root": str("Remote root."), "profile": str("")},
			},
		},
		{
			Name:        "detach",
			Description: "Start remote detached job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": str(""),
					"profile": str(""),
					"confirm": map[string]any{"type": "boolean", "default": false, "description": "Required when guard is on AND command hits a high-risk pattern."},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "list_jobs",
			Description: "List detached jobs.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": str("")},
			},
		},
		{
			Name:        "tail_log",
			Description: "Tail job log.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    str(""),
					"lines": map[string]any{"type": "integer", "default": 200},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "list_dir",
			Description: "List remote directory entries (subset of `ls -1Ap`). Use this instead of `run \"ls ...\"` for path discovery -- response is structured, ANSI-clean, and hits the warm daemon cache (sub-100ms on repeat). Pass an empty path for the active cwd. Dirs carry trailing '/'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      str("Remote path prefix. Empty = current cwd. Trailing '/' = list that directory; no trailing '/' = match entries whose name starts with the basename. E.g., '/etc/' lists /etc; '/etc/host' returns host*, hostname, hosts, hosts.allow."),
					"dirs_only": map[string]any{"type": "boolean", "default": false, "description": "Filter to directories only (entries ending in '/')."},
					"limit":     map[string]any{"type": "integer", "default": 500, "description": "Maximum entries returned. Anything beyond gets dropped; truncated_count surfaces the cut so you know to query a deeper prefix."},
					"profile":   str(""),
				},
			},
		},
		{
			Name:        "kill_job",
			Description: "Signal detached job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     str(""),
					"signal": map[string]any{"type": "string", "default": "TERM"},
				},
				"required": []string{"id"},
			},
		},
	}
}

func mcpHandleTool(name string, args map[string]any, cfg *Config) toolResult {
	profileOverride, _ := args["profile"].(string)

	textErr := func(s string) toolResult {
		return toolResult{
			IsError: true,
			Content: []toolContent{{Type: "text", Text: s}},
		}
	}

	switch name {
	case "run":
		cmd, _ := args["command"].(string)
		if cmd == "" {
			return textErr("error: command is required")
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
			return textErr(err.Error())
		}
		cwd := GetCwd(profName, prof)
		res, _ := runRemoteCapture(prof, cwd, cmd)
		text, truncatedBytes := buildMCPRunText(res, cwd)
		// Stdout/Stderr live in the text Content; don't duplicate them
		// into StructuredContent -- the MCP client (Claude Code) keeps
		// both in its conversation history, doubling memory for large
		// command outputs.
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

	case "cd":
		path, _ := args["path"].(string)
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
		}
		newCwd, err := changeRemoteCwd(profName, prof, path)
		if err != nil {
			return textErr(err.Error())
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: newCwd}},
			StructuredContent: map[string]any{"cwd": newCwd, "profile": profName},
		}

	case "pwd":
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
		}
		cwd := GetCwd(profName, prof)
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: cwd}},
			StructuredContent: map[string]any{"cwd": cwd, "profile": profName},
		}

	case "use":
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
			return textErr(fmt.Sprintf("profile %q not found", target))
		}
		sid, _ := SetSessionProfile(target)
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: pinned to %q", sid, target)}},
			StructuredContent: map[string]any{"session": sid, "profile": target},
		}

	case "status":
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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

	case "list_profiles":
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

	case "check":
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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

	case "doctor":
		checks, ok := doctorChecks(cfg, profileOverride)
		res := mcpJSONResult(map[string]any{"ok": ok, "checks": checks})
		res.IsError = !ok
		return res

	case "daemon_status":
		conn := daemonDial(time.Second)
		if conn == nil {
			return mcpJSONResult(map[string]any{"running": false})
		}
		defer conn.Close()
		resp, err := daemonCall(conn, daemonRequest{Op: "status"}, 2*time.Second)
		if err != nil || resp == nil {
			return textErr(fmt.Sprintf("daemon status failed: %v", err))
		}
		return mcpJSONResult(map[string]any{
			"running":         true,
			"uptime_sec":      resp.Uptime,
			"profiles_pooled": resp.Profiles,
			"protocol":        resp.V,
		})

	case "env":
		action, _ := args["action"].(string)
		if action == "" {
			action = "list"
		}
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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
			if err := SaveConfig(cfg); err != nil {
				return textErr(err.Error())
			}
		case "unset":
			if key == "" {
				return textErr("key is required")
			}
			delete(prof.Env, key)
			if err := SaveConfig(cfg); err != nil {
				return textErr(err.Error())
			}
		case "clear":
			prof.Env = nil
			if err := SaveConfig(cfg); err != nil {
				return textErr(err.Error())
			}
		default:
			return textErr("unknown env action")
		}
		return mcpJSONResult(map[string]any{"profile": profName, "env": prof.Env})

	case "diff":
		local, _ := args["local"].(string)
		if local == "" {
			return textErr("local is required")
		}
		remote, _ := args["remote"].(string)
		text, rc, err := diffLocalRemote(cfg, profileOverride, local, remote)
		if err != nil {
			return textErr(err.Error())
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           rc != 0,
			StructuredContent: map[string]any{"exit_code": rc, "local": local, "remote": remote},
		}

	case "push":
		local, _ := args["local"].(string)
		if local == "" {
			return textErr("local is required")
		}
		if _, err := os.Stat(local); err != nil {
			return textErr(fmt.Sprintf("local path missing: %q", local))
		}
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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
		rc, perr := pushPath(prof, local, abs, recursive)
		// Always surface the resolved absolute remote path. SFTP MkdirAll
		// will silently create missing parent dirs, so a typo / wrong
		// project root used to land in a phantom location with just an
		// "uploaded" confirmation -- caller had to inspect StructuredContent
		// to notice. Putting the path in the text makes the misroute
		// visible in conversation history at a glance.
		var text string
		if rc == 0 {
			text = fmt.Sprintf("uploaded %s -> %s [exit 0]", local, abs)
		} else {
			text = fmt.Sprintf("upload FAILED %s -> %s [exit %d]", local, abs, rc)
			if perr != nil {
				text += ": " + perr.Error()
			}
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           rc != 0,
			StructuredContent: map[string]any{"exit_code": rc, "remote": abs, "local": local},
		}

	case "pull":
		remote, _ := args["remote"].(string)
		if remote == "" {
			return textErr("remote is required")
		}
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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
		rc, perr := pullPath(prof, abs, local, recursive)
		// Surface both ends of the transfer so the caller can spot a
		// wrong-path source or destination without inspecting structured.
		var text string
		if rc == 0 {
			text = fmt.Sprintf("downloaded %s -> %s [exit 0]", abs, local)
		} else {
			text = fmt.Sprintf("download FAILED %s -> %s [exit %d]", abs, local, rc)
			if perr != nil {
				text += ": " + perr.Error()
			}
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           rc != 0,
			StructuredContent: map[string]any{"exit_code": rc, "remote": abs, "local": local},
		}

	case "sync":
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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
		// Block sync-with-delete under the guard unless confirmed. Done
		// here so we short-circuit before doing the file walk and remote
		// dial. Pure dry_run is allowed -- it doesn't mutate state.
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
				return textErr("no mode resolved (not a git repo and no include/since/files)")
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
			return textErr(err.Error())
		}
		deletes, err := collectSyncDeletes(o, localRoot, allExcludes)
		if err != nil {
			return textErr(err.Error())
		}
		limit := o.deleteLimit
		if limit == 0 {
			limit = 20
		}
		if len(deletes) > limit && !o.dryRun && !o.yes {
			return textErr(fmt.Sprintf("sync delete would remove %d files (limit %d). Re-run dry_run=true, yes=true, or set delete_limit.", len(deletes), limit))
		}
		if len(files) == 0 && len(deletes) == 0 {
			return toolResult{
				Content:           []toolContent{{Type: "text", Text: "(nothing to sync)"}},
				StructuredContent: map[string]any{"files": []string{}, "deletes": deletes, "remote_root": remoteRoot, "exit_code": 0},
			}
		}
		if o.dryRun {
			text := fmt.Sprintf("would sync %d files to %s\n", len(files), remoteRoot)
			limit := len(files)
			if limit > 200 {
				limit = 200
			}
			text += strings.Join(files[:limit], "\n")
			if len(files) > 200 {
				text += fmt.Sprintf("\n... (%d more)", len(files)-200)
			}
			if len(deletes) > 0 {
				text += "\nwould delete:\n" + strings.Join(deletes, "\n")
			}
			// Don't re-emit the full files list in structured -- text
			// already shows the first 200 with a "(N more)" marker, and a
			// 5000-file repo otherwise puts ~150KB of duplicated paths
			// into the MCP client's context. Counts are enough for the
			// model to decide whether to dig deeper via `srv sync` directly.
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
		if len(files) > 0 {
			rc, terr = tarUploadStream(prof, localRoot, files, remoteRoot)
		}
		if rc == 0 && len(deletes) > 0 {
			rc, terr = deleteRemoteFiles(prof, remoteRoot, deletes)
		}
		// Wording must distinguish success from failure -- the prior
		// "synced %d files" said the same thing on rc=255 (e.g. ssh
		// handshake timeout post-reconnect, files NOT transferred) as on
		// rc=0, leading callers to believe a transfer that never happened.
		// On failure, lead with FAILED and clarify the count is the
		// attempted set, not what landed.
		var text string
		if rc == 0 {
			text = fmt.Sprintf("synced %d files to %s [exit 0]", len(files), remoteRoot)
		} else {
			text = fmt.Sprintf("sync FAILED to %s [exit %d]; %d files were NOT transferred -- verify with `run \"ls -la %s\"` before assuming",
				remoteRoot, rc, len(files), remoteRoot)
		}
		if terr != nil {
			text += ": " + terr.Error()
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			IsError: rc != 0,
			StructuredContent: map[string]any{
				"files_count":   len(files),
				"deletes_count": len(deletes),
				"remote_root":   remoteRoot,
				"exit_code":     rc,
			},
		}

	case "sync_delete_dry_run":
		profName, prof, err := ResolveProfile(cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
		}
		root, _ := args["root"].(string)
		if root == "" {
			root = findGitRoot(mustCwd())
		}
		if root == "" {
			return textErr("not in a git repo")
		}
		root, _ = filepath.Abs(root)
		o := &syncOpts{mode: "git", gitScope: "all", delete: true, dryRun: true}
		allExcludes := append([]string{}, prof.SyncExclude...)
		allExcludes = append(allExcludes, defaultSyncExcludes...)
		deletes, err := collectSyncDeletes(o, root, allExcludes)
		if err != nil {
			return textErr(err.Error())
		}
		cwd := GetCwd(profName, prof)
		remoteRoot := cwd
		if v, _ := args["remote_root"].(string); v != "" {
			remoteRoot = resolveRemotePath(v, cwd)
		} else if prof.SyncRoot != "" {
			remoteRoot = resolveRemotePath(prof.SyncRoot, cwd)
		}
		text := fmt.Sprintf("would delete %d files from %s\n%s", len(deletes), remoteRoot, strings.Join(deletes, "\n"))
		// Text already contains the full deletes list -- don't double it
		// in structured. Count is enough to verify scope.
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			StructuredContent: map[string]any{
				"deletes_count": len(deletes),
				"remote_root":   remoteRoot,
				"dry_run":       true,
			},
		}

	case "detach":
		cmd, _ := args["command"].(string)
		if cmd == "" {
			return textErr("command is required")
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
			return textErr(err.Error())
		}
		rec, err := spawnDetached(profName, prof, cmd)
		if err != nil {
			return textErr(err.Error())
		}
		return mcpJSONResult(rec)

	case "list_jobs":
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

	case "tail_log":
		jid, _ := args["id"].(string)
		lines := 200
		if v, ok := args["lines"].(float64); ok {
			lines = int(v)
		}
		jobs := loadJobsFile()
		j := findJob(jobs, jid)
		if j == nil {
			return textErr(fmt.Sprintf("no such job %q", jid))
		}
		prof, ok := cfg.Profiles[j.Profile]
		if !ok {
			return textErr(fmt.Sprintf("profile %q not found", j.Profile))
		}
		res, _ := runRemoteCapture(prof, "", fmt.Sprintf("tail -n %d %s", lines, j.Log))
		text := res.Stdout
		if text == "" {
			text = res.Stderr
		}
		// The tail content lives in Content; don't re-emit it as
		// structured.tail. With lines=5000 the duplication doubles the
		// tokens this tool spends per call. The job record is already
		// available via list_jobs if the model needs metadata.
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           res.ExitCode != 0,
			StructuredContent: map[string]any{"job_id": j.ID, "exit_code": res.ExitCode},
		}

	case "list_dir":
		path, _ := args["path"].(string)
		dirsOnly, _ := args["dirs_only"].(bool)
		// JSON numbers come in as float64; treat <=0 / missing as the
		// default 500.
		limit := 500
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		entries, err := listRemoteEntries(path, cfg, profileOverride)
		if err != nil {
			return textErr(err.Error())
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

	case "kill_job":
		jid, _ := args["id"].(string)
		sig, _ := args["signal"].(string)
		if sig == "" {
			sig = "TERM"
		}
		jobs := loadJobsFile()
		j := findJob(jobs, jid)
		if j == nil {
			return textErr(fmt.Sprintf("no such job %q", jid))
		}
		prof, ok := cfg.Profiles[j.Profile]
		if !ok {
			return textErr(fmt.Sprintf("profile %q not found", j.Profile))
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
	}

	return textErr(fmt.Sprintf("unknown tool %q", name))
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

func mcpSend(obj any) {
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	defer func() { _ = recover() }() // BrokenPipe
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

func mcpResponse(id any, result any, errObj *jsonRPCError) jsonRPCResponse {
	r := jsonRPCResponse{JSONRPC: "2.0", ID: id}
	if errObj != nil {
		r.Error = errObj
	} else {
		r.Result = result
	}
	return r
}

// mcpMode is set true while the process is acting as a stdio MCP server.
// Other code (progress meters, prompts, anything that would render
// human-facing chrome to stderr) reads this to stay silent so it never
// leaks into the JSON-RPC response stream the model parses. Cannot be
// re-enabled from inside the MCP path -- the entire reason it exists.
var mcpMode bool

func cmdMcp(cfg *Config) int {
	mcpMode = true
	rd := bufio.NewReader(os.Stdin)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return 0
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
			mcpSend(mcpResponse(nil, nil, &jsonRPCError{
				Code:    -32700,
				Message: "parse error: " + jerr.Error(),
			}))
			continue
		}
		switch req.Method {
		case "initialize":
			mcpSend(mcpResponse(req.ID, map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "srv", "version": Version},
			}, nil))
		case "notifications/initialized":
			// no response for notifications
		case "ping":
			mcpSend(mcpResponse(req.ID, map[string]any{}, nil))
		case "tools/list":
			mcpSend(mcpResponse(req.ID, map[string]any{"tools": mcpToolDefs()}, nil))
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				mcpSend(mcpResponse(req.ID, nil, &jsonRPCError{
					Code:    -32602,
					Message: "invalid tools/call params: " + err.Error(),
				}))
				continue
			}
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			cfg2, _ := LoadConfig()
			if cfg2 == nil {
				cfg2 = newConfig()
			}
			res := safeMCPHandle(p.Name, args, cfg2)
			mcpSend(mcpResponse(req.ID, res, nil))
		default:
			if req.ID != nil {
				mcpSend(mcpResponse(req.ID, nil, &jsonRPCError{
					Code:    -32601,
					Message: "method not found: " + req.Method,
				}))
			}
		}
	}
}

func safeMCPHandle(name string, args map[string]any, cfg *Config) (res toolResult) {
	defer func() {
		if r := recover(); r != nil {
			res = toolResult{
				IsError: true,
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("panic: %v", r)}},
			}
		}
	}()
	return mcpHandleTool(name, args, cfg)
}
