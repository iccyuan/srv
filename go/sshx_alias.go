package main

import "srv/internal/sshx"

// SSH client moved to srv/internal/sshx. Type and var aliases keep
// every existing call site in package main (ops.go, mcp_tools.go,
// daemon family, tunnel, check, edit, group, jobs, sync, streams,
// completion_remote, ui, ...) compiling unchanged while the source
// of truth lives in the new package. Feature packages already
// extracted (jobs, project, install, completion, progress) reach
// for the qualified `sshx.X` directly.

type (
	Client           = sshx.Client
	DialOptions      = sshx.DialOptions
	RunCaptureResult = sshx.RunCaptureResult
	StreamChunkKind  = sshx.StreamChunkKind
)

const (
	StreamStdout = sshx.StreamStdout
	StreamStderr = sshx.StreamStderr
)

var (
	Dial     = sshx.Dial
	DialOpts = sshx.DialOpts
)
