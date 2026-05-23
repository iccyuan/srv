package mcp

import (
	"fmt"
	"strings"

	"srv/internal/config"
	"srv/internal/sshx"
	"srv/internal/sudo"
)

// handleSudo runs a privileged command on the remote using a
// password that MUST have been seeded into the daemon's in-memory
// cache from a TTY (`srv sudo --cache-ttl 15m true`). Two boundary
// rules carry the whole security argument; neither is configurable:
//
//   - Every call MUST go through elicitation. Unlike `run`'s guard
//     where the high-risk pattern can be `confirm=true`-bypassed at
//     the model's discretion, sudo asks a human EVERY time and a
//     client that doesn't support elicitation is hard-denied. A
//     `confirm` arg is intentionally not exposed -- there is no
//     model-side bypass for crossing the privilege boundary.
//   - The handler ONLY reads sudo.CacheGet; it NEVER writes to the
//     cache. A password can therefore only enter the cache via the
//     TTY-prompting `srv sudo` CLI. If the cache is empty / expired,
//     the response tells the caller to seed it from a terminal --
//     we do not invent a "paste your password into the chat" path.
//
// The full cmd flows through MCP replay (~/.srv/mcp-replay.jsonl)
// like every other tool call; pair with `srv mcp replay show <idx>`
// when auditing.
func handleSudo(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return textErr("error: command is required")
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}

	// Force-elicit BEFORE looking at the cache. We don't want a
	// "cache empty" failure to leak the fact that a particular
	// command was about to be tried; if the human says no, the
	// response is the same shape regardless of whether the cache
	// would have hit.
	allow, asked := elicitConfirm(fmt.Sprintf(
		"srv sudo: about to run a privileged command on profile %q.\n\n  sudo %s\n\nAllow it to run?",
		profName, cmd))
	if !asked {
		// Client cannot be asked -> hard deny. There is NO automatic
		// fallback; sudo via MCP requires a human-in-the-loop client.
		// A CLI / terminal user can always run `srv sudo ...`.
		r := guardBlocked("sudo", "MCP client does not support elicitation (the interactive Allow/Deny round-trip). sudo via MCP requires a client that does; run privileged commands from a terminal via `srv sudo ...` instead.")
		return r
	}
	if !allow {
		r := guardDenied("sudo", "human declined the privileged command via elicitation")
		return r
	}

	pw := sudo.CacheGet(profName)
	if pw == "" {
		// Structured cached:false so a client UI can render a guided
		// "seed the cache" hint; the text body is what the model sees
		// and explicitly does NOT instruct it to ask the user for a
		// password (which it would then potentially store / log).
		text := fmt.Sprintf(
			"sudo: no cached password for profile %q. "+
				"Seed it from a terminal first: `srv sudo --cache-ttl 15m -P %s true` "+
				"(or any harmless sudo command). The cache lives in the daemon's "+
				"in-memory store, never on disk; MCP only reads it.",
			profName, profName)
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			StructuredContent: map[string]any{
				"cached":  false,
				"profile": profName,
				"hint":    "srv sudo --cache-ttl 15m -P " + profName + " true",
			},
			IsError: true,
		}
	}

	cwd := config.GetCwd(profName, prof)
	c, err := sshx.Dial(prof)
	if err != nil {
		return textErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()
	// Mirror the CLI shape (sudo/sudo.go::runRemote): -S reads the
	// password from stdin; -p '' suppresses sudo's own prompt so the
	// captured stderr doesn't contain "[sudo] password for ...". The
	// trailing newline on the piped password is what makes sudo's
	// read() return.
	full := sshx.WrapWithCwd("sudo -S -p '' "+cmd, cwd)
	res, runErr := c.RunCaptureStdin(full, "", strings.NewReader(pw+"\n"))
	if runErr != nil {
		return textErr(fmt.Sprintf("sudo: %v", runErr))
	}
	text := buildRunText(res, cwd)
	if len(text) > ResultByteMax {
		return oversizeResult("sudo", len(text),
			"narrow the sudo command's output -- pipe through `head -n N` / `tail -n N` / `grep PATTERN`",
			map[string]any{"profile": profName, "exit_code": res.ExitCode})
	}
	return payloadResult(text, res.ExitCode != 0)
}
