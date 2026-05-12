package main

import "srv/internal/group"

// Profile-groups feature moved to srv/internal/group. Aliases keep
// commands.go (CLI dispatcher), main.go (-G runGroup default path),
// and mcp_tools.go (handleMCPRunGroup) compiling unchanged.

type groupResult = group.Result

var (
	cmdGroup           = group.Cmd
	cmdRunGroup        = group.RunCmd
	runGroup           = group.Run
	renderGroupResults = group.RenderResults
	groupResultsJSON   = group.ResultsJSON
)
