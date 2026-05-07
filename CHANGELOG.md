# Changelog

格式参考 [Keep a Changelog](https://keepachangelog.com)。版本号在破坏性变更时增加。

**维护状态(2026-05-07 起)**:

- **Go 实现** (`go/`,**默认 `srv` 入口**):正常维护,接收新功能和 bug 修复
- **Python 实现** (`python/`):**已冻结在 0.7.5**,不再接收功能或修复;仍可显式 `python python/srv.py ...` 调用,行为对齐 Go 2.0.1。后续可能从仓库移除,但 git 历史保留

两版共享 `~/.srv/{config,sessions,jobs}.json`,迁移期间可来回切换。

---

## 维护策略变更 — 2026-05-07

冻结 Python 实现,后续单走 Go。原因:

1. Python 版本曾深度依赖系统 ssh/scp,在 Windows 上累积了一系列 OpenSSH 9.5p2 quirks 的 workaround;Go 版本用 `crypto/ssh` 直接做协议,这些坑根本不存在。
2. 同时维护两份实现的迁移摩擦 > 收益(Python 的优势是"不用编译",但用户已经有 Go 工具链)。
3. Go 二进制 8MB 单文件,部署体验比"Python + 系统 ssh + 各种 shim"明显更好。

Python 版本最后一次有意义的迭代是 0.7.5(MCP 加固 + ControlMaster 修复 + tilde 引号 fix),与 Go 2.0.1 行为对齐;之后任何新坑只在 Go 侧修。

---

## [Go 2.0.2] — 2026-05-07

### Fixed
- **PowerShell tab 补全**实际上不工作,踩了三个坑全修了:
  1. **单元素管道收缩成标量** —— `Where-Object` 过滤后只剩一个 token 时,PowerShell 会把数组拆掉,后续 `foreach` 按字符迭代。`@(...)` 强制装箱。
  2. **profile 名拼接 `--clear` 变字符串相加** —— `(@profiles + '--clear')` 同样的问题,需要 `@(@profiles) + '--clear'`。
  3. **ArgumentCompleter 作用域 PATH 不可见** —— 完成器内 `& srv _profiles` 找不到 srv 命令。修法:`srv completion powershell` 输出时**烧入 srv.exe 的绝对路径**(`os.Executable()`)。bash/zsh 不受影响(用户能跑 `srv completion bash` 就说明 srv 在 PATH 上)。
- 同时补齐:`srv config use|remove|show <TAB>` 现在也补 profile 名(原先只补 list/use/remove/show/set);`srv push <TAB>` 补本地文件。

### Notes
README 加了 PowerShell `$PROFILE` 的永久安装一行命令,以及详细的"什么场景补什么"对照表。

---

## [Go 2.0.1] — 2026-05-07

### Changed
- **`src/` 重命名为 `python/`** —— 跟 `go/` 命名对称,标识"两个实现并列"。
- **Go 二进制成为默认 `srv` 入口** —— 编译目标改为仓库根 (`../srv.exe` / `../srv`)。原先在仓库根的 Python shim(`srv.cmd` / `srv`)删除。
- 用户 PATH 不变(仍指仓库根),但 `srv` 现在直接是 Go 二进制 —— 启动 <10ms,无 Python 依赖。
- 文档一律推荐:`claude mcp add srv -- D:\...\srv\srv.exe mcp`(无 `python ...` 中转层)。
- Python 实现仍可显式调用:`python python/srv.py ...`。两版共享 `~/.srv/{config,sessions,jobs}.json`。

### Fixed
- `RunCaptureResult` 加 JSON 标签(`stdout` / `stderr` / `exit_code` / `cwd`),MCP `run` 的 `structuredContent` 现在与 Python 版字段名一致。
- 新增 `shQuotePath` 保留 `~`/`~/` 前缀的远端 shell 展开;`wrapWithCwd` / `RunDetached` / `changeRemoteCwd` / `tarUploadStream` 全部使用,修复了 cwd=`~` 时 `cd '~'` 不展开导致的 exit 1 + 空 stdout。

---

## [Go 2.0.0] — 2026-05-07

完整的 Go 重写,放在 [`go/`](./go) 子目录,**Python 版本继续保留**在 `src/`。

### 用 Go 解决了什么

Python 版本因为包装系统 `ssh.exe`,在 Windows 上累积了一连串 OpenSSH 9.5p2 的 quirk(ControlMaster 把 stdout 管道锁住、stdin 默认转发要按两次 Enter、`getsockname failed: Not a socket`、`Read from remote host: Unknown error` 等)。Go 版用 `golang.org/x/crypto/ssh` 自实现 SSH 协议,**彻底绕开这些坑**;同时编译成单二进制,部署等于"复制文件"。

### 包含的所有功能(对齐 Python 0.7.5,无遗漏)

- 全部 18 个子命令:`init` / `config <list|use|remove|show|set>` / `use` / `cd` / `pwd` / `status` / `check` / `run`(默认)/ `exec` / `push` / `pull` / `sync` / `jobs` / `logs` / `kill` / `sessions <list|show|clear|prune>` / `completion` / `mcp` / `_profiles`(内部)/ `help` / `version`
- 全部 3 个全局 flag:`-P/--profile`、`-t/--tty`、`-d/--detach`
- 所有 profile 键(`multiplex` 和 `control_persist` 在 Go 版无操作—用进程内连接代替 ControlMaster)
- 完整的 session 模型:Windows 进程树游走跳过 `cmd.exe`/`python.exe` 中间层,Unix 用 `os.Getppid()`,`SRV_SESSION` 显式覆盖
- 完整的 sync 模式:git(all/staged/modified/untracked)/ mtime / glob / list,加 `--exclude`、`--root`、`--no-git`、`--dry-run`、`sync_root`、`sync_exclude`、11 项默认排除
- 完整的 14 个 MCP 工具:`run` / `cd` / `pwd` / `use` / `status` / `check` / `list_profiles` / `push` / `pull` / `sync` / `detach` / `list_jobs` / `tail_log` / `kill_job`,协议 2024-11-05
- check 的 9 类诊断 + 针对性修复建议
- 后台作业:nohup + base64 编码避免引号问题,jobs.json 索引,前缀匹配
- shell 补全:bash / zsh / powershell

### 实现要点

- `crypto/ssh` 主连接 + `pkg/sftp` 文件传输 + 内置 `archive/tar` 自打包(sync 不再调用本地 tar)
- 已知 hosts:`golang.org/x/crypto/ssh/knownhosts` accept-new
- 认证:ssh-agent → profile.identity_file → 默认 `~/.ssh/id_ed25519` / `id_rsa` / `id_ecdsa`,passphrase 交互
- 跨平台编译:`GOOS=linux/darwin/windows go build -o srv .`
- 与 Python 版**共享 `~/.srv/{config,sessions,jobs}.json`**,两版可任意切换

详见 [`go/README.md`](./go/README.md)。

---

## [0.7.5] — 2026-05-06

### Fixed
**Windows 上 `srv ls` / `srv "echo hi"` 等流式命令"要回车两次"才返回**:不是终端 buffer 问题,是 ssh 客户端默认会**把本地 stdin 转发给远端命令的 stdin**。远端 `ls` 早早执行完了,但本地 ssh 还在轮询 stdin,直到用户按 Enter 触发 broken-pipe 检测才肯退。

修法:`build_ssh_cmd` 在 `tty=False` 且 `sys.stdin.isatty()` 为真时追加 `-n`,告诉 ssh 不读 stdin(等价于 stdin 接 /dev/null)。如果 stdin 是管道(`cat foo | srv "wc -l"`),isatty=False,不加 `-n`,管道数据照常转发到远端。

### Changed
n/a

---

## [0.7.4] — 2026-05-06

### Fixed
**Windows 上 `srv cd` / `srv ls` 等出现 "Read from remote host: Unknown error" / "getsockname failed: Not a socket" 的真凶** —— 不是 stdin、也不是远端,是 ControlMaster 多路复用在 Windows OpenSSH 9.5p2 + 某些远端组合下直接破:子 master 进程继承父 ssh 的 stdout 管道,父退后 master 还把管道占着,Python `communicate()` 读半关闭管道炸掉。直接在 PowerShell 里跑同样的 `ssh -o ControlMaster=auto ... root@host echo ok` 也会复现。

修法:`_default_ssh_options` 在两种情形下强制 `ControlMaster=no` + `ControlPath=none`:
- `capture=True`(stdio 是 PIPE)—— 一次性探测命令(cd / push / pull / MCP run / MCP probe)反正用不上 multiplex
- `sys.platform == "win32"`(任何模式)—— Windows 上索性不开,等以后 Win32-OpenSSH 自己修了再说;Linux / macOS 上 `profile.multiplex` 仍然按用户设置生效

代价:Windows 用户失去 multiplex 的握手加速。但稳定性 > 速度,且 Windows 用户可在 `profile.ssh_options` 里手工塞 ControlMaster 绕过(自担风险)。

---

## [0.7.3] — 2026-05-06

### Fixed
0.7.2 给 `_ssh_run` 一律加了 `stdin=subprocess.DEVNULL` 防止 MCP 模式下子 ssh 继承 JSON-RPC 管道。但**Windows OpenSSH 9.5p2 收到 NUL 设备作为 stdin 时会失败**,表现为 "Read from remote host: Unknown error" —— 这一下把 CLI 的 `srv cd` / `srv pwd`(走 capture 路径)搞挂了。

修法:`stdin=DEVNULL` 改成**仅 MCP 模式启用**(那里才真有 JSON-RPC 管道需要隔离)。CLI 模式让 stdin 继承父终端,与 `srv check` 走的逻辑一致。

---

## [0.7.2] — 2026-05-06

### Fixed
**MCP 工具调用 hang 死的根因**(用户反馈 `srv__cd` 长时间无响应):

- `_ssh_run` 之前用 `subprocess.run(cmd, capture_output=True, text=True)` —— **没指定 stdin**,子 ssh 进程继承了父进程(MCP server)的 stdin,即 Claude Code 来的 JSON-RPC 管道。一旦 ssh 想 prompt 任何东西(passphrase、host-key 二次验证、密码 fallback)就读 stdin,读到 JSON-RPC 字节,一直试一直读,**永远不退出**。修法:`stdin=subprocess.DEVNULL`。
- 同时给 `_ssh_run` 加 60s 硬超时,撞上就返回合成 `CompletedProcess(returncode=124)`,不再无限挂。
- `build_ssh_cmd` / `build_scp_cmd` 在 `capture=True` 时自动追加 `-o BatchMode=yes`(capture 模式 = 非交互上下文,prompt 永远不应该出现;有的话快速失败远好过 hang)。MCP push / pull handler 显式传 `capture=True`。

合并 0.7.1 的 4 处加固(UTF-8 stdio / BrokenPipe 兜底 / `_IN_MCP_MODE` 静默 stderr / readline 异常宽容),MCP server 现在应当不会再"很容易断"或"无响应"。

### Note
如果之前调试时把 `multiplex` 关了(`srv config set <prof> multiplex false`),建议改回:`srv config set <prof> multiplex true`。ControlMaster 让 ssh 复用一个已认证 socket,不每次重做握手,**根本上**避免 prompt 触发场景。

---

## [0.7.1] — 2026-05-06

### Fixed
MCP server stability hardening (用户反馈"很容易断"):
- 进入 `cmd_mcp` 后立刻 `sys.stdout/stdin.reconfigure(encoding="utf-8", errors="replace")`,避免 Windows 默认 cp1252 / cp936 编码下,非 ASCII payload(中文 profile 名 / 路径 / 远端 stderr)写 stdout 直接 `UnicodeEncodeError` 让进程崩。
- `_mcp_send` 包 `try / except (BrokenPipeError, OSError)`:客户端短暂关闭读端时不连累 server,readline 循环下一轮 EOF 自然退出。
- 引入 `_IN_MCP_MODE` 全局标志,该模式下 `_ssh_call` / `_ssh_run` 的握手重试提示不再写 stderr——某些 MCP 客户端会因 stderr 里的非 JSON 行判定服务异常。
- 主循环 `sys.stdin.readline()` 异常处理从 `KeyboardInterrupt` 扩展到 `OSError` / `UnicodeDecodeError`,管道异常状态下也优雅退出。

---

## [0.7.0] — 2026-05-06

### Added
- **`srv check`** —— 主动连通性诊断。用 `BatchMode=yes` + `StrictHostKeyChecking=accept-new` + 关闭 ControlMaster 起一条干净的探测连接,15 秒超时,**永不 hang**。失败时按 stderr 模式分类,给出对应修复命令:
  - `no-key`(`Permission denied (publickey`)→ 输出本机的 `ssh-copy-id` 命令和 PowerShell 的等价管道
  - `host-key-changed` → `ssh-keygen -R` + `ssh-keyscan`
  - `dns` / `refused` / `no-route` / `tcp-timeout` / `perm-denied` / `unknown` 各自有针对性提示
- `srv init` 末尾追加提示语,引导用户立刻跑 `srv check`。
- MCP 工具 `check`,返回 `{ok, diagnosis, advice, exit_code, stderr}`,Agent 客户端能自动分流处理。

### Fixed
- 改善了 SSH 配置错误的可发现性。原先用户没在服务器配 key 时,看到的只是模糊的 "Read from remote host..." 之类底层报错;现在 `srv check` 会明确告诉他们 "key 没加到 authorized_keys" 和怎么加。

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
