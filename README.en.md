# srv

[中文](./README.md) | English

> Cross-platform SSH command runner. Configure locally, run on the remote. Persistent cwd, connection multiplexing, per-shell session isolation, detached jobs. Callable from Bash or as an MCP server (Claude Code / Codex). A single Go binary with zero runtime dependencies; speaks the SSH protocol natively (no system ssh.exe required).

## Cheat sheet

| What you want | Command |
|---|---|
| First-time setup + verify | `srv init && srv check` |
| Run on remote | `srv ls -la` / `srv "ps aux \| grep x"` |
| Persistent cwd | `srv cd /opt/app` |
| Switch profile (this shell) | `srv use <profile>` |
| Push changed files | `srv sync` |
| Push a single file | `srv push ./a.py` |
| Diagnose local setup | `srv doctor` |
| Compare local/remote file | `srv diff ./a.py` |
| Forward a port (see remote dev server) | `srv tunnel 8080` |
| Edit a remote file in $EDITOR | `srv edit /etc/foo.conf` |
| Open a remote folder in VS Code | `srv code /opt/app` |
| Open a remote file locally | `srv open logs/app.log` |
| Long-running background task | `srv -d ./build.sh` |
| Inspect background jobs | `srv jobs` / `srv logs <id> -f` |
| Live-follow a remote file | `srv tail [-f] [--grep RE] <path>` |
| Follow a systemd unit | `srv journal -u <unit> [-f]` |
| Periodic command view (in-place) | `srv watch -n 1 "ps aux \| head"` |
| Stream remote top | `srv top` |
| Run on a profile group in parallel | `srv -G <group> <cmd>` |
| Remote sudo (no-echo password prompt) | `srv sudo <cmd>` |
| One-screen state overview | `srv ui` |
| Diagnose connection issues | `srv check` |
| Interactive (vim/htop) | `srv -t <cmd>` |
| Claude Code integration | See [Claude Code / Codex integration](#claude-code--codex-integration) |

## Contents

1. [What it solves](#what-it-solves)
2. [Install](#install)
3. [Quickstart](#quickstart)
4. [Subcommands](#subcommands)
   - [Live remote views](#live-remote-views)
   - [Parallel fan-out (profile groups)](#parallel-fan-out-profile-groups)
   - [Remote sudo](#remote-sudo)
   - [State dashboard (srv ui)](#state-dashboard-srv-ui)
   - [Per-project pin (.srv-project)](#per-project-pin-srv-project)
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

Three paths. **No Python, no system ssh client required** — the Go binary speaks SSH directly.

### Path A: One-liner (recommended — no build)

Downloads the latest release binary, puts it on PATH, prints next steps. End-to-end automated.

**macOS / Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/iccyuan/srv/main/get.sh | sh
```

**Windows (PowerShell)**:

```powershell
iwr -useb https://raw.githubusercontent.com/iccyuan/srv/main/get.ps1 | iex
```

Optional environment overrides:
- `SRV_VERSION=2.6.5` (pin a version; default is latest)
- `SRV_INSTALL_DIR=~/bin` (override location; default `~/.srv/bin` / `%USERPROFILE%\.srv\bin`)

After install, open a new terminal and run `srv install` to launch the browser-based wizard for Claude Code MCP registration and your first profile.

### Path B: Download a release archive

Pick the right one from the [Releases page](https://github.com/iccyuan/srv/releases/latest):

| OS / Arch | Archive |
|---|---|
| Linux x86_64 | `srv_<ver>_linux_x86_64.tar.gz` |
| Linux arm64 | `srv_<ver>_linux_arm64.tar.gz` |
| macOS Intel | `srv_<ver>_macos_x86_64.tar.gz` |
| macOS Apple Silicon | `srv_<ver>_macos_arm64.tar.gz` |
| Windows x86_64 | `srv_<ver>_windows_x86_64.zip` |

After extracting:

```sh
# Linux / macOS
tar -xzf srv_<ver>_linux_x86_64.tar.gz
./srv install       # opens browser wizard: PATH + Claude MCP + first profile
```

```powershell
# Windows
Expand-Archive srv_<ver>_windows_x86_64.zip
.\srv\srv.exe install
```

### Path C: Build from source (developers)

Requires Go 1.25+.

```sh
git clone https://github.com/iccyuan/srv && cd srv
go build -o srv.exe ./cmd/srv               # Windows
go build -o srv     ./cmd/srv               # macOS / Linux
.\install.ps1 -Gui                          # Windows GUI install
./install.sh --gui                          # macOS / Linux GUI install
```

`install.ps1` / `install.sh` are the source-build helpers (auto-detect the script's directory → add to PATH); both support `-Uninstall` / `--uninstall` for clean removal.

Verify with `srv version`, then run `srv install` to enter the GUI wizard.

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
srv config default <name>           # set the global default (persists in ~/.srv/config.json, all shells)
srv config default                  # TTY: arrow-key picker; non-TTY: print current default
srv config remove <name>            # delete a profile
srv config set <prof> <key> <val>   # set one key (true/false/int/null are auto-typed)
srv config edit [name]              # edit one profile as JSON in $EDITOR
srv env list                        # list profile-level remote env vars
srv env set KEY value               # inject KEY=value before remote commands
srv env unset KEY                   # remove one profile env var
srv env clear                       # drop all env vars for this profile
```

### Quick profile switching

**Two scopes — get them straight or you'll trip over yourself:**

| Command | Scope | Persistence |
|---|---|---|
| `srv use <name>` | **this shell session** (pinned in `~/.srv/sessions.json`) | Gone when this shell exits |
| `srv config default <name>` | **global** (writes `default_profile` in `~/.srv/config.json`) | Persists; shared by all shells |

`srv use` is a temporary switch; `config default` changes the fallback. They don't fight — a session pin always wins over the default.

```
srv use                # TTY: arrow-key picker (/ filter, Enter select, q cancel)
srv use <profile>      # pin directly
srv use --clear        # unpin
```

In the picker each row gets a marker: `[this shell]` (yellow, the current shell's pin) and `[default]` (cyan, the global default). Both can apply to the same profile.

`srv use` off a TTY (pipes / scripts / CI) keeps the original behavior: prints current pin / default / active.

**Resolution order** (highest first):

```
-P/--profile (per call)
  > srv use session pin
  > $SRV_PROFILE
  > srv config default global default
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
srv sync --delete --dry-run           # preview deletes for tracked files removed locally
srv sync --delete                     # also remove those tracked files on the remote
srv sync --delete --yes               # apply deletes above the default safety limit
srv sync --delete-limit 50            # change delete safety limit (default 20)
srv sync --exclude "*.log"            # extra exclude, repeatable
srv sync /opt/app                     # explicit remote root (default = sync_root or cwd)
srv sync --root ./subproject          # explicit local root (default = git toplevel / cwd)
srv sync --no-git                     # disable git auto-mode in a repo
srv sync --watch                      # keep watching and syncing changes
```

Default excludes: `.git`, `node_modules`, `__pycache__`, `.venv`, `venv`, `.idea`, `.vscode`, `.DS_Store`, `*.pyc`, `*.pyo`, `*.swp`. `list` mode (`--files`) skips default excludes — explicit user files are unconditionally sent.

Files are anchored at the git toplevel (git mode) or current dir (other modes); the remote receives them at `remote_root/<relative_path>`.

`--delete` currently works in git mode only. Use `--delete --dry-run` first: it prints `delete <path>` entries and does not touch the remote. Non-dry-run deletes are capped at 20 files by default; use `--yes` or `--delete-limit N` when the preview is expected.

### Live remote views

Five commands share one auto-reconnect engine (exponential backoff 1s -> 2s -> 4s, capped at 30s). Pick by source:

```
srv tail [-f] [-n N] [--grep RE] <path>...    # any remote file
srv journal [-u UNIT] [--since TIME] [-f]     # systemd unit log
srv logs <id> [-f]                            # detached-job output
srv watch [-n SECS] [--diff] <cmd>            # periodic in-place command
srv top [-n SECS]                             # streamed `top -b`
```

`tail` / `journal` / `top` survive SSH drops -- they reconnect and keep streaming. `watch --diff` highlights changed lines. `srv -t top` is the pty-in-place variant; `srv top` is the scrolling log variant.

### Parallel fan-out (profile groups)

Name a set of profiles, then fan a command out across them in parallel:

```
srv group set web web-1 web-2 web-3       # create / replace
srv group list                             # show all groups
srv group show web                         # show members
srv group remove web                       # delete

srv -G web "uptime"                        # parallel on all three
srv -G web "systemctl restart nginx"
```

Output is one section per profile with a `N succeeded, M failed.` summary line. The shell exit code is the max non-zero across members; dial failures surface as 255 to distinguish them from a real `exit 1` from the command. The MCP equivalent is the `run_group` tool.

### Remote sudo

```
srv sudo systemctl restart nginx     # prompts for password locally (no echo)
srv sudo apt update                  # reuses cached password within ~5 min
srv sudo --no-cache <cmd>            # always prompt, never store
srv sudo --cache-ttl 10m <cmd>       # custom TTL (daemon caps at 60min)
srv sudo --clear-cache               # drop the cached entry now
```

Password is read with `term.ReadPassword` (no echo, no shell history) and piped to remote `sudo -S`. Cache lives in daemon process memory only -- never written to disk; auto-evicted on exit 1 (likely auth failure).

### State dashboard (srv ui)

```
srv ui            # one-screen dashboard; auto-refreshes on data change (no flicker)
```

Shows active profile / cwd / project pin, daemon status, saved tunnels (running ones in yellow), the last 5 MCP tool calls, detached jobs, and recent sessions. `q` to quit, `r` to force a redraw.

### Per-project pin (.srv-project)

Drop a `.srv-project` JSON file at a repo root:

```json
{ "profile": "prod-db", "cwd": "/srv/app" }
```

Any `srv` invocation from that directory or any subdirectory auto-pins the profile and remote cwd, so you don't have to `srv use` each new shell or MCP session. Particularly valuable for MCP: each Claude Code project lands directly on the right profile instead of falling back to the global default.

Profile precedence: `-P` > `srv use` > `$SRV_PROFILE` > **`.srv-project`** > global default.
Cwd precedence: session cwd > `$SRV_CWD` > **`.srv-project.cwd`** > `profile.default_cwd`.

`srv project` prints the currently resolved pin.

### Port forwarding (`srv tunnel`)

`ssh -L` / `ssh -R` equivalent with two modes: one-shot foreground or saved daemon-hosted persistent.

```
# one-shot foreground (Ctrl-C to stop)
srv tunnel 8080            # local 127.0.0.1:8080  ->  remote 127.0.0.1:8080
srv tunnel 8080:9090       # local 127.0.0.1:8080  ->  remote 127.0.0.1:9090
srv tunnel 8080:db:5432    # local 127.0.0.1:8080  ->  db:5432 (resolved on the remote)
srv tunnel -R 9000:3000    # remote 127.0.0.1:9000 -> local 127.0.0.1:3000

# named + persistent (daemon-hosted; survives CLI exit)
srv tunnel add db -L 5432:db.internal:5432 -P prod --autostart
srv tunnel up db                   # start (daemon must be running)
srv tunnel down db                 # stop
srv tunnel list                    # saved tunnels + live status
srv tunnel show db                 # one tunnel's full definition
srv tunnel remove db               # delete the saved definition
```

`--autostart` brings the tunnel up automatically when the daemon starts. When the daemon exits all tunnels stop; bringing the daemon back up + autostart restores them.

Behavior (one-shot): `Ctrl-C` stops it; if the SSH connection drops, the tunnel notices and stops too. Each incoming connection runs in its own goroutine (bidirectional `io.Copy`). The local side binds `127.0.0.1` only -- not exposed to the LAN.

### Edit a remote file locally (`srv edit`)

```
srv edit /etc/nginx/conf.d/api.conf      # pull -> $EDITOR -> push back if changed
```

Flow: SFTP-pull into an `os.MkdirTemp` directory (basename preserved so editor syntax detection works) → spawn `$VISUAL` / `$EDITOR` (split on whitespace, so `EDITOR='code --wait'` works) → after editor exit, compare mtime+size: changed → upload back; unchanged → "no changes; not uploading".

Editor resolution: `$VISUAL` → `$EDITOR` → Windows: `notepad.exe` → otherwise: `vim` / `vi` / `nano`.

### Local helpers (`srv open`, `srv code`, `srv diff`, `srv doctor`)

```
srv doctor                         # local config / daemon / active-profile report
srv doctor --json                  # JSON diagnostics
srv open logs/app.log              # pull a remote file to a temp dir and open it
srv code /opt/app                  # open VS Code Remote SSH for a remote folder
srv diff ./app.py app.py           # compare local file with remote file
srv diff --changed                 # compare changed git files with remote counterparts
```

`srv open` is read-only; use `srv edit` when you want to save changes back. `srv code` runs `code --folder-uri ...` when the VS Code CLI is available, otherwise it prints the command to run.

**Known caveats**:

- **No locking**. Before save-back, `srv edit` checks the remote size/mtime and refuses to overwrite when another session changed it while your editor was open. For heavily shared files, SSH in and use vim directly.
- **VS Code requires `--wait`**. `EDITOR=code` returns immediately, so srv sees "no changes" and exits while the editor is still open. Set `EDITOR='code --wait'` instead.
- **Notepad converts LF → CRLF** on Windows, which makes the entire file look modified. Set `$EDITOR` to vim, notepad++, or `code --wait` instead.

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

The user command is base64-encoded into the spawn line, sidestepping any nested-quoting problems. The wrapper additionally writes the user command's exit code to `~/.srv-jobs/<id>.exit` when it finishes (consumed by the MCP `wait_job` tool below).

#### MCP long-task pattern: `detach` + `wait_job`

MCP is synchronous JSON-RPC: a blocking `run` call holds the model's whole turn, and Claude Code's per-tool timeout (default 60000ms / 60s, tunable via `MCP_TOOL_TIMEOUT`) kills any `run` that exceeds it -- the UI shows a red dot and the next call respawns the MCP child. **For any command expected to take longer than ~10s, use `run` with `background=true` or call `detach` directly, then poll with `wait_job`**:

```
run { command: "npm run build", background: true }  -> job_id (returns sub-second)
wait_job { id, max_wait_seconds: 8 }                -> status=running   (job still going; call again)
wait_job { id, max_wait_seconds: 8 }                -> status=completed exit_code=0 + log tail
                                         (local jobs.json auto-pruned on completion)
```

The `wait_job` wait loop runs **on the remote** as a single SSH round-trip -- a small bash for-loop polls the `.exit` marker and the PID, prints `STATUS=...` plus the log tail when it resolves. `max_wait_seconds` defaults to 8 and is hard-capped at 15 so Claude Code stays responsive instead of sitting in one long tool call. Status values:

- `completed` — `.exit` file written, `exit_code` populated, local job record removed
- `running` — still alive after the wait window; call `wait_job` again to keep waiting
- `killed` — PID gone without an `.exit` marker (someone SIGKILLed it externally)

Between wait_job calls the model can interleave other tool calls.

### Sessions

```
srv sessions          # list all session records (alive/dead)
srv sessions show     # full JSON for current session
srv sessions clear    # drop current session record
srv sessions prune    # GC: remove records whose pid no longer exists
```

### Prune accumulated caches

`srv prune <target>` keeps the live/recent part and drops only the stale part — full erasure is a different verb (e.g. `srv stats --clear` / `srv sessions clear`). The target is tab-completable.

```
srv prune jobs              # drop FINISHED records from the local jobs.json ledger (running kept)
srv prune jobs <id>         # only that id's finished record (errors if it's still running)
srv prune sessions          # drop DEAD-pid session records (alias of `srv sessions prune`)
srv prune mcp-log           # trim mcp.log to its recent ~256 KB tail (line-aligned)
srv prune mcp-stats         # drop mcp-stats.jsonl rows older than 7d (incl. the rotated .1)
srv prune all               # all four above, in one pass
srv prune jobs --remote     # ALSO delete COMPLETED jobs' ~/.srv-jobs/*.log + *.exit on the
srv prune all  --remote     # server, gated on the remote .exit marker so running jobs are
                            # never touched. Explicit opt-in, never implied; jobs/all only.
```

### Daemon management

`srv` auto-spawns a daemon (`~/.srv/daemon.sock`) the first time it needs `_ls`, a non-TTY command, or `cd` — pooling SSH connections so subsequent calls skip the ~2.7s handshake. You usually don't touch it; for direct control:

```
srv daemon                          # run in foreground (mainly for debugging)
srv daemon status                   # show pooled profiles / uptime (human-readable)
srv daemon status --json            # same, machine-readable JSON
srv daemon restart                  # stop and respawn in the background
srv daemon stop                     # stop
srv daemon logs                     # cat the auto-spawned daemon's stdout/stderr log (~/.srv/daemon.log)
srv daemon prune-cache              # drop the _ls remote-completion cache (~/.srv/cache/)
```

Socket lives at `~/.srv/daemon.sock` (AF_UNIX on Windows too — needs Win10 1803+). The daemon self-exits after 30 minutes idle; per-profile SSH connections are reaped after 10 minutes of disuse.

### Shell completion (tab completion)

**PowerShell** (persistent — added to `$PROFILE`, picked up by every new shell):

```powershell
# Append once; new PowerShell sessions load it automatically
"`n# srv tab completion`nsrv completion powershell | Out-String | Invoke-Expression" |
    Add-Content $PROFILE
```

Current-session only:

```powershell
srv completion powershell | Out-String | Invoke-Expression
```

**bash** (persistent via `~/.bashrc`):

```sh
echo 'source <(srv completion bash)' >> ~/.bashrc
```

**zsh** (same idea, `~/.zshrc`):

```sh
echo 'source <(srv completion zsh)' >> ~/.zshrc
```

**What it completes**:

| Input | Completion |
|---|---|
| `srv <TAB>` | all subcommands |
| `srv c<TAB>` | prefix-filtered (config/cd/check/completion) |
| `srv config <TAB>` | list/use/remove/show/set |
| `srv config default\|remove\|show\|edit <TAB>` | configured profile names |
| `srv use <TAB>` | profile names + `--clear` |
| `srv -P <TAB>` | profile names |
| `srv sessions <TAB>` | list/show/clear/prune |
| `srv prune <TAB>` | jobs/sessions/mcp-log/mcp-stats/all |
| `srv completion <TAB>` | bash/zsh/powershell |
| `srv push <TAB>` | local files |
| `srv push <local> <TAB>` | **remote** dirs / files |
| `srv cd <TAB>` / `srv cd /opt/<TAB>` | **remote dirs only** |
| `srv pull <TAB>` / `srv pull /etc/<TAB>` | **remote** dirs / files |
| `srv edit <TAB>` / `srv edit /etc/<TAB>` | **remote** dirs / files |

**Remote completion** uses an internal `srv _ls <prefix>` that runs `ls -1Ap` on the remote and caches results in `~/.srv/cache/` (5-second TTL). The first tab on a fresh prefix pays one full SSH handshake (~2-3 s); subsequent tabs hit the cache (~60 ms). Each invocation respects the current `cwd` and pinned profile, so remote completion follows `srv use` automatically.

The PowerShell script bakes in `srv.exe`'s absolute path (since the ArgumentCompleter scope doesn't always inherit PATH), so profile-name and remote lookups work from any directory.

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
| `keepalive_interval` | 30 | SSH-level keepalive probe interval (seconds); drop to 10-15 on flaky networks |
| `keepalive_count` | 3 | Probes that must fail before declaring the link dead |
| `dial_attempts` | 1 | Number of times to retry the initial TCP dial / SSH handshake on transient failure (2.6.4+); set 3-5 on flaky networks. Auth / host-key errors never retry |
| `dial_backoff` | `500ms` | Initial wait between dial retries; doubles each attempt up to 30s. `time.ParseDuration` format |
| `control_persist` | `10m` | How long ControlMaster sockets linger idle |
| `sync_root` | null | Default remote root for `srv sync` (used when no positional arg given) |
| `sync_exclude` | `[]` | Profile-level extra excludes for `srv sync`, merged with defaults |
| `compress_sync` | true | Gzip the `srv sync` tar stream (~70% smaller for code/text; ms-level CPU) |
| `env` | `{}` | Profile-level environment variables, prepended to every remote command and detached job (managed via `srv env ...`) |
| `jump` | `[]` | ProxyJump bastion chain. Each entry `[user@]host[:port]`, dialed in array order before the final target |
| `ssh_options` | `[]` | Raw `-o` strings, appended **last** (overrides everything above) |

---

## Multi-server, multi-terminal

### Model

- **Profile** = one server (host + user + port + key + default_cwd, etc.)
- **Session** = one shell instance. Session id = the shell process's PID. On Windows, intermediate shim layers (e.g. `cmd.exe`) are skipped automatically to find the real shell
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

Four layers of protection against bad networks (high ping, packet loss, NAT idle-kill), kernel up to protocol:

| Layer | Default | Tunable | What it does |
|---|---|---|---|
| **TCP keepalive (OS)** | always on, 15 s probes | not configurable, no downside | NAT/firewall-killed dead conns surface as EOF in seconds, not as a hung write |
| **SSH keepalive (app)** | every 30 s, 3 missed = dead (~90 s total) | profile `keepalive_interval` / `keepalive_count` | Active probing keeps the SSH channel alive when intermediate hops drop packets, and prevents server-side idle kills |
| **Daemon connection pool** | auto-spawned, exits after 30 min idle | nothing to tune | One handshake, many calls — sidesteps the ~2.7 s cold-handshake cost |
| **Dial retry** (2.6.4+) | off (`dial_attempts=1`) | profile `dial_attempts` / `dial_backoff` | Auto-retry when the first SYN drops or hits an RST. Heals ~90% of "first call failed on flaky link" cases. Auth / host-key errors never retry. |

**Tuning recipe for "high ping ~250 ms, occasional loss"**:

```sh
srv config set <profile> keepalive_interval 15      # tighter SSH probes
srv config set <profile> keepalive_count 4          # tolerate one extra hiccup
srv config set <profile> dial_attempts 4            # try up to 4 dials
srv config set <profile> dial_backoff 800ms         # 800ms / 1.6s / 3.2s / 6.4s, capped at 30s
srv config set <profile> connect_timeout 20         # bigger per-attempt budget
```

**Measure the link** — when you don't know whether srv is slow or the network is:

```
srv check --rtt                  # 10 SSH-level RTT samples; min/med/avg/max + loss%
srv check --rtt --count 30       # longer sample
srv check --rtt --interval 50ms  # tighter spacing for jitter view
```

The trailing `verdict:` line tags the link as `healthy` / `high latency` / `noticeable jitter` / `packet loss is high`, pointing you at the right knob.

**Resumable transfers** - when `srv push` / `srv pull` of a large file is interrupted, 2.6.4+ resumes from the partial offset on the next try (single-file granularity; works inside directory recursion too). Before appending, srv verifies that the existing destination bytes exactly match the source prefix; mismatched partials are overwritten from scratch. If source and destination have the same size and match byte-for-byte, the transfer is skipped.

**`srv -d` is the truly disconnect-proof option** — backgrounded jobs run under `nohup` with output to `~/.srv-jobs/<id>.log`, so a local connection drop doesn't kill the remote process. Reattach with `srv logs <id> -f` / `srv kill <id>`.

---

## Claude Code / Codex integration

### Option 1: Bash invocation

If `srv` is on PATH, you're done — both clients can call it as a regular command.

```
srv ls /opt
srv -d "python long.py"
```

### Option 2: MCP server (structured tools)

Claude Code gets the following tools via stdio MCP, grouped by purpose. Each Claude Code instance gets its own session id (= the MCP server process PID), so multiple conversations don't share state.

| Category | Tools |
|---|---|
| Profile / session | `use` `cd` `pwd` `status` `list_profiles` |
| Diagnostics | `check` `doctor` `daemon_status` `list_dir` |
| Run | `run` `run_stream` `run_group` `detach` `wait_job` `kill_job` `list_jobs` |
| Log viewing | `tail` `journal` `tail_log` |
| Env / transfer | `env` `diff` `push` `pull` `sync` `sync_delete_dry_run` |

`detach` + `wait_job` is the recommended pattern for long-running commands -- see [Detached jobs](#detached-jobs) above. `list_dir` lets the model enumerate remote directories structurally without burning tokens on `run "ls ..."` and inheriting its ANSI noise.

#### MCP token-economy gates

To stop "the model sends one `cat /var/log/syslog` and burns a six-figure token bill" outcomes, a handful of tools enforce hard preconditions before any remote work happens. Rejected calls return `IsError=true` with a `rejected_reason` field the client can branch on; the message text includes a ready-to-copy example for the model.

| Tool | Trigger | Required to proceed |
|---|---|---|
| `tail` | `follow_seconds > 0` (any value) | `grep` non-empty (not `.*` / `.+` -- those are detected as bypasses) |
| `journal` | `follow_seconds > 0` (any value) | at least one of `unit` / `since` / `priority` / `grep` |
| `run` | `cat <file>`, bare `dmesg`, unfiltered `journalctl`, `find /...` | a downstream limiter (`\| head`, `\| tail`, `\| grep`, `\| wc`, ...) OR use the dedicated tool (`tail` / `journal`) |
| `run_stream` | Same rules as `run` | Same |
| `lines` clamps | -- | `tail` capped at 1000, `journal` at 2000, `follow_seconds` at 60 |

These gates only apply on the MCP path; the CLI counterparts (`srv tail`, `srv journal`, `srv run`) are unconstrained -- a human paying bandwidth and scroll-time isn't the same problem as a model paying tokens.

**Claude Code** — pick one of three scopes depending on how you want it shared:

| Scope | Written to | Use case |
|---|---|---|
| `user` | `~/.claude.json` | Available in every project, **recommended for personal use** |
| `project` | `<repo>/.mcp.json` | **Shared with teammates** — commit and they get it on clone |
| `local` | per-project user file | Only in this project, only for you, not committed |

```sh
# 1) personal global (works in any directory)
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp

# 2) project-shared (run from repo root; writes .mcp.json, commit it)
cd <your-project>
claude mcp add srv --scope project -- D:\WorkSpace\server\srv\srv.exe mcp

# 3) project-private (not in .mcp.json, only you see it)
cd <your-project>
claude mcp add srv --scope local -- D:\WorkSpace\server\srv\srv.exe mcp

# verify (works for any scope)
claude mcp list   # should show  srv: ✓ Connected
```

> On macOS / Linux replace the path with `/path/to/srv/srv` (no `.exe`). Or just `srv mcp` if `srv` is on PATH — that's the cleanest form.

New Claude Code sessions pick it up automatically; existing sessions need `/mcp` to reconnect.

**Codex CLI** — `~/.codex/config.toml`:

```toml
[mcp_servers.srv]
command = "D:\\WorkSpace\\server\\srv\\srv.exe"
args = ["mcp"]
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
| `SRV_CWD` | Fallback cwd when no session cwd is set (2.6.2). In MCP registrations, set `"env": {"SRV_CWD": "/mnt/project/foo"}` so each new MCP session lands directly in the project directory instead of `~`. Priority: session pin > `$SRV_CWD` > `profile.default_cwd`. |
| `SRV_LANG` | UI language (`en` / `zh` / `auto`); overrides system-locale detection. Lower priority than `lang` in config. Default: `auto`. |
| `SRV_HINTS` | `0` / `false` / `off` disables typo hints. Higher priority than config or `--no-hints` flag. |
| `SRV_GUARD` | Force-overrides the MCP high-risk guard: `1`/`true`/`on`/`yes` forces it on, `0`/`false`/`off`/`no` forces it off. Highest precedence (beats per-session and global settings). When unset, the guard is on by default. |
| `SRV_ALLOW_AI_CLI` | `1` / `true` / `on` / `yes` lifts the AI-agent CLI block. By default, when an AI coding-agent shell is detected (`CLAUDECODE` / `CLAUDE_CODE_ENTRYPOINT` / Codex `CODEX_*` markers), srv **hard-refuses** every remote-touching subcommand (run/push/pull/sync/edit/diff/tail/watch/journal/top/sudo/shell/logs/kill/tunnel/recipe/ui and the implicit `srv <cmd>` remote run) and points at the MCP server instead (the MCP path is unaffected and keeps its token / destructive-command gates). Set this for a human working in an agent terminal who really wants the raw CLI. |

---

## Command typo hints (2.6.5+)

When you mistype a local subcommand, `srv` prints a single stderr line nudging you toward the right one — and still runs the command on the remote (you might genuinely want a remote command with that name). Two trigger points:

```
$ srv staus -la
srv: hint: "staus" looks like the local subcommand "status". Running on remote anyway.
ls: cannot access 'staus': ...

$ srv pwd2
bash: pwd2: command not found
srv: hint: "pwd2" isn't installed on the remote and looks like "pwd" (a local subcommand). Try: srv pwd
exit 127
```

**To disable** (any of these — env wins over flag wins over config):

```sh
SRV_HINTS=0 srv staus            # this shell, ad-hoc
srv --no-hints staus             # one call
srv settings hints false         # permanent (writes ~/.srv/config.json)
srv settings hints --clear       # back to default (on)
```

The MCP path never fires hints — keeps Claude Code's tool stderr clean.

## UI language

`srv help` and high-traffic error / usage strings come in English and Chinese. The active language is **auto-detected from system locale** (`zh*` → Chinese, anything else → English).

**Force it**:

```sh
srv settings lang zh             # pin Chinese
srv settings lang en             # pin English
srv settings lang auto           # back to auto-detect
SRV_LANG=zh srv help             # ad-hoc
```

Technical outputs (`srv check` diagnoses, `srv doctor`, daemon protocol fields, MCP tool responses) stay English on purpose so terminology doesn't drift and grepping stays predictable.

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
- Calling `srv.exe` directly from PATH should be stable
- For unusual shell nesting, set `$env:SRV_SESSION = $PID` manually

### `srv -d` process exits immediately
- The remote needs `bash`, `base64`, `nohup` (coreutils, virtually always present)
- Check `srv logs <id>` for the remote stderr

### Claude Code doesn't see new MCP tools
The MCP server is loaded at session startup. Open a **new** Claude Code session, or `/mcp` to reconnect.

### MCP `run` returns `-32700 parse error` on a complex command
**Client-side JSON encoding issue** — the combination of deeply nested shell substitution + non-ASCII (e.g. CJK) + multi-layer quoting makes Claude Code's tool-call JSON malformed. Not a srv bug. Workarounds:

1. Split the command into multiple steps, one quoting layer each
2. `export VAR=...` first, then reference `$VAR` to flatten the inner literal
3. Push a script and run it: `srv push script.sh /tmp/ && srv "bash /tmp/script.sh"`

### MCP `run` with a heredoc fails with `parse error near '\n'`
**Fixed in 2.6.2**. `wrapWithCwd` now puts a newline before the closing `)` of the subshell, so the heredoc terminator stays on its own line instead of getting fused with `)` into `EOF)`. Upgrade to 2.6.2.

### MCP always starts at `~`; need to `srv cd` every session
**Fixed in 2.6.2** via `$SRV_CWD`, which takes priority over `profile.default_cwd`. In the per-project mcpServers block:

```json
"srv": {
  "type": "stdio",
  "command": "D:\\WorkSpace\\server\\srv\\srv.exe",
  "args": ["mcp"],
  "env": { "SRV_CWD": "/mnt/project/alpha-bot" }
}
```

### MCP hangs / returns EOF on the first call after a long idle
**Mitigated in 2.6.2**: the daemon now health-checks pooled SSH connections idle longer than 30 s with one keepalive ping; on failure it evicts and re-dials. Calls succeed on the second attempt before this fix.

### MCP `run` chain like `token=$(login) && curl ...` reports exit 0 even when login failed
**Bash semantics**, not a srv bug. `$(...)` failures don't propagate, and `curl -s` returns 0 on HTTP errors too — so the final exit code is curl's 0. Three fixes:

1. **Split into two `srv run` calls** — a login failure surfaces as its own non-zero exit, the chain breaks at step 1
2. **Use strict mode**: `srv "set -euo pipefail; token=\$(login) && curl ..."` — any sub-command failing aborts the chain
3. **Use `curl -fsS`** instead of `-s` — `-f` makes curl exit non-zero on HTTP errors

### MCP `run` inline backgrounding (`& disown` / `nohup &`) doesn't actually start the process
**Use `srv -d` instead.** `srv -d <cmd>` is the dedicated path for backgrounded jobs: it does `nohup`, redirects stdout/stderr to `~/.srv-jobs/<id>.log`, and records the PID. Inline `&` over a non-TTY SSH session has races (channel close → SIGHUP / stdout blocking) — that's SSH + shell behavior, not a srv bug.

```
srv -d ./svc                   # start in background, returns a job id
srv jobs                       # list running jobs
srv logs <id> -f               # tail the remote log
srv kill <id>                  # SIGTERM
```

### MCP `psql -c 'SELECT a; SELECT b;'` only returns the last result
**psql behavior, not srv**. `-c` only returns the last statement's result set. Workarounds:

- `DO` block with `RAISE NOTICE` to surface intermediate values
- Write the SQL to a file and run with `psql -f /tmp/multi.sql` (after `srv push`)
- Issue separate `psql -c` calls

### MCP occasionally drops characters in long output (CJK / commit hashes)
**No clean root cause** — sporadic, hard to repro. Workaround: `srv "cmd > /tmp/out.txt"` then `srv pull /tmp/out.txt` or `srv "head -n 100 /tmp/out.txt"`.

### `srv config set` change doesn't seem to apply
- Inspect `~/.srv/config.json` to confirm the value landed on the right profile
- A higher-priority `-P` flag, session pin, or `SRV_PROFILE` may be overriding

---

## Design tradeoffs / known limitations

- **Non-interactive ssh doesn't source `.bashrc`**: aliases / PATH from rc files aren't visible by default. `srv "bash -ic '<cmd>'"` forces an interactive shell.
- **Mid-transfer disconnects**: `srv push` / `srv pull` resume safely after verifying the partial file prefix. `srv sync` still streams a tar archive and does not resume mid-stream.
- **Long ssh commands die on disconnect**: only `srv -d` survives a network interruption.
- **ControlMaster compatibility**: Windows OpenSSH 9.5+ has full support. Older versions may need `multiplex=false`.
- **Session id can mis-detect under unusual shell nesting**: use `SRV_SESSION` to pin.
- **Single cwd per (session, profile)**: no `pushd`/`popd`-style stack.

---

## Further reading

- [README.md](./README.md) — Chinese version
- [CHANGELOG.md](./CHANGELOG.md) — version history
- [docs/ARCHITECTURE.en.md](./docs/ARCHITECTURE.en.md) — architecture + the "why it's built this way" technical reference ([中文](./docs/ARCHITECTURE.md))

---

## Version

Currently **Go 2.6.x** (what `srv version` prints). Version bumps on breaking changes. Full history in [CHANGELOG.md](./CHANGELOG.md).

## Development (contributors)

The repo ships a pre-commit hook at `.githooks/pre-commit` that runs `gofmt -l` + `go vet` and rejects unformatted / unsafe code (bypass with `--no-verify` when you really mean it). Activate **once after cloning**:

```sh
git config core.hooksPath .githooks
```

Each `git commit` then runs the checks automatically. The hook only fires when `go/*.go` files are staged, so doc-only commits stay fast.

## Releasing (maintainers)

Releases are driven by GitHub Actions + goreleaser. Push a `vX.Y.Z` tag and binaries for 5 OS/arch combos plus checksums get published as a GitHub Release.

```sh
# 1) Update CHANGELOG.md (new entry at top), commit
# 2) Tag and push
git tag v2.4.2
git push origin v2.4.2
```

The release workflow:
- Cross-compiles linux/darwin/windows × amd64/arm64 (5 binaries -- windows-arm64 skipped)
- Embeds the tag into `srv version` via `-ldflags -X main.Version=`
- Packages each binary as `srv_<ver>_<os>_<arch>.tar.gz` (or `.zip` on Windows) alongside LICENSE, READMEs, CHANGELOG
- Emits `checksums.txt` (SHA256)
- Publishes to https://github.com/iccyuan/srv/releases

Local dry-run (no upload):

```sh
# Install goreleaser first: https://goreleaser.com/install/
goreleaser release --snapshot --clean --skip=publish
# Artifacts land in ./dist/
```
