# Changelog

格式参考 [Keep a Changelog](https://keepachangelog.com)。版本号在破坏性变更时增加。

---

## [0.6.0] — 2026-05-06

### Added
- **批量同步** `srv sync`:把已变更文件按相对路径成批推到远端,通过 `tar -cf - | ssh remote tar -xf -` 单条 ssh 流式传输(配合 ControlMaster 几乎零握手开销)。
- 4 种文件选择模式:
  - **git**(默认,在 git 仓库内):走 `git ls-files --modified --others --exclude-standard` + `git diff --cached`,可用 `--all` / `--staged` / `--modified` / `--untracked` 限定范围
  - **mtime**:`--since 2h` / `30m` / `1d` / `90s`
  - **glob**:`--include "src/**/*.py"`(可重复)
  - **list**:`--files a.py --files b/c.py`,或 `-- a.py b.py` 之后所有当作文件
- `--dry-run` 预览将传的文件清单
- `--exclude PATTERN` 自定义排除(可重复),与默认排除合并
- `--root <dir>` 显式指定本地根,默认 = git 顶层 / 当前目录
- 默认排除:`.git`、`node_modules`、`__pycache__`、`.venv`、`venv`、`.idea`、`.vscode`、`.DS_Store`、`*.pyc`、`*.pyo`、`*.swp`(list 模式不应用,显式用户列表无条件传)
- profile 新键:`sync_root`(默认远端目标根)、`sync_exclude`(profile 级追加排除)
- MCP 工具 `sync`,接受同名参数,Claude Code / Codex 一键推

---

## [0.5.0] — 2026-05-06

### Added
- **每个 shell 一个 session**:cwd 状态按 `(session_id, profile)` 双键存,两个终端用同一个 profile 不再互相覆盖 `cd`。
- `srv use <profile>` / `srv use --clear` / `srv use` —— 当前 shell 的快速 profile 切换。
- `srv sessions [list|show|clear|prune]` —— 查看和管理 session 记录。
- MCP 工具 `use`,Claude Code / Codex 端同样能 pin profile。
- Windows session id 检测:跳过 `.cmd` shim 和 `python.exe` launcher 中间层,定位到真正的用户 shell PID。
- `$SRV_SESSION` 环境变量,显式指定 session(脚本/CI 跨多次调用共享状态用)。
- `srv config list` 用 `@` 标记当前 session pin 的 profile,`*` 仍代表全局默认。
- `srv status` 新增 `session :` 行和默认值汇总。

### Changed
- cwd 持久化从全局单值(`state.json`)改成按 session 分桶(`sessions.json`)。
- `state.json` 不再读写,旧数据会被忽略。

---

## [0.4.0] — 2026-05-06

### Added
- 网络弹性默认参数自动应用到每条 ssh/scp:`ControlMaster=auto`、`ConnectTimeout=10`、`ServerAliveInterval=30`、`ServerAliveCountMax=3`、`TCPKeepAlive=yes`、`Compression=yes`。
- 握手期失败自动重试(ssh exit 255 且 5 秒内退出),3 次,1s/2s 退避。
- 后台作业:`srv -d <cmd>` 起 nohup 进程并捕获日志;配套 `srv jobs`、`srv logs <id> [-f]`、`srv kill <id>`。
- MCP 工具:`detach`、`list_jobs`、`tail_log`、`kill_job`。
- Profile 可调键:`multiplex`、`compression`、`connect_timeout`、`keepalive_interval`、`keepalive_count`、`control_persist`。
- `jobs.json` 持久化作业索引。

### Fixed
- `srv config set` 现在能正确把 `true` / `false` 字符串解析成布尔值(以前存为字符串)。
- 移除一段死代码 `BatchMode=no` 始终被设的逻辑。

---

## [0.3.0] — 2026-05-06

### Changed
- 二进制改名 `servermake` → `srv`(同步改了配置目录、环境变量、MCP server 名、补全脚本里的标识符)。
- 配置目录:`~/.servermake/` → `~/.srv/`
- 环境变量:`SERVERMAKE_HOME` → `SRV_HOME`,`SERVERMAKE_PROFILE` → `SRV_PROFILE`

---

## [0.2.0] — 2026-05-06

### Added
- 文件传输:`srv push <local> [<remote>] [-r]` 和 `srv pull <remote> [<local>] [-r]`,走系统 `scp`,本地是目录时自动加 `-r`。
- shell 补全生成器:`srv completion <bash|zsh|powershell>` 把脚本写到 stdout。
- stdio MCP server(`srv mcp`,协议 2024-11-05),暴露 7 个工具:`run`、`cd`、`pwd`、`status`、`list_profiles`、`push`、`pull`。
- 内部命令 `srv _profiles`(每行一个 profile 名),供补全脚本使用。

---

## [0.1.0] — 2026-05-06

### Added
- 首次发布(原名 `servermake`)。
- 子命令:`init`、`config <list|use|remove|show|set>`、`cd`、`pwd`、`status`、`run`、`exec`。
- profile 配置存 `~/.servermake/config.json`,持久化全局 cwd 存 `state.json`。
- 全局 flag:`-P/--profile`、`-t/--tty`。
- Windows `.cmd` shim 和 POSIX bash shim,跨 shell 调用。
- profile 解析顺序:`-P` flag > `$SERVERMAKE_PROFILE` > config 默认。
- 默认子命令:首个非保留参数当作远端命令处理。
