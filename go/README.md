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

## Command reference (by category)

Every command with one paragraph on what it does, the common forms,
and when *not* to use it (pointing at the sibling that handles that
case better).

### Setup

**`srv init`** -- interactive first-run wizard. Walks through host /
user / identity_file and writes `~/.srv/config.json`. Run once per
machine.

**`srv install`** -- spawns the browser-based installer UI for
registering srv with downstream MCP clients (Claude Code, Codex).
Does not edit `~/.srv/config.json` itself; use `srv init` for that.

**`srv completion <bash|zsh|powershell> [--install]`** -- emits the
shell completion script; `--install` writes a loader into the shell's
rc file (idempotent, marker-block-fenced).

### Profile & session

**`srv config <list|default|global|remove|show|set|edit>`** --
CRUD on `~/.srv/config.json`. `set` mutates a single field; `edit`
opens the file in `$EDITOR`. `default <profile>` chooses the
fallback when nothing else pins.

**`srv use [<profile> | --clear]`** -- pin a profile for the
current shell session. With no arg on a TTY, opens an interactive
picker. With `--clear`, unpins.

**`srv -P <profile> <cmd>`** -- one-shot profile override. Bypasses
the session pin / project file / global default for this single
invocation.

**`srv status`** -- prints active profile name, host, cwd, daemon
status. The "what does srv see right now" sanity check.

**`srv pwd`** / **`srv cd <path>`** -- show / change the persisted
remote cwd for this session+profile. Next `run` / `push` / `pull`
will use it as the base.

**`srv sessions <list|show|clear|prune>`** -- inspect / manage the
shell-session records in `~/.srv/sessions.json`. `prune` removes
records for shells whose pid is dead.

**`srv project`** -- prints the resolved `.srv-project` file (if any)
and the pin it carries. Useful for "why did srv pick that profile?"
debugging.

**`srv group <list|show|set|remove>`** -- named profile groups for
fan-out. `srv group set web web-1 web-2 web-3` defines `web` as a
3-member group; `srv -G web <cmd>` runs the command on all three
in parallel.

### Running remote commands

**`srv <cmd>`** -- catch-all. Unrecognized first tokens are treated
as remote shell commands. Same as `srv run <cmd>`.

**`srv run <cmd>`** -- explicit run with captured stdout/stderr.
The command is wrapped in the persisted cwd via `cd ... && ( ... )`.
Returns the remote exit code.

**`srv -t <cmd>`** -- allocate a remote pty (TTY mode). Necessary
for interactive things: vim, htop, sudo password prompt, anything
that checks `isatty()`.

**`srv -d <cmd>`** -- detach. Creates a job record in
`~/.srv/jobs.json`, logs to `~/.srv-jobs/<id>.log` on the remote,
returns the job id. Pair with `srv logs <id> -f` to watch and
`srv kill <id>` to terminate.

**`srv shell`** -- interactive remote shell session (pty + login
shell). Quits with the shell's own exit, e.g. `exit`/Ctrl-D.

**`srv watch [-n SECS] [--diff] <cmd>`** -- BSD `watch` over SSH:
runs the command every N seconds, redraws in place, optional
line-level diff highlight. Uses the daemon pool so each tick is
sub-100ms warm.

**`srv top [-n SECS]`** -- streams `top -b -d N` from the remote
with auto-reconnect. Scrolling "log of frames" view. For an
in-place curses-style top use `srv -t top` instead.

**`srv sudo [--no-cache] [--cache-ttl <dur>] <cmd>`** -- runs the
command via remote `sudo`. Password is read locally with echo
off, piped to `sudo -S`, and cached in the daemon's memory for
~5min so consecutive sudos don't re-prompt. Never persisted to disk.

**`srv -G <group> <cmd>`** -- fan-out: run the command on every
profile in `<group>` in parallel. One section per profile in the
output, summary line at the end; max non-zero exit code becomes
the shell exit.

### Viewing remote logs

These three target different *sources*; pick by what reference you have.

**`srv tail [-n LINES] [--grep RE] <path>...`** -- live-follow any
remote file. Reconnects automatically on SSH drop, with exponential
backoff. Use `--grep` for client-side regex filter.

