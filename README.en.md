# srv

[中文](./README.md) | English

> Cross-platform SSH command runner. Configure locally, run on the remote. Persistent cwd, connection multiplexing, per-shell session isolation, detached jobs. Callable from Bash or as an MCP server (Claude Code / Codex). Zero third-party deps — just Python 3 + system `ssh` / `scp`.

## Cheat sheet

| What you want | Command |
|---|---|
| First-time setup + verify | `srv init && srv check` |
| Run on remote | `srv ls -la` / `srv "ps aux \| grep x"` |
| Persistent cwd | `srv cd /opt/app` |
| Switch profile (this shell) | `srv use <profile>` |
| Push changed files | `srv sync` |
| Push a single file | `srv push ./a.py` |
| Long-running background task | `srv -d ./build.sh` |
| Inspect background jobs | `srv jobs` / `srv logs <id> -f` |
| Diagnose connection issues | `srv check` |
| Interactive (vim/htop) | `srv -t <cmd>` |
| Claude Code integration | See [Claude Code / Codex integration](#claude-code--codex-integration) |

## Contents

1. [What it solves](#what-it-solves)
2. [Install](#install)
3. [Quickstart](#quickstart)
4. [Subcommands](#subcommands)
5. [Profile keys](#profile-keys)
6. [Multi-server, multi-terminal](#multi-server-multi-terminal)
7. [Network resilience](#network-resilience)
8. [Claude Code / Codex integration](#claude-code--codex-integration)
9. [Files](#files)
10. [Environment variables](#environment-variables)
11. [Troubleshooting](#troubleshooting)
12. [Design tradeoffs / known limitations](#design-tradeoffs--known-limitations)

---

## What it solves

Developing locally but needing a real server to actually run things means a lot of `ssh user@host "cd /opt && python test.py"`. Every call also pays a full TCP+SSH handshake, which gets painful on bad networks. `srv` packages the whole workflow:

- After `srv cd /opt`, `srv python test.py` runs in `/opt` automatically
- ControlMaster auto-multiplexes connections — subsequent calls return in <30 ms
- Multiple terminals and multiple servers don't clobber each other
- `srv -d` detaches long-running tasks; logs land on the remote
- AI clients (Claude Code / Codex) get a structured MCP interface for free

---

## Install

### Prerequisites

- Python 3.9+ (Windows Store Python is fine)
- OpenSSH client (built-in on Windows 10+; usually default on macOS / Linux)

### Windows

Add the tool directory to your user PATH:

```powershell
[Environment]::SetEnvironmentVariable(
    "Path",
    "$([Environment]::GetEnvironmentVariable('Path','User'));D:\WorkSpace\server\srv",
    "User"
)
```

Open a new PowerShell, then `srv version`.

### macOS / Linux

Add the project directory to PATH (recommended; least intrusive):

```sh
echo 'export PATH="$PATH:/path/to/srv"' >> ~/.bashrc   # or ~/.zshrc
chmod +x /path/to/srv/srv
exec $SHELL && srv version
```

Or symlink the shim into an existing PATH directory (the shim follows symlinks):

```sh
chmod +x /path/to/srv/srv
ln -s /path/to/srv/srv ~/.local/bin/srv
srv version
```

---

## Quickstart

```sh
$ srv init
profile name [prod]:
host (ip or hostname): 1.2.3.4
user [admin]: ubuntu
port [22]:
identity file (blank = ssh default):
default cwd [~]: /opt
saved profile 'prod' to ~/.srv/config.json

$ srv status
profile : prod (default)
target  : ubuntu@1.2.3.4:22
cwd     : /opt
session : 11872
defaults: multiplex=True  compression=True  connect_timeout=10s

$ srv ls -la                     # runs in remote /opt
$ srv cd app
/opt/app
$ srv "ps aux | grep python"     # pipes need local quoting
$ srv -t htop                    # interactive (TTY)
$ srv -d ./long-build.sh         # detached
```

---

## Subcommands

### Profile management

```
srv init                            # interactive wizard for a new profile
srv config list                     # list profiles; * = global default, @ = pinned to this session
srv config show [name]              # full JSON for one profile
srv config use <name>               # set the global default
srv config remove <name>            # delete a profile
srv config set <prof> <key> <val>   # set one key (true/false/int/null are auto-typed)
```

### Quick profile switching

```
srv use <profile>     # pin <profile> to this shell; subsequent srv calls use it
srv use --clear       # unpin
srv use               # show current pin / default / active
```

**Resolution order** (highest first):

```
-P/--profile (per call)
  > srv use session pin
  > $SRV_PROFILE
  > srv config use default
```

### Running on the remote

```
srv <args...>            # run in current cwd (default subcommand)
srv run <args...>        # explicit form (use this when args clash with subcommand names)
srv -t <cmd>             # allocate a TTY (vim / htop / sudo password)
srv -P <profile> <cmd>   # one-shot profile override
srv -d <cmd>             # detached background job (see below)
```

Commands containing shell metacharacters need **local** quoting — `srv` joins all args and hands the whole string to the remote shell:

```sh
srv "ls /var/log | grep error"
srv 'find . -name "*.py"'
srv "FOO=1 python script.py"            # one-shot env var
srv "bash -ic 'myalias arg'"            # force interactive shell so aliases load
```

### Connectivity diagnosis

```
srv check        # active probe with BatchMode=yes; diagnoses common failure modes
```

Never hangs (no ControlMaster, no stdin reads), auto-accepts first-time host keys. Diagnosis categories:

| diagnosis | meaning | output hint |
|---|---|---|
| `no-key` | server rejected publickey | prints `ssh-copy-id` and PowerShell-equivalent pipe |
| `host-key-changed` | host key mismatch | prints `ssh-keygen -R` + `ssh-keyscan` |
| `dns` | hostname resolution failed | prompts to check host spelling |
| `refused` | connection refused | sshd not running / wrong port / firewall |
| `no-route` | network unreachable | VPN / routing |
| `tcp-timeout` | TCP timed out | server down / silent firewall drop |
| `perm-denied` | generic auth failure | check key pairing |

`srv init` suggests running `srv check` immediately after — you find out in 15 s whether your config actually works.

### Working directory

```
srv cd <path>    # remote-validates `cd <path> && pwd`, persists absolute path to this session
srv cd           # cd to ~
srv pwd          # display current cwd
```

`srv cd` is intercepted locally — it doesn't actually run `cd` on the remote (that wouldn't survive across separate ssh calls). State lives in `sessions.json` and is **per terminal**.

### File transfer (scp)

```
srv push <local> [<remote>] [-r]    # upload (auto -r when local is a directory)
srv pull <remote> [<local>] [-r]    # download
```

Remote paths resolve relative to the current cwd:

- `srv push ./a.py` → uploads to `<cwd>/a.py`
- `srv push ./dist /opt/app` → uploads to `/opt/app`
- `srv pull logs/app.log` → downloads from `<cwd>/logs/app.log` to local `.`
- Absolute paths (`/...`) and `~/...` go through unchanged

### Bulk sync of changed files

Streams via `tar -cf - | ssh remote tar -xf -` — single ssh connection, preserves relative paths, near-zero handshake cost when ControlMaster is on.

```
srv sync                              # in a git repo: modified+staged+untracked
srv sync --staged                     # staged only
srv sync --modified                   # working-tree changes only
srv sync --untracked                  # untracked only
srv sync --since 2h                   # mtime-based (2h / 30m / 1d / 90s)
srv sync --include "src/**/*.py"      # glob mode, repeatable
srv sync --files a.py --files b/c.py  # explicit list; also `srv sync -- a.py b.py`
srv sync --dry-run                    # preview, don't transfer
srv sync --exclude "*.log"            # extra exclude, repeatable
srv sync /opt/app                     # explicit remote root (default = sync_root or cwd)
srv sync --root ./subproject          # explicit local root (default = git toplevel / cwd)
srv sync --no-git                     # disable git auto-mode in a repo
```

Default excludes: `.git`, `node_modules`, `__pycache__`, `.venv`, `venv`, `.idea`, `.vscode`, `.DS_Store`, `*.pyc`, `*.pyo`, `*.swp`. `list` mode (`--files`) skips default excludes — explicit user files are unconditionally sent.

Files are anchored at the git toplevel (git mode) or current dir (other modes); the remote receives them at `remote_root/<relative_path>`.

### Detached jobs

```
srv -d <cmd>      # nohup + redirect to ~/.srv-jobs/<id>.log; returns job id and pid immediately
srv jobs          # list local job records
srv logs <id>     # cat the remote log
srv logs <id> -f  # tail -f
srv kill <id>     # SIGTERM
srv kill <id> -9                  # SIGKILL
srv kill <id> --signal=USR1       # custom signal
```

Job ids look like `20260506-143052-abc1` (second-precision timestamp + random suffix). Prefix matching is supported — `srv logs 20260506` works if unambiguous.

The user command is base64-encoded into the spawn line, sidestepping any nested-quoting problems.

### Sessions

```
srv sessions          # list all session records (alive/dead)
srv sessions show     # full JSON for current session
srv sessions clear    # drop current session record
srv sessions prune    # GC: remove records whose pid no longer exists
```

### Shell completion

```sh
# bash
srv completion bash > ~/.bash_completion.d/srv

# zsh
srv completion zsh > "${fpath[1]}/_srv"

# PowerShell — add to $PROFILE:
srv completion powershell | Out-String | Invoke-Expression
```

Covers: subcommands, `config` actions, profile names after `-P` and `use`, `sessions` actions, shell names after `completion`.

---

## Profile keys

Set with `srv config set <profile> <key> <value>`. Bool strings (`true`/`false`) and digit strings are auto-converted.

| Key | Default | Meaning |
|---|---|---|
| `host` | (required) | Remote host |
| `user` | current OS user | SSH username |
| `port` | 22 | SSH port |
| `identity_file` | null | Private key path; blank uses ssh's default search |
| `default_cwd` | `~` | Initial cwd for new sessions |
| `multiplex` | true | Enable ControlMaster connection sharing |
| `compression` | true | SSH transport compression |
| `connect_timeout` | 10 | Handshake timeout (seconds) |
| `keepalive_interval` | 30 | Keepalive probe interval (seconds) |
| `keepalive_count` | 3 | Probes that must fail before declaring the link dead |
| `control_persist` | `10m` | How long ControlMaster sockets linger idle |
| `sync_root` | null | Default remote root for `srv sync` (used when no positional arg given) |
| `sync_exclude` | `[]` | Profile-level extra excludes for `srv sync`, merged with defaults |
| `ssh_options` | `[]` | Raw `-o` strings, appended **last** (overrides everything above) |

---

## Multi-server, multi-terminal

### Model

- **Profile** = one server (host + user + port + key + default_cwd, etc.)
- **Session** = one shell instance. Session id = the shell process's PID. On Windows, intermediate `cmd.exe` shim and python launcher layers are skipped automatically to find the real shell
- cwd is keyed by **(session, profile)**

### Isolation matrix

| | Terminal A pinned to prod | Terminal B pinned to prod | Terminal C pinned to dev |
|---|---|---|---|
| In A: `srv cd /a` | A.prod.cwd=/a | unchanged | unchanged |
| In B: `srv cd /b` | unchanged | B.prod.cwd=/b | unchanged |
| In C: `srv cd /c` | unchanged | unchanged | C.dev.cwd=/c |
| In A: `srv -P dev cd /x` | A.dev.cwd=/x, A.prod.cwd unchanged | — | — |

A and B don't step on each other even using the same profile. A briefly switching to dev doesn't disturb dev's cwd in C.

### Explicit session id

```sh
# CI / scripts: pin a stable session across multiple srv calls
$ SRV_SESSION=ci-build-42 srv cd /opt
$ SRV_SESSION=ci-build-42 srv ./run.sh
```

---

## Network resilience

Every ssh / scp call automatically gets:

```
-o ControlMaster=auto
-o ControlPath=~/.srv/cm/%C.sock
-o ControlPersist=10m
-o ConnectTimeout=10
-o ServerAliveInterval=30
-o ServerAliveCountMax=3
-o TCPKeepAlive=yes
-o Compression=yes
```

**Multiplexing**: after the first handshake, the socket sticks around for 10 minutes. Subsequent `srv` calls reuse it and skip TCP/SSH handshake — latency drops from 100–300 ms to under 30 ms on flaky links.

**Retry**: handshake-class failures (ssh exit==255 within 5 seconds) auto-retry up to 3 times with 1 s / 2 s backoff. `-t` (interactive) and `-d` (spawn) skip retry to avoid replay hazards.

**Dead-link detection**: 30 s keepalive probes; 3 consecutive failures (90 s total) declare the link dead instead of hanging forever.

---

## Claude Code / Codex integration

### Option 1: Bash invocation

If `srv` is on PATH, you're done — both clients can call it as a regular command.

```
srv ls /opt
srv -d "python long.py"
```

### Option 2: MCP server (structured tools)

Claude Code gets 14 tools via stdio MCP (`run`, `cd`, `pwd`, `use`, `status`, `check`, `list_profiles`, `push`, `pull`, `sync`, `detach`, `list_jobs`, `tail_log`, `kill_job`). The MCP server's session id = the Claude Code process PID, so each Claude Code instance is independent.

**Claude Code** — pick one of three scopes depending on how you want it shared:

| Scope | Written to | Use case |
|---|---|---|
| `user` | `~/.claude.json` | Available in every project, **recommended for personal use** |
| `project` | `<repo>/.mcp.json` | **Shared with teammates** — commit and they get it on clone |
| `local` | per-project user file | Only in this project, only for you, not committed |

```sh
# 1) personal global (works in any directory)
claude mcp add srv --scope user -- python D:\WorkSpace\server\srv\src\srv.py mcp

# 2) project-shared (run from repo root; writes .mcp.json, commit it)
cd <your-project>
claude mcp add srv --scope project -- python D:\WorkSpace\server\srv\src\srv.py mcp

# 3) project-private (not in .mcp.json, only you see it)
cd <your-project>
claude mcp add srv --scope local -- python D:\WorkSpace\server\srv\src\srv.py mcp

# verify (works for any scope)
claude mcp list   # should show  srv: ✓ Connected
```

> On macOS / Linux replace the path with `/path/to/srv/src/srv.py` — or just `srv mcp` if `srv` is on PATH.

New Claude Code sessions pick it up automatically; existing sessions need `/mcp` to reconnect.

**Codex CLI** — `~/.codex/config.toml`:

```toml
[mcp_servers.srv]
command = "python"
args = ["D:\\WorkSpace\\server\\srv\\src\\srv.py", "mcp"]
```

---

## Files

`~/.srv/` (override with `$SRV_HOME`, mainly for isolation testing):

```
config.json          all profile definitions + global default
sessions.json        {session_id: {profile, cwds: {profile: cwd}, last_seen, started}}
jobs.json            local index of detached jobs
cm/                  ControlMaster sockets — one .sock per host+user+port
```

Remote `~/.srv-jobs/<id>.log` holds detached-job logs (auto-created).

---

## Environment variables

| Variable | Effect |
|---|---|
| `SRV_HOME` | Override config directory (default `~/.srv`) |
| `SRV_PROFILE` | Default profile for this shell (lower priority than `srv use`) |
| `SRV_SESSION` | Explicit session id; useful for scripts / CI sharing state across calls |

---

## Troubleshooting

### `error: 'ssh' not found in PATH`
Install OpenSSH client.
- Windows: `Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0` (admin PowerShell)
- Linux: `apt install openssh-client` etc.
- macOS: built-in

### Handshake still slow / multiplexing not kicking in
- `srv status` should show `multiplex=True`
- `~/.srv/cm/` should contain `.sock` files after the first connection
- Some servers disable multiplexing: `srv config set <prof> multiplex false`
- A conflicting `ControlPath` in `~/.ssh/config` may interfere

### Windows session id seems unstable / different each call
- Calls via the `srv` shim (`srv.cmd`) should be stable
- `python srv.py` direct calls are stable
- For unusual shell nesting, set `$env:SRV_SESSION = $PID` manually

### `srv -d` process exits immediately
- The remote needs `bash`, `base64`, `nohup` (coreutils, virtually always present)
- Check `srv logs <id>` for the remote stderr

### Claude Code doesn't see new MCP tools
The MCP server is loaded at session startup. Open a **new** Claude Code session, or `/mcp` to reconnect.

### `srv config set` change doesn't seem to apply
- Inspect `~/.srv/config.json` to confirm the value landed on the right profile
- A higher-priority `-P` flag, session pin, or `SRV_PROFILE` may be overriding

---

## Design tradeoffs / known limitations

- **Env vars don't persist across calls**: every `srv` invocation is a fresh ssh process. Use inline: `srv "FOO=1 python x.py"`.
- **Non-interactive ssh doesn't source `.bashrc`**: aliases / PATH from rc files aren't visible by default. `srv "bash -ic '<cmd>'"` forces an interactive shell.
- **Mid-transfer disconnects**: scp may leave a half-written file. Re-running overwrites; we accept that rather than implementing resume.
- **Long ssh commands die on disconnect**: only `srv -d` survives a network interruption.
- **ControlMaster compatibility**: Windows OpenSSH 9.5+ has full support. Older versions may need `multiplex=false`.
- **Session id can mis-detect under unusual shell nesting**: use `SRV_SESSION` to pin.
- **Single cwd per (session, profile)**: no `pushd`/`popd`-style stack.

---

## Further reading

- [README.md](./README.md) — Chinese version
- [CHANGELOG.md](./CHANGELOG.md) — version history
- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) — code organization and how to extend

---

## Version

Currently **0.7.5**. Version bumps on breaking changes — see `srv version` and the `VERSION` constant near the top of `srv.py`.
