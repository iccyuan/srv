package main

import (
	"fmt"
	"os"
	"strings"
)

// AI-agent CLI gate.
//
// The bare `srv` CLI executes remote commands through remote.RunStream
// with NONE of the token-economy / sync / destructive-command gates the
// MCP server enforces (see internal/mcp/gates.go). An AI coding agent
// that shells out to `srv ...` therefore bypasses every guard and can
// stream unbounded output straight into the model's context. The policy:
// an AI agent must drive srv through the MCP server (the run /
// run_group / push / ... tools), never the raw CLI.
//
// This only ever fires for an agent shelling out to `srv` directly. The
// MCP server is started via `srv mcp` (a non-remote subcommand, never
// blocked here) and from then on serves tools in-process through the
// mcp package -- it does not re-enter this CLI dispatcher for remote
// work, so MCP keeps working untouched.

// aiAgentEnvVars are environment variables an AI coding agent sets in
// the shell it spawns. Presence of any (non-empty) one means "this srv
// invocation originates from an agent's shell". Kept as a small list so
// new agents are a one-line addition.
var aiAgentEnvVars = []string{
	"CLAUDECODE",             // Claude Code sets this to "1"
	"CLAUDE_CODE_ENTRYPOINT", // Claude Code entrypoint marker
	"CODEX_MANAGED_BY_NPM",   // OpenAI Codex CLI npm launcher marker
	"CODEX_SANDBOX_NETWORK_DISABLED",
	"CODEX_THREAD_ID",
}

// remoteSubcommands is the set of subcommand PRIMARY names that open an
// SSH connection to do work, move/compare data, or stream remote output
// -- i.e. the "remote actions" an agent must route through MCP instead.
// Local-only commands (help/version/config/guard/sessions/jobs/...) and
// the MCP server entry itself are deliberately absent so they keep
// working from any shell. Aliases are normalised to the primary name by
// the caller, so only primaries are listed.
var remoteSubcommands = map[string]bool{
	"cd": true, "check": true, "shell": true,
	"push": true, "pull": true, "sync": true,
	"edit": true, "open": true, "code": true, "diff": true,
	"tunnel": true, "logs": true, "kill": true,
	"recipe": true, "sudo": true, "ui": true,
	"tail": true, "watch": true, "journal": true, "top": true,
	"run": true, // `exec` aliases to `run`; callers pass the primary
}

// aiAgentDetected reports whether srv is running inside an AI coding
// agent's shell, based on the agent env markers.
func aiAgentDetected() bool {
	for _, k := range aiAgentEnvVars {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// aiCLIAllowed is the documented escape hatch. SRV_ALLOW_AI_CLI set to a
// truthy value disables the block entirely -- for the rare case a human
// is working inside an agent terminal and really wants the raw CLI. A
// hard block with no override is too sharp an edge.
func aiCLIAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SRV_ALLOW_AI_CLI"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// blockAIRemote decides whether this invocation must be refused: a
// remote-touching subcommand, OR the implicit "srv <cmd>" remote-run
// fallthrough (known == false), invoked from an agent shell with the
// escape hatch off. Pure function of its inputs + env so it is unit
// testable.
func blockAIRemote(known, isRemote bool) bool {
	if aiCLIAllowed() || !aiAgentDetected() {
		return false
	}
	// !known is the implicit remote-execute path (`srv ls -la`,
	// `srv 'journalctl ...'`) -- always a remote action.
	return !known || isRemote
}

// aiBlockMessage is the educational refusal printed to stderr. Mirrors
// the MCP gate messages' tone: say what was refused and what to do.
func aiBlockMessage(sub string) string {
	return fmt.Sprintf(
		"srv: refusing remote action %q -- this shell is an AI coding agent.\n"+
			"AI clients must drive srv through the MCP server (the run / run_group / push /\n"+
			"pull / sync / tail / journal / ... tools), which enforces the token-economy and\n"+
			"destructive-command gates the bare CLI does not. Use the srv MCP tool instead.\n"+
			"Escape hatch (human in an agent terminal): set SRV_ALLOW_AI_CLI=1.\n",
		sub,
	)
}
