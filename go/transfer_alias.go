package main

import "srv/internal/transfer"

// SFTP push/pull moved to srv/internal/transfer. Aliases preserve
// the call sites in cmds.go (cmdPush / cmdPull), feature_cmds.go
// (cmdCode pull), mcp_tools.go (handleMCPPush / handleMCPPull).

var (
	pushPath = transfer.PushPath
	pullPath = transfer.PullPath
)
