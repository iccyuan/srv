package main

import "srv/internal/syncx"

// Sync moved to srv/internal/sync. Aliases preserve the call sites
// in commands.go (CLI dispatcher) and mcp_tools.go (handleMCPSync
// reaches into the parser + helpers).

type syncOpts = syncx.Options

var (
	cmdSync            = syncx.Cmd
	parseSyncOpts      = syncx.ParseOptions
	collectSyncFiles   = syncx.CollectFiles
	collectSyncDeletes = syncx.CollectDeletes
	tarUploadStream    = syncx.TarUploadStream
	deleteRemoteFiles  = syncx.DeleteRemoteFiles
)
