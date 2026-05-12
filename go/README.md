# srv Go Source

This directory contains the primary Go implementation of `srv`.

`srv` uses `golang.org/x/crypto/ssh` directly, so it does not need a system `ssh` or `scp` executable at runtime.

## Build

Requires Go 1.25+.

```sh
cd go
go build -o ../srv.exe .   # Windows
go build -o ../srv .       # macOS / Linux
```

Cross-build examples:

```sh
GOOS=windows GOARCH=amd64 go build -o ../srv.exe .
GOOS=linux   GOARCH=amd64 go build -o ../srv .
GOOS=darwin  GOARCH=arm64 go build -o ../srv .
```

## Test

```sh
go test ./...
```

On locked-down Windows environments, set a writable build cache:

```powershell
$env:GOCACHE = "D:\WorkSpace\server\srv\.gocache"
go test ./...
```

## Command Surface

```text
srv init
srv config <list|default|global|remove|show|set|edit>
srv use <profile> | --clear
srv cd <path>
srv pwd
srv status
srv check [--rtt]
srv doctor [--json]
srv <cmd>
srv run <cmd>
srv shell
srv -t <cmd>
srv -d <cmd>
srv -P <profile> <cmd>
srv -G <group> <cmd>
srv push <local> [remote]
srv pull <remote> [local]
srv sync [...]
srv edit <remote_file>
srv open <remote_file>
srv code [remote_dir]
srv diff [--changed] <local> [remote]
srv env <list|set|unset|clear>
srv tunnel [-R] <port-spec> | <add|up|down|list|show|remove> ...
srv jobs
srv logs <id> [-f]
srv kill <id> [-9|--signal=NAME]
srv tail [-n LINES] [--grep RE] <remote-path>...
srv watch [-n SECS] [--diff] <cmd>
srv journal [-u UNIT] [--since TIME] [-f] [-g RE] [-n LINES]
srv top [-n SECS]
srv sudo [--no-cache] [--cache-ttl <dur>] <cmd>
srv sessions [list|show|clear|prune]
srv daemon [status|restart|logs|prune-cache|stop]
srv group <list|show|set|remove> ...
srv project
srv ui
srv completion <bash|zsh|powershell> [--install]
srv mcp
srv guard [on|off|status]
srv install
srv help
srv version
```

## Picking the right command

Several command clusters share a mental model -- "watch a log",
"run a command", "transfer a file". These tables show what to reach
for given what you have.

### Viewing logs

| You have...                       | Command                                |
|-----------------------------------|----------------------------------------|
| A path on the remote              | `srv tail [-n N] [--grep RE] <path>`   |
| A systemd unit name               | `srv journal -u UNIT [-f]`             |
| A detached-job id                 | `srv logs <id> [-f]`                   |

All three follow with `-f` and reconnect on SSH drop. They cover
different *sources*, not different mechanics -- the right tool is
whichever matches what you can reference.

### Running a remote command

| You want...                       | Command                                |
|-----------------------------------|----------------------------------------|
| Short, capture stdout/stderr      | `srv run <cmd>` (or `srv <cmd>`)       |
| Interactive (vim, sudo prompt)    | `srv -t <cmd>` or `srv shell`          |
| Long-running, background          | `srv -d <cmd>` (creates a job)         |
| Periodic snapshot, in-place       | `srv watch [-n N] <cmd>`               |
| Parallel across hosts             | `srv -G <group> <cmd>`                 |
| Stream `top` indefinitely         | `srv top` (or `srv -t top` in-place)   |

### Transfer

| You want...                       | Command                                |
|-----------------------------------|----------------------------------------|
| Upload one file/dir               | `srv push <local> [remote]`            |
| Download one file/dir             | `srv pull <remote> [local]`            |
| Bidirectional diff before edit    | `srv diff <local> [remote]`            |
| Open in `$EDITOR`, save back      | `srv edit <remote>`                    |
| Pull to temp, open in OS app      | `srv open <remote>`                    |
| Open remote folder in VS Code     | `srv code [remote_dir]`                |
| Bulk incremental sync             | `srv sync [...]`                       |

