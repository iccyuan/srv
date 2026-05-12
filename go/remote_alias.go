package main

import "srv/internal/remote"

// Remote-execution helpers moved to srv/internal/remote. Aliases
// keep the existing call sites in ops.go / env.go / tunnel /
// streams / sync / completion_remote / feature_cmds / mcp_tools
// compiling under the familiar names.

var (
	runRemoteStream   = remote.RunStream
	runRemoteCapture  = remote.RunCapture
	changeRemoteCwd   = remote.ChangeCwd
	validateRemoteCwd = remote.ValidateCwd
	resolveRemotePath = remote.ResolvePath
	applyRemoteEnv    = remote.ApplyEnv
)

var spawnDetached = remote.SpawnDetached
