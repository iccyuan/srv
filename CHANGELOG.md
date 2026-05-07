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

## [Go 2.4.0] — 2026-05-07

### Added
**daemon 协议加流式输出**(`stream_run` op),解决 v2.3.0 留下的"`tail -f` 走 daemon 会被 buffer 到命令结束才出现"的问题。

**协议**:CLI 发 `{"op":"stream_run",...}`,daemon 每收到 4 KB stdout/stderr 就发一帧:

```json
{"id":1,"k":"out","b":"<base64>"}      // stdout 块
{"id":1,"k":"err","b":"<base64>"}      // stderr 块
{"id":1,"k":"end","c":0}               // 命令退出码
{"id":1,"k":"fail","err":"reason"}     // 启动前失败(dial / 没 profile)
```

CLI 边收边解码写本地终端,无 buffer。

### Implementation notes
- `handleConn` 在写 `wr` 上加了 `wrMu`(stdout / stderr 两条转发 goroutine 并发写),非流式响应路径也用同一把锁。
- 转发 goroutine 写失败(client 断了,典型是用户 Ctrl+C)→ `sess.Close()`,远端命令收 SIGHUP,**不漏进程**。
- `tryDaemonRun` 移除,`tryDaemonStreamRun` 取代。`runRemoteStream(tty=false)` 走流式 daemon。
- 输出帧大小 4 KB(`bytes.Buffer` 默认),适配 ssh.Session 的 channel 流控,无需调参。
- base64 编码代价 ~33% 字节膨胀;over unix-socket 在本机,可忽略。

### Verified

```
$ srv 'for i in 1 2 3; do echo "remote:$(date +%T.%N) line $i"; sleep 1; done'
local-recv: 19:29:46.356 | remote:11:29:46.243663125 line 1
local-recv: 19:29:47.358 | remote:11:29:47.246116125 line 2
local-recv: 19:29:48.359 | remote:11:29:48.248527427 line 3
```

每行本地接收时间和远端 echo 时间一一对应,差 1 秒——证实是边写边收,不是 buffer 到末尾。

### Edge cases
- **stdin 转发暂未实现**:管道喂入(`cat foo | srv "wc -l"`)走 daemon 会丢掉本地 stdin。当前 `runRemoteStream` 只在 tty=false 时走 daemon;再加一条"stdin 是 TTY 时才走 daemon"判据可以更稳——但 `runRemoteStream` 调用方还没这个上下文。后续如果用户报问题再补。
- **Ctrl+C 干净退出**:CLI 进程退,unix socket close,daemon 转发 goroutine 写失败 → 关 ssh session → 远端进程收 SIGHUP。已经处理。

---

## [Go 2.3.0] — 2026-05-07

### Added
**所有 CLI 命令(非 TTY)走 daemon** —— 之前 daemon 只服务 tab 补全,日常 `srv ls / srv "git status" / srv cd /opt` 仍每次握手 2-3s。现在 daemon 跑起来后,这些都瞬间复用同一条 SSH 连接。

具体路由:

| 调用 | v2.2.1 | v2.3.0 |
|---|---|---|
| `srv _ls <prefix>` (tab) | daemon | daemon(已经是) |
| `srv ls /opt` / `srv "..."`(非 TTY) | 直连 | **daemon**(`tryDaemonRun`) |
| `srv cd /opt` | 直连 | **daemon**(`tryDaemonCd`) |
| `srv pwd` | 本地 | 本地(daemon 不参与,纯读 sessions.json) |
| `srv -t <cmd>`(交互) | 直连 | 直连(TTY 必须 PTY) |
| `srv shell` | 直连 | 直连(PTY) |
| `srv push / pull / sync` | 直连 | 直连(SFTP / tar 流) |
| `srv -d <cmd>`(后台) | 直连 | 直连(spawn,一次性) |
| `srv logs <id> -f` | 直连 | 直连(stream) |

### Changed
- `daemonRequest` 加 `Cwd` 字段。**daemon 永远不读自己的 sessions.json**——daemon 的 session id 跟调用 shell 的 session id 不同,daemon 用自己的 session 里的 cwd 是错的。CLI 把自己的 cwd 跟每个请求一起发过去。
- `handleCd` / `handleLs` / `handleRun` 全都用 `req.Cwd`。`handlePwd` 改成纯 echo(协议完整性,实际 CLI 直接读本地 session)。
- `changeRemoteCwd` 拆出 `validateRemoteCwd` —— 无副作用的 cd-and-pwd 探测。CLI 走 daemon 失败时回落到这个直连版本;daemon 内部不再调原 `changeRemoteCwd`(那个会写 sessions.json,wrong session)。
- `tryDaemonCd` 三态返回 `(newCwd, used, err)`:`used=false` 没 daemon → 直连;`used=true err=nil` 成功;`used=true err!=nil` daemon 给了明确错误(比如目录不存在)→ 不重试直连,直接报错。

### Performance(预期,网络通时)