**`srv journal [-u UNIT] [--since TIME] [-f] [-g RE] [-n LINES] [-p PRI]`** --
systemd journalctl wrapper. `-f` follows; without it, one-shot.
Argument shape matches journalctl so muscle memory transfers.

**`srv logs <id> [-f]`** -- output of a detached srv job (the
`~/.srv-jobs/<id>.log` file written by `-d <cmd>` /
`srv detach`). Resolves the id then `cat` or `tail -f`.

### File transfer

**`srv push <local> [remote]`** -- upload via SFTP. Resumes
partial uploads (verified by remote sha256 of the prefix bytes),
mirrors local mode on success.

**`srv pull <remote> [local]`** -- download via SFTP. Same resume
semantics in reverse. If `[local]` is an existing directory, the
remote's basename is appended.

**`srv sync [...]`** -- bulk incremental sync, mode-driven:
`git` (changed-by-git), `mtime` (newer-than), `glob` (include
patterns), `list` (explicit files). Uploads a tar stream and
optionally deletes removed files (`--delete`).

**`srv edit <remote-file>`** -- pulls the remote file to a temp
dir, opens it in `$EDITOR`, and pushes back if the buffer changed.
For one-off edits without leaving srv.

**`srv open <remote-file>`** -- pulls to a temp file and opens it
in the OS default app (Quick Look on macOS, Explorer on Windows,
xdg-open on Linux). Read-only by intent.

**`srv code [<remote-dir>]`** -- launches VS Code Remote SSH on
the remote dir. Builds the right `vscode://vscode-remote/ssh-remote/`
URI from the profile.

**`srv diff [--changed] <local> [remote]`** -- diff a local file
vs its remote counterpart. `--changed` looks at git-changed files
under the local path.

### Tunneling

**`srv tunnel [-R] <port-spec>`** -- one-shot foreground tunnel.
Blocks until Ctrl-C. Spec forms: `N` / `L:R` / `L:host:R`. `-R`
flips the direction (remote port -> local).

**`srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart]`** --
save a named tunnel definition. `--autostart` brings it up
automatically when the daemon starts.

**`srv tunnel up <name>`** / **`down <name>`** -- start / stop a
saved tunnel inside the daemon process so it survives the CLI exit.

**`srv tunnel list`** / **`show <name>`** -- inspect saved tunnels
overlaid with live up/down status.

**`srv tunnel remove <name>`** -- delete the saved definition. Use
`down` first to stop a running instance.

### Background jobs

**`srv jobs`** -- list detached jobs (id, pid, profile, started,
cmd).

**`srv logs <id> [-f]`** -- view a job's log (see "Viewing remote
logs" above for the broader log-view cluster).

**`srv kill <id> [-9|--signal=NAME]`** -- send a signal to the
remote pid behind a job. Drops the job record on success.

### Connectivity & dashboard

**`srv check [--rtt]`** -- one-round SSH probe with structured
diagnosis (auth / host-key / dns / dial / etc.). `--rtt` adds a
round-trip latency report.

**`srv doctor [--json]`** -- local readiness check: config valid?
daemon up? known_hosts writable? `--json` for scripting.

**`srv ui`** -- one-screen dashboard (profiles, daemon, tunnels,
MCP, jobs, sessions). Auto-refreshes only when data changes;
flicker-free. `q` to quit, `r` to force-redraw.

### Knobs

**`srv env <list|set|unset|clear>`** -- manage `Profile.Env` (env
vars prepended to every remote command on this profile). Stored
in `~/.srv/config.json`.

**`srv color <on|off|auto|use|list|status>`** -- local color preset
inlined before user commands in CLI mode (LS_COLORS, etc.). MCP
mode never colors.

**`srv guard <on|off|status>`** -- per-session MCP confirmation
gate. When on, MCP `run` / `detach` / `sync` calls with destructive
patterns (`rm -rf`, `dd of=`, `drop database`, ...) require
`confirm=true`.

### Background services

**`srv daemon <status|stop|restart|logs|prune-cache>`** -- control
the connection-pool daemon (`~/.srv/daemon.sock`). The daemon
keeps SSH connections warm across CLI invocations and hosts
saved tunnels.

