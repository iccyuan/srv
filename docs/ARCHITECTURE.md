# Architecture

`srv` is a single-binary Go CLI for running commands on remote SSH hosts. It keeps local state under `~/.srv`, talks SSH directly through `golang.org/x/crypto/ssh`, and exposes both a human CLI and a stdio MCP server.

## High-Level Model

```text
local shell / MCP client
        |
        v
     srv CLI
        |
        +-- local state: config.json, sessions.json, jobs.json
        |
        +-- daemon client -> srv daemon -> pooled SSH clients
        |
        +-- direct SSH client -> remote host
```

The daemon is an optimization, not a requirement. Heavy streaming operations can still use direct SSH where that is simpler or safer.

## Core Concepts

### Profile

A profile describes one SSH target:

- host, user, port, identity file
- default cwd
- network settings such as keepalive and dial retry
- sync defaults
- ProxyJump chain
- profile-level remote env vars

Profiles live in `~/.srv/config.json`.

### Session

A session is one local shell or MCP process. It stores:

- optional pinned profile
- cwd map keyed by profile
- last-seen metadata

Sessions live in `~/.srv/sessions.json`.

Resolution order:

```text
-P/--profile > session pin > SRV_PROFILE > config default
```

### cwd

Remote `cd` cannot persist across separate SSH commands, so `srv cd` validates the path remotely and stores the resulting absolute path locally. Later commands wrap the remote command with `cd <cwd> && (...)`.

## Main Packages / Files

```text
main.go                 entrypoint and command dispatch
config.go               config schema, profile resolution, atomic JSON writes
session.go              session records and cwd persistence
client.go               SSH dial/auth/keepalive/ProxyJump/SFTP
ops.go                  remote run and file operations
cmds.go                 config/use/cd/pwd/status handlers
feature_cmds.go         doctor/open/code/diff/env helpers
check.go                connectivity diagnosis and RTT probe
jobs.go                 detached job records and log/kill commands
sync.go                 file collection, tar stream upload, delete support
sync_watch.go           fsnotify watch mode
tunnel.go               local and reverse port forwarding
daemon.go               SSH connection pool daemon
daemon_client.go        CLI side of daemon protocol
completion*.go          local and remote shell completion
mcp.go                  stdio MCP server and tool handlers
install*.go             browser installer and platform-specific PATH helpers
```

## Command Dispatch

`main.go` parses global flags first:

- `-P` / `--profile`
- `-t`
- `-d`
- `--no-hints`

Reserved subcommands are handled locally. Any first arg outside the reserved set is treated as a remote command and passed to the active profile.

## SSH Client

`client.go` owns SSH behavior:

- SSH agent auth if `SSH_AUTH_SOCK` is present
- profile `identity_file`, otherwise common default key paths
- known_hosts verification with accept-new behavior
- optional ProxyJump chain
- TCP keepalive and SSH keepalive
- SFTP client lazy initialization

`Client.Close()` tears down SFTP, primary SSH connection, ProxyJump chain, and keepalive goroutine.

## Daemon

The daemon listens on `~/.srv/daemon.sock` and pools SSH clients by profile.

Supported operations include:

- `ls` for remote completion
- `cd`
- `pwd`
- `run`
- `stream_run`
- `status`
- `shutdown`

Design rules:

- Do not hold `daemonState.mu` while dialing or running remote commands.
- Health-check idle pooled connections before reuse.
- Drop expired completion cache entries during GC.
- Self-exit after 30 minutes idle.

## Sync

`srv sync` has four collection modes:

- git: modified/staged/untracked
- mtime: changed since a duration
- glob: include patterns, supports `**`
- list: explicit files

Transfer uses a Go tar stream piped into remote `tar -xf -`; when `compress_sync` is enabled it uses gzip and remote `tar -xzf -`.

Delete support is intentionally limited to git mode. Deletes require preview discipline and have a default safety cap.

`sync --watch` installs fsnotify watchers on non-excluded directories. Events are debounced and sync runs are serialized; events during an active sync queue one follow-up run.

## MCP

`mcp.go` implements JSON-RPC over stdio. The server exposes structured tools for remote command execution, cwd/profile management, sync, file transfer, detached jobs, diagnostics, and daemon status.

Token discipline matters because MCP clients keep tool schemas and tool results in context:

- `run` output is capped.
- Large payloads are not duplicated in both text and structured fields.
- `sync` success returns counts instead of full path lists.
- Tool descriptions are intentionally short.

## Installer

`srv install` starts a localhost HTTP server with embedded `install.html`. It helps with:

- adding `srv` to PATH
- registering Claude Code MCP
- creating the first profile

Platform-specific PATH and browser helpers live in `install_unix.go` and `install_windows.go`.

## State Files

Default root: `~/.srv`, override with `SRV_HOME`.

```text
config.json          profiles and global config
sessions.json        per-session pin/cwd state
jobs.json            detached job records
cache/               remote completion cache
daemon.sock          daemon socket
daemon.log           auto-spawn daemon output
cm/                  legacy/control socket directory when applicable
```

Remote job logs:

```text
~/.srv-jobs/<job-id>.log
```

## Extension Checklist

### Add a CLI command

1. Add the name to `reservedSubcommands` in `main.go`.
2. Implement `cmd<Name>` in the appropriate file.
3. Wire dispatch in `main.go`.
4. Update help text and README.
5. Add completion entries if relevant.
6. Add tests for parsing or shared behavior when practical.

### Add an MCP tool

1. Add a compact tool definition in `mcpToolDefs`.
2. Add a handler branch in `mcpHandleTool`.
3. Keep text and structured output non-duplicative.
4. Cap or summarize large outputs.
5. Update README MCP tool list.

### Add profile config

1. Add a field to `Profile`.
2. Add accessor defaults when needed.
3. Update config docs and README.
4. Ensure old configs remain valid.

## Testing

Core local tests:

```sh
go test ./...
```

Manual areas that usually need real SSH:

- `srv check`
- `srv run` / `srv shell`
- `srv push` / `srv pull`
- `srv sync` and `srv sync --watch`
- `srv tunnel`
- MCP client registration and tool calls

On Windows machines where Go cannot write the default build cache, point `GOCACHE` at a writable workspace directory.