| 路径 | v2.2.1(daemon 跑着) | v2.3.0(daemon 跑着) |
|---|---|---|
| `srv _ls /opt/<TAB>` | 70ms(已经走 daemon) | 70ms |
| `srv ls /opt`(非 tab) | **2700ms**(直连握手) | **~100ms**(daemon 池) |
| `srv "git status"` | 2700ms | ~100ms |
| `srv cd /opt` | 2700ms | ~100ms |
| `srv -t htop` | 直连 | 直连(不变,正确) |

简单说:**日常工作流的所有连环命令,从第二条开始都是毫秒级**。

### Notes
- 流式命令(`srv "tail -f /var/log/x"` `srv "find / -name foo"`)走 daemon 后输出会**buffer 到命令结束**才出现,因为 daemon 协议是一次响应一条 JSON。要实时输出请用 `srv -t <cmd>`,会自动绕过 daemon 走 PTY 直连。
- daemon 仍可用 `srv daemon stop` 关掉;关了之后所有命令自动回到 v2.2.1 之前的直连行为。

---

## [Go 2.2.1] — 2026-05-07

### Added
**Daemon 自动启动**(填 v2.2.0 留下的"得手动跑 `srv daemon`"坑):

- `ensureDaemon()`:`srv _ls` 找不到 daemon 时,后台启 `srv daemon`,1.5 秒内轮询 socket 出现就重试。
- `spawnDaemonDetached()` 跨平台 detach:
  - Unix:`SysProcAttr{Setsid: true}` —— 子进程是新 session leader,父 shell 退出不带走它
  - Windows:`DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_BREAKAWAY_FROM_JOB` —— 摆脱 Windows Terminal 的 Job 对象,关终端不杀 daemon
- 竞态:两个 srv 同时尝试启动 → 第一个 listen 成功,第二个看到 socket 已绑定就退出(`cmdDaemon` 已有此分支)
- Release 子 process handle,避免父进程攒僵尸句柄

### Fixed
**`getClient` 持锁拨号导致 daemon 整体被堵**:

- 远端不通时 `ssh.Dial` 阻塞 50+ 秒。原 `getClient` 在拨号期间持有 `daemonState.mu`,所以 `daemon status` / `daemon stop` 等只读请求也跟着挂。
- 改成"快路径持锁查池 → 快路径未命中先释锁 → 慢路径无锁拨号 → 重新加锁安装(同时检测竞态重复拨号)"。
- 实测:_ls 拨号挂 56s 期间,`daemon status` 仍 47ms 回包。

### Notes
本会话末尾测试时远端服务器(23.166.40.67)不通,`srv check` 自己也 15s 超时退出,与 srv 代码无关——daemon 无法实际服务 ls 请求,但 auto-spawn 行为本身已验证。

---

## [Go 2.2.0] — 2026-05-07

### Added
**Phase 2 + Phase 3:连接守护进程 + 测试 + CI**

- **`srv daemon`** —— 常驻守护进程,本地 AF_UNIX socket(`~/.srv/daemon.sock`,Win10+ 也支持)。持有 per-profile SSH `*Client` 池;每条连接独立 10 分钟空闲 TTL,整个 daemon 30 分钟全空闲自动退。`srv daemon status` / `srv daemon stop` 客户端管理。Ctrl-C 优雅关闭并清理 socket 文件。
- **协议**:每行一条 JSON-RPC——`{op, profile, ...}` → `{ok, ...payload}`。当前实现的 op:`ls` / `cd` / `pwd` / `run` / `status` / `shutdown` / `ping`。
- **`srv _ls` 三级回退**:文件 cache(60ms)→ daemon(~70ms,内存 cache 命中)→ 直连(~2700ms,完整握手)。daemon 跑起来后,**冷 tab 从 2700ms 降到 70ms**(38× 加速)。
- **后台 prefetch**:daemon 处理 `ls /opt/` 后,异步 fire-and-forget 预取所有子目录。下一级 tab 从 daemon-cold ~500ms 降到 daemon-cache-hit ~80ms。
- **单元测试**:`parseHostSpec` / `shQuote` / `shQuotePath` / `parseDuration` / `globToRegex` / `matchesAnyExclude` / `splitRemotePrefix` / `resolveRemotePath` / `parseSyncOpts` 等 9 个纯函数,共 ~150 行,`go test ./...` 35ms 全过。
- **GitHub Actions CI**:linux / macos / windows × go 1.22,跑 `go vet` + `gofmt -l` + `go test -race` + `go build`,PR 自动触发。

### Skipped
**rsync-style delta sync** —— 分析后认为不值得做。当前 git/mtime 模式已经在文件层面只传变更文件;byte-level 滚动哈希 delta 的真实增益场景是"单文件 GB 级,只改少量字节"(增量数据库 dump 之类),不是 srv 的主力使用场景(推代码到远端开发机,KB 级文件)。这种场景应该直接用 `rsync`。任务从 backlog 删除,避免过度工程。

### Performance numbers (本机实测)