### Tunnels

| You want...                       | Command                                |
|-----------------------------------|----------------------------------------|
| One-shot foreground tunnel        | `srv tunnel [-R] <spec>`               |
| Named persistent (in daemon)      | `srv tunnel add <name> <spec>` then `srv tunnel up <name>` |
| List / inspect                    | `srv tunnel list` / `srv tunnel show <name>` |
| Auto-start on daemon boot         | `srv tunnel add ... --autostart`       |

### State + dashboard

| You want...                       | Command                                |
|-----------------------------------|----------------------------------------|
| Current profile / cwd             | `srv status` / `srv pwd`               |
| SSH connectivity OK?              | `srv check [--rtt]`                    |
| Full local readiness report       | `srv doctor`                           |
| One-screen live overview          | `srv ui`                               |
| Pinned by .srv-project?           | `srv project`                          |

## MCP

Build the binary first, then register it with an MCP client:

```sh
cd go
go build -o ../srv.exe .
cd ..
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp
claude mcp list
```

MCP tools currently include:

```text
run, cd, pwd, use, status, check, list_profiles, doctor, daemon_status,
env, diff, push, pull, sync, sync_delete_dry_run, detach, list_jobs,
tail_log, kill_job
```

Token-sensitive MCP behavior lives in `mcp.go`: large `run` output is capped, duplicated structured/text payloads are avoided, and sync success responses return counts instead of full path lists.

## File Map

```text
main.go                 CLI entry, global flags, subcommand dispatch, help/version
config.go               ~/.srv/config.json, Profile, ResolveProfile
session.go              ~/.srv/sessions.json and session model
session_unix.go         Unix session id
session_windows.go      Windows process-tree session id
client.go               SSH client, auth, ProxyJump, keepalive, SFTP
ops.go                  Remote run/cd/push/pull/diff helpers
cmds.go                 Config/use/cd/pwd/status command handlers
feature_cmds.go         doctor/open/code/diff/env helpers
check.go                SSH connectivity diagnosis and RTT probe
jobs.go                 Detached job records and log/kill commands
sync.go                 sync file collection, tar upload, delete handling
sync_watch.go           fsnotify-based sync --watch loop
tunnel.go               local and reverse TCP forwarding
daemon.go               SSH connection pool daemon
daemon_client.go        CLI client for daemon protocol
daemon_spawn*.go        background daemon spawn per platform
completion.go           shell completion scripts
completion_remote.go    remote completion and cache
mcp.go                  stdio MCP server
install.go              browser-based installer and MCP registration
install.html            embedded installer UI
install_unix.go         Unix install helpers
install_windows.go      Windows install helpers
regex.go                cached regex matching for glob/exclude logic
term.go                 terminal/raw-mode helpers
helpers.go              small shared helpers
```

## Important Runtime State

`ConfigDir()` defaults to `~/.srv` and can be overridden by `SRV_HOME`.

```text
config.json        profiles and global settings
sessions.json      per-shell profile pin and cwd map
jobs.json          detached job records
cache/             remote completion cache
daemon.sock        daemon AF_UNIX socket
daemon.log         auto-spawned daemon output
```

Remote detached-job logs are written under `~/.srv-jobs/`.

## Development Notes

- Prefer existing helpers over new abstractions.
- Keep MCP responses compact; the client stores tool results in conversation history.
- Keep daemon operations responsive: slow dial/handshake work should not hold `daemonState.mu`.
- `sync --watch` must serialize sync runs; file events during an active sync should queue one follow-up run, not start concurrent uploads.
- Avoid changing user-facing CLI text casually; `main.go`, `i18n.go`, and README should stay aligned.

## Release

Releases are handled by GitHub Actions and goreleaser. Maintainer flow:

```sh
# update CHANGELOG.md first
git tag vX.Y.Z
git push origin vX.Y.Z
```

The release workflow builds linux/darwin/windows archives, injects the tag into `main.Version`, generates `checksums.txt`, and publishes a GitHub Release.
