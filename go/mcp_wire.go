package main

import (
	"srv/internal/config"
	"srv/internal/mcp"
)

// mcpMode is set true while the process is acting as a stdio MCP
// server. Other code (fatal in main.go, anything that would render
// human-facing chrome to stderr) reads this to stay silent so it
// never leaks into the JSON-RPC response stream the model parses.
// Cannot be re-enabled from inside the MCP path -- the entire reason
// it exists.
//
// The actual silencing of i18n / project / progress lives inside
// internal/mcp.Run; this flag is just for top-level helpers like
// fatal() that can't import internal/mcp without a cycle.
var mcpMode bool

// mcpDeps adapts top-level main-package helpers (runCheck,
// checkAdvice, doctorChecks, diffLocalRemote, listRemoteEntries)
// into the small Deps struct internal/mcp's handlers expect. Keeping
// the indirection here means internal/mcp doesn't depend on main and
// the handlers themselves stay narrow.
func mcpDeps() mcp.Deps {
	return mcp.Deps{
		Check: func(prof *config.Profile) mcp.CheckResult {
			r := runCheck(prof)
			if r == nil {
				return mcp.CheckResult{}
			}
			return mcp.CheckResult{
				OK:        r.OK,
				Diagnosis: r.Diagnosis,
				Stderr:    r.Stderr,
				ExitCode:  r.ExitCode,
			}
		},
		CheckAdvice: checkAdvice,
		Doctor:      doctorChecks,
		Diff:        diffLocalRemote,
		ListEntries: listRemoteEntries,
	}
}
