# Architecture

[中文](./ARCHITECTURE.md) | English

This is the technical reference for `srv`: not just the high-level
architecture but the rationale behind the non-obvious implementation
choices (network/performance, cross-platform behavior, the guard
gate, package layering). The README is the user-facing guide; this
document is for contributors and anyone asking "why is it built this
way?".

## Contents

- [Overview](#overview)
- [High-Level Model](#high-level-model)
- [Repository Layout](#repository-layout)
- [Core Concepts](#core-concepts)
- [Command Dispatch](#command-dispatch)
- [SSH Client](#ssh-client)
- [Daemon](#daemon)
- [Network & Performance](#network--performance)
- [Sync](#sync)
- [MCP](#mcp)
- [Guard](#guard)
- [Cross-Platform Notes](#cross-platform-notes)
- [Installer](#installer)
- [State Files](#state-files)
- [Extension Checklist](#extension-checklist)
- [Testing](#testing)

## Overview

`srv` is a single-binary Go CLI for running commands on remote SSH
hosts. It keeps local state under `~/.srv`, talks SSH directly through
`golang.org/x/crypto/ssh` (no system `ssh`, no Python), and exposes
both a human CLI and a stdio MCP server for AI coding agents.

## High-Level Model

```text
local shell / MCP client
        |
        v
     srv CLI / srv mcp
        |
        +-- local state: config.json, sessions.json, jobs.json
        |
        +-- daemon client -> srv daemon -> pooled SSH clients
        |
        +-- direct SSH client -> remote host
```

The daemon is an optimization, not a requirement. Heavy streaming
operations can still use direct SSH where that is simpler or safer.

## Repository Layout

Standard Go layout: module at repo root, entrypoint under `cmd/srv`,
everything else under `internal/` (40 focused packages).

```text
cmd/srv/                entrypoint, global-flag parsing, command dispatch
internal/config         config schema, profile resolution, atomic JSON
                        writes, GuardActive (effective-guard resolver)
internal/session        per-shell records, session-id derivation,
                        cwd persistence, GuardPref (env+session slice)
internal/sshx           SSH dial/auth/known_hosts/keepalive/ProxyJump,
                        SFTP, capture & streaming run helpers
internal/daemon         connection-pool daemon + CLI-side protocol
internal/transfer       push/pull, parallel chunked transfer, resume
internal/syncx          sync file collection, tar stream, delete, watch
internal/remote         bare-CLI remote run path (streaming)
internal/runwrap        remote command wrapping: cwd, restart-on-fail,
                        cpu/mem limits
internal/mcp            stdio MCP server, tool handlers, pre-exec gates
internal/guard          `srv guard` CLI (toggle / rules / status)
internal/group          profile groups, parallel fan-out (-G)
internal/sudo           remote `sudo -S` password handling
internal/streams        tail / journal streaming with auto-reconnect
internal/tunnel         port-forward definitions
internal/tunnelproc     independent tunnel subprocess mode
internal/jobs           detached job records (jobs.json)
internal/jobcli         jobs / logs / kill CLI
internal/jobnotify      job-completion OS notify / webhook (leaf pkg)
internal/check          connectivity diagnosis, RTT, bandwidth, key rotate
internal/prune          `srv prune` targets (selective cleanup)
internal/recipe         named multi-step recipes
internal/hooks          lifecycle hooks (pre/post cd/sync/run/push/pull)
internal/history        ~/.srv/history.jsonl CLI command log
internal/atrest         at-rest AES-256-GCM for history / mcp-replay
internal/completion     local + remote shell completion
internal/picker         TTY selector UI
internal/ui             one-screen TUI dashboard
internal/theme          color presets
internal/progress       transfer progress meter
internal/platform       per-OS stats/notify (platform_*.go split)
internal/install        browser installer + platform PATH helpers
internal/launcher       background daemon / detached spawn helper
internal/srvtty         TTY / raw-mode / shell-quoting helpers
internal/srvutil        shared utils: paths, file lock, atomic JSON
internal/mcplog         mcp.log lifecycle + prune
internal/i18n           help text, zh/en localization
internal/project        .srv-project pin resolution
internal/diff           local/remote file diff
internal/editcmd        srv edit / open / code
internal/hints          command-spelling hints
```

## Core Concepts

### Profile

One SSH target: host/user/port/identity, default cwd, network
settings (keepalive, dial retry, pool size, compression), sync
defaults, ProxyJump chain, profile-level remote env. Stored in
`~/.srv/config.json`.

### Session

One local shell or MCP process. Stores an optional pinned profile, a
cwd map keyed by profile, the previous cwd (for `srv cd -`), the
per-shell guard tri-state, and last-seen metadata. Stored in
`~/.srv/sessions.json`.

Session id derivation (this is load-bearing for guard and cwd — see
[Cross-Platform Notes](#cross-platform-notes)):

- `SRV_SESSION` env wins if set.
- Unix: parent pid (`os.Getppid()`) — the interacting shell.
- Windows: walk the process tree, skipping launcher wrappers
  (`python.exe` et al.), stop at the first real shell.

Profile resolution order:

```text
-P/--profile > session pin > SRV_PROFILE > .srv-project > config default
```

### cwd

Remote `cd` cannot persist across separate SSH commands, so `srv cd`
validates the path remotely and stores the resulting absolute path
locally. Later commands wrap the remote command with
`cd <cwd> && (...)`.

## Command Dispatch

`cmd/srv` parses global flags first (`-P/--profile`, `-G/--group`,
`-t`, `-d`, `--no-hints`). Reserved subcommands are handled locally;
any first arg outside the reserved set is treated as a remote command
against the active profile. When an AI-agent shell is detected
(`CLAUDECODE` / `CODEX_*` markers), bare-CLI remote subcommands are
hard-refused and point at the MCP server (escape hatch:
`SRV_ALLOW_AI_CLI=1`) — bare CLI bypasses the MCP token/sync/guard
gates, so agents must go through MCP.

## SSH Client

`internal/sshx` owns SSH behavior:

- SSH-agent auth when `SSH_AUTH_SOCK` is set, then profile
  `identity_file`, then default key paths.
- known_hosts verification with accept-new on first connect; a changed
  key always rejects.
- optional ProxyJump chain; optional SOCKS5/HTTP-CONNECT proxy on the
  first dial only.
- TCP keepalive (SO_KEEPALIVE, 15s) and SSH-level keepalive.
- SFTP client lazy-initialized and owned by `*Client`.

`Client.Close()` tears down SFTP, the primary connection, the
ProxyJump chain (reverse order), and the keepalive goroutine (via a
stop channel so short-lived MCP clients don't pile up idle goroutines).

## Daemon

Listens on `~/.srv/daemon.sock`, pools SSH clients per profile, serves
`ls`/`cd`/`pwd`/`run`/`stream_run`/`status`/`shutdown`.

Design rules:

- Never hold `daemonState.mu` while dialing or running remote commands.
- Health-check idle pooled connections (>30s) before reuse so a
  silently-dead conn is never handed out.
- Single-flight identical concurrent `ls`/`run` requests.
- Drop expired completion-cache entries during GC; self-exit after
  30 min idle.

Pool sizing: `pool_size` defaults to 4 (clamped `[1,16]`). A single
SSH connection's flow-control window caps throughput on a high
bandwidth-delay-product link, and concurrent MCP calls / large sync
trees / a busy `srv ui` serialize behind one connection. Four parallel
connections fill the pipe without risking the remote `sshd`
`MaxStartups`/`MaxSessions` budget. `GetPoolSize()` treats unset and
`<1` as the default; `pool_size: 1` restores single-connection
behavior. `autoconnect: true` pre-warms a profile into the pool at
daemon startup (first call goes from a ~200-800ms handshake to 0-RTT).

## Network & Performance

- **Parallel chunked transfer.** Files ≥32 MiB with no resumable
  partial are split into 8 MiB chunks moved over N parallel
  `WriteAt`/`ReadAt` streams on one SSH connection, overlapping
  window-refresh round-trips. ~3-5× on high-RTT links, no-op on LAN.
  Tunable via `SRV_TRANSFER_CHUNK_{THRESHOLD,BYTES,PARALLEL}`.
- **Directory parallelism.** Recursive push/pull/sync fan files across
  `SRV_TRANSFER_WORKERS` goroutines (default 4, range 1-32) on the
  shared connection.
- **Resume with hash prefix check.** A partial is verified as a true
  prefix via a remote `sha256(head -c N)` (~80-byte reply) instead of
  re-downloading it to compare.
- **Compression.** `compress_sync` (default on) gzips the sync tar
  stream. `compress_streams` (default off) gzips captured stdout on
  the wire — only pays off on slow/cross-region links, decode failure
  falls back to plain.
- **Dial retry.** `dial_attempts` / `dial_backoff` (exponential,
  capped 30s); auth and host-key errors never retry — another round
  trip won't change the answer.
- **Keepalive.** TCP SO_KEEPALIVE (kernel notices a dead peer fast)
  plus SSH-level keepalive (`keepalive_interval`/`keepalive_count`).

## Sync

Four collection modes: git (modified/staged/untracked), mtime
(changed since a duration), glob (`**` supported), explicit list.
Transfer is a Go tar stream piped into remote `tar -xf -`
(`-xzf -` when `compress_sync`). Delete support is intentionally
git-mode only, with preview discipline and a default safety cap.
`sync --watch` installs fsnotify watchers on non-excluded dirs;
events are debounced and runs serialized, one follow-up queued.

## MCP

`internal/mcp` is JSON-RPC over stdio exposing structured tools for
remote run, cwd/profile, sync, transfer, jobs, diagnostics, daemon
status. Token discipline matters because clients keep tool schemas
and results in context:

- `run` output is capped (64 KiB).
- Large payloads are not duplicated in text + structured fields.
- `sync` returns counts, not full path lists.
- Tool descriptions are intentionally short.
- Unbounded sources (`cat`, bare `journalctl`/`find /`, `tail -f`)
  are rejected pre-exec and pointed at a bounded form / background job.

## Guard

The high-risk-op confirmation gate is ON by default. Rationale behind
the non-obvious parts:

**Narrow pattern set.** Only irreversible destruction (`rm -rf`,
`dd of=`, `mkfs`, `DROP`/`TRUNCATE`, raw-disk redirects, the NoSQL
equivalents, macOS `diskutil`/`newfs_*`) plus host power-control.
Recoverable-but-disruptive ops and pure precursors (`chattr -i`) are
deliberately excluded: with default-on, a false positive only costs a
re-issue with `confirm=true`, but constant friction on routine ops
would push users to disable the gate entirely. False negatives are
not recoverable, so the bias is "few rules, all unambiguous".

**Quoted-payload matching.** `codePositions` classifies each byte as
code vs string-literal so `echo "rm -rf /"` does not trip — quoted
content is treated as inert. That same rule would let
`mysql -e "DROP DATABASE x"` through. The DB-client rules work around
it by anchoring the regex on the *unquoted client binary* (`mysql`,
`psql`, `mongosh`, ...), which sits at a code position, then reaching
forward into the quoted arg. The match start is what the gate checks,
so the verb-in-quotes is caught, while an echo-wrapped form (where the
client name itself is quoted) is still suppressed — no broad
false-positive increase. `[^|;&\n]` on the client→flag and flag→verb
gaps keeps the verb in the same simple command, so a later
`&& echo "...drop database..."` cannot trip it. Bounded quantifiers
keep RE2 linear.

**Three-layer state, and why `--global` exists.** Effective state
resolves as: `SRV_GUARD` env > per-session record > global config
(`GuardConfig.GlobalOff`) > built-in ON. The per-session record is
keyed by a ppid-derived session id. The MCP server is a child process
of the AI client, not of the user's interactive shell, so its session
id never matches. A per-shell `srv guard off` therefore cannot reach
the model's path — that is the entire reason `srv guard off --global`
exists. It writes `config.json`, which the MCP server re-reads on
every call (live, no restart).

**Package layering.** `config` imports `session`, so the env+session
slice lives in `session.GuardPref()` (tri-state: enabled/disabled/
unset, no default applied) and the global+default layers live in
`config.GuardActive()`. `session` cannot import `config` (cycle), so
`GuardActive` is the single source of truth; every guard consumer
holding a `*config.Config` must call it rather than
`session.GuardOn()` (which only sees the env+session slice).

## Cross-Platform Notes

`srv` targets Windows, macOS, Linux, and the BSDs from one binary.
The non-portable details that bite:

- **Session id.** Unix uses the parent pid; Windows walks the process
  tree skipping launcher wrappers. Consequence: the MCP server's
  session never matches the interactive shell — see why `srv guard
  --global` exists in [Guard](#guard).
- **base64 decode.** GNU/busybox spell it `base64 -d`, macOS/BSD spell
  it `-D`. The detached-job spawn line tries `-d` then falls back to
  `-D`; without it, detached jobs on macOS decoded to nothing.
- **`setsid`.** util-linux only. The spawn line gates on
  `command -v setsid` and falls back to plain `nohup` on macOS/BSD
  (kill then reaches only the wrapper pid, which `kill_job` already
  handles).
- **Raw disk nodes.** macOS uses `/dev/rdiskN` for raw access; the
  guard's `> /dev/...` rule matches `r?disk` so the macOS form is
  covered. `dd of=` is caught regardless of target.
- **Per-OS code.** `internal/platform` and `internal/install` split by
  build tag (`*_unix.go` / `*_darwin.go` / `*_bsd.go` /
  `*_windows.go`); business code stays free of `runtime.GOOS`
  branching. A Windows `go test ./...` does **not** compile the
  non-Windows files — verify cross-platform changes with
  `GOOS=darwin/linux go build ./... && go vet ./...`.

## Installer

`srv install` serves embedded `install.html` on localhost: PATH setup,
Claude Code MCP registration, first profile. PATH/browser helpers are
in `install_unix.go` / `install_windows.go`.

## State Files

Default root `~/.srv`, override with `SRV_HOME`.

```text
config.json          profiles, groups, tunnels, hooks, global config
sessions.json        per-session pin/cwd/guard state
jobs.json            detached job records
history.jsonl        CLI remote-command log
mcp.log              MCP lifecycle + tool-call log
cache/               remote completion cache
daemon.sock          daemon socket
daemon.log           auto-spawn daemon output
```

Remote job logs: `~/.srv-jobs/<job-id>.log` (+ `.exit` marker).

## Extension Checklist

**Add a CLI command:** register in `cmd/srv` dispatch; implement the
handler in the right `internal/` package; update help text + README;
add completion entries; add parsing/behavior tests.

**Add an MCP tool:** add a compact tool def + handler branch in
`internal/mcp`; keep text/structured output non-duplicative; cap or
summarize large output; update the README MCP list.

**Add profile config:** add the field to `Profile`; add accessor
defaults; keep old configs valid; document in README (user-facing)
and here (rationale, if non-obvious).

## Testing

```sh
go test ./...
```

`go test ./...` on Windows only compiles the Windows build tags.
Verify cross-platform changes with:

```sh
GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...
GOOS=linux  GOARCH=amd64 go build ./... && GOOS=linux  GOARCH=amd64 go vet ./...
```

Areas that usually need real SSH: `check`, `run`/`shell`,
`push`/`pull`, `sync` (+`--watch`), `tunnel`, MCP registration. Live
SSH tests must be bounded and loop-safe. On Windows where Go cannot
write the default build cache, point `GOCACHE` at a writable dir.
