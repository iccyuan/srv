package main

import "srv/internal/daemon"

// Daemon RPC protocol + client + server impl moved to
// srv/internal/daemon. Aliases keep the existing CLI / tunnel /
// completion / mcp_tools / ui call sites compiling unchanged while
// the source of truth lives in the new package.

type (
	daemonRequest  = daemon.Request
	daemonResponse = daemon.Response
	tunnelInfo     = daemon.TunnelInfo
)

var (
	daemonDial   = daemon.DialSock
	daemonCall   = daemon.Call
	ensureDaemon = daemon.Ensure
	cmdDaemon    = daemon.Cmd
)