| 路径 | v2.0.4 | v2.2.0 | 加速比 |
|---|---|---|---|
| `_ls` 冷调用 | 2.7s | 2.7s(daemon 没起)/ 3.1s(daemon 起冷) | 1× |
| `_ls` 同前缀 5s 内重复 | 60ms(文件 cache) | 60ms(文件 cache) / 70ms(daemon 内存 cache) | 1× |
| `_ls` 不同前缀,**首次** | 2.7s | **80ms**(daemon prefetch 命中) | **34×** |
| `_ls` 跨 5s 后再调 | 2.7s | 70ms(daemon 内存 cache 5s TTL 已过 → 直接走 daemon SSH 不握手) | 38× |

---

## [Go 2.1.0] — 2026-05-07

### Added
**Phase 1 of the optimization roadmap (4 features)**:

- **`profile.jump`** —— ProxyJump 支持。一个或多个堆叠跳板,格式 `[user@]host[:port]`,JSON 数组或 `srv config set <prof> jump bastion1,bastion2`。底层在 `crypto/ssh` 的 `Client.Dial` 上链式打 TCP 隧道,中间 client 由 srv 的 Client 持有,Close 时反向拆除,不漏 socket。
- **`srv shell`** —— 交互式远端 shell,自动 cwd 定位(`cd <cwd> && exec ${SHELL:-/bin/bash} -l`),PTY + raw-mode stdin。
- **`srv sync --watch`** —— fsnotify 递归 watcher 跟踪 localRoot,250ms debounce 后触发 sync;新建子目录自动加 watch;Ctrl+C 优雅退出(刷一次队列里待 sync 的)。第一次 sync 是普通 sync,之后进 watch loop。
- **错误分类化贯穿到所有 SSH 路径**:`runRemoteStream` / `cmdCd` / `cmdPush` / `cmdPull` / `cmdSync` 失败时调 `printDiagError(err, profile)`,把原 `srv check` 的 9 类诊断 + 修复建议(no-key / host-key-changed / dns / refused 等)统一给 CLI 命令。Profile 加 transient `Name` 字段(`json:"-"`)由 `ResolveProfile` 填,深层不用串签名。

---

## [Go 2.0.4] — 2026-05-07

### Added
**bash 和 zsh 也接上远端 tab 补全**(0.7.5 之前只有 PowerShell 有):

- bash 模板:跟踪 `sub` / `sub2` 双层位置参数;`srv cd <TAB>` 只补远端目录,`srv pull <TAB>` 第一位远端、第二位本地,`srv push <TAB>` 反之
- zsh 模板:同等行为,用 `compadd -S ''` 保留 dir 后斜杠不加空格
- bash `_srv_remote_ls` 内置 `MSYS_NO_PATHCONV=1`——保护 git-bash 用户(否则 git-bash 会把 `/opt/` 自动转成 `C:/Program Files/Git/opt/` 再传给 native srv.exe)。Linux / macOS 上是无害空操作

### Fixed
- `srv _ls` 失败时把原因写到 stderr(原先静默)。argument completer 上下文里 stderr 不影响 UX,直接命令行调用时方便诊断("找不到目录 / 路径含 git-bash 路径转换 / 等")
- `_ls` 超时从 3s 放宽到 10s——首连握手在慢链路下能装得下;命中缓存仍然 ~60ms

### Notes
- 验证脚本:bash 在 git-bash + 真服务器上 10/10 上下文通过(subcommand / config 二级 / `-P` profile / `use` profile + `--clear` / `cd` 远端 dirs / `pull` 远端 any / `push` 第一位本地 + 第二位远端)
- zsh 模板按 zsh 约定写,未在本机测(没装 zsh);Linux 用户用过来反馈可以调

---

## [Go 2.0.3] — 2026-05-07

### Added
**Tab 远端补全**——`srv cd /opt/<TAB>`、`srv pull /etc/host<TAB>`、`srv push README.md /mnt/<TAB>` 现在直接列远端的目录和文件:

- 新增内部子命令 `srv _ls <prefix>`:在远端跑 `ls -1Ap <dir>`,按 base 过滤,目录加 `/` 后缀,把完整路径(`<dir>+<entry>`)输出到 stdout 供 shell 替换
- 5 秒 TTL 缓存到 `~/.srv/cache/ls-<sha1>.txt`,key 是 `host+user+target` 的散列。冷调用 ~2.7s(SSH 完整握手),缓存命中 ~65ms(40× 加速)
- PowerShell completer 三个分支用上:
  - `srv cd <TAB>` —— 只列 dirs(`-EndsWith '/'`)
  - `srv pull <TAB>` —— 第一位远端 any,第二位本地文件
  - `srv push <TAB>` —— 第一位本地,第二位远端 any
- 跟随当前 session 的 cwd 和 pinned profile —— `srv use prod` 切完,远端补全立刻基于 prod 列目录
- 失败安静(profile 未配 / 远端不通 / 路径不存在 → 空补全,不打扰用户)
- 跟 `_profiles` 一样,`srv completion powershell` 会把 srv.exe 的绝对路径烧进脚本,确保 ArgumentCompleter 作用域能找到二进制

bash / zsh 模板暂未接远端补全(按模式 grep 文档可见),想用先在 PowerShell 上验证。

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