**`srv mcp`** -- run as a stdio MCP server. Started by Claude Code
/ Codex when registered.

### Help

**`srv help`** / **`srv --help`** / **`srv -h`** -- full help text
(localized: respects `SRV_LANG=zh`).

**`srv version`** / **`srv --version`** -- print version string.

### Global flags

These flags work before any subcommand:

| Flag                 | Purpose                                          |
|----------------------|--------------------------------------------------|
| `-P <profile>` / `--profile <profile>` | One-shot profile override   |
| `-G <group>` / `--group <group>`       | Fan-out across a group      |
| `-t` / `--tty`       | Allocate a pty for the remote command            |
| `-d` / `--detach`    | Create a detached job instead of running fg      |
| `--no-hints`         | Suppress the typo-correction / hint emitter      |

`-P` and `-G` are mutually exclusive.

## MCP

Build the binary first, then register it with an MCP client:

```sh
cd go
go build -o ../srv.exe .
cd ..
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp
claude mcp list
```

### MCP tool reference

Grouped by purpose. Token-economy gates are noted where the call
can produce unbounded output -- they reject the call before any
remote work happens, with a structured error the model can branch
on (`rejected_reason="unbounded_streaming" | "unbounded_output"`).

#### Profile & session

| Tool             | Purpose                                                    |
|------------------|------------------------------------------------------------|
| `use`            | Pin / clear the active profile for the MCP session         |
| `cd`             | Change persisted remote cwd                                |
| `pwd`            | Show persisted remote cwd                                  |
| `status`         | Active profile + connection details                        |
| `list_profiles`  | Available profile names                                    |

#### Diagnostics

| Tool             | Purpose                                                    |
|------------------|------------------------------------------------------------|
| `check`          | SSH connectivity probe with structured diagnosis           |
| `doctor`         | Local readiness report                                     |
| `daemon_status`  | Daemon up? uptime, pooled profiles                         |
| `list_dir`       | `ls -1Ap` of a remote path (warm daemon cache)             |

#### Run

| Tool             | Purpose                                                    |
|------------------|------------------------------------------------------------|
| `run`            | Synchronous remote command. **Gate:** `cat <file>` / bare `dmesg` / unfiltered `journalctl` / `find /` rejected unless a downstream limiter (`\| head`, `\| tail`, `\| grep`, ...) is present. |
| `run_stream`     | Same as `run`, but stdout/stderr arrives via `notifications/progress`. Same gate. |
| `run_group`      | Fan-out `run` across a profile group; per-profile structured results. |
| `detach`         | Start command as a background job; returns job_id          |
| `wait_job`       | Poll a job for completion with bounded short-wait          |
| `kill_job`       | Signal a job's remote pid                                  |
| `list_jobs`      | Detached job records                                       |

#### Viewing remote logs

| Tool             | Purpose                                                    |
|------------------|------------------------------------------------------------|
| `tail`           | Read last N lines of a file. **Gate:** any `follow_seconds > 0` requires a `grep` regex. `lines` clamped to 1000. |
| `journal`        | systemd journal. **Gate:** any `follow_seconds > 0` requires at least one of `unit` / `since` / `priority` / `grep`. `lines` clamped to 2000. |
| `tail_log`       | Tail a detached-job log by job_id (one-shot)               |

#### Environment / transfer

| Tool                    | Purpose                                              |
|-------------------------|------------------------------------------------------|
| `env`                   | Manage `Profile.Env`                                 |
| `diff`                  | Diff local vs remote file                            |
| `push`                  | Upload file or directory                             |
| `pull`                  | Download file or directory                           |
| `sync`                  | Bulk incremental sync                                |
| `sync_delete_dry_run`   | Preview the deletes a `sync --delete` would do       |

Implementation note: token-sensitive behavior lives in
`mcp_tools.go` / `mcp_proto.go`. Large `run` output is capped at 64
KiB, duplicated structured/text payloads are avoided, sync success
returns counts instead of full path lists, and progress
notifications during streaming follow modes are bounded by the
filter requirement (no filter = call rejected at parse time).

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
