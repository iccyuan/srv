# Changelog

## [Go 2.6.7] - 2026-05-15

### Added
- **断点续传的流式命令**:`srv tail` 单文件用 `tail -F -c +<N>` 按字节续传(多文件回退到 `-n 0` 不重放 backlog);`srv journal -f` 用 ISO 时间戳重续(`--since=<ts>` + 边界行去重)。重连后不再从头打印 -n N 条 backlog,丢失窗口最小化。
- **每 profile 加密套件/MAC/KEX/HostKey 锁定**:新增 `ciphers` / `macs` / `key_exchanges` / `host_key_algorithms` 字段,空缺保持库默认。x86 AES-NI 上锁 `aes128-gcm@openssh.com` 比默认 chacha20-poly1305 大宗传输 2-3 倍。
- **daemon 单飞 `run` 去重**:并发同 `(profile, cwd, command)` 请求合并成一次 SSH session。MCP 并发 4 个相同 `ls /` 现在只跑一次。
- **可选 wire-level gzip**:`compress_streams: true` 时 `RunCapture` 在远端把 stdout 通过 `set -o pipefail; ... | gzip -c -1` 压缩、本地解。stderr 不动;远端缺 gzip 自动回退明文。跨境/移动网络的大输出明显省流量。
- **大文件并行分片传输**:文件 ≥ 32MB 且无可续传 partial 时,push/pull 走 N 路并行 WriteAt/ReadAt 同时填 SSH 窗口。高 RTT 链路 3-5× 提升。`SRV_TRANSFER_CHUNK_{THRESHOLD,BYTES,PARALLEL}` 可调。
- **daemon 预热(`autoconnect`)**:profile 上设 `autoconnect: true`,daemon 启动并行 dial 进池。首条命令从 ~200-800ms 握手降到 0-RTT。
- **每 profile 多 SSH 连接池**:`pool_size: N`(默认 1,上限 16)。`acquireClient` 按 inflight 最少调度,池空闲超 `connIdleTTL` 单独回收一个槽不影响其他;高并发 MCP / 大 sync 场景填满 TCP 窗口。
- **at-rest 加密(opt-in)**:`SRV_AT_REST_ENCRYPT=1` 后 `history.jsonl` 和 `mcp-replay.jsonl` 用 AES-256-GCM 逐行加密。密钥 `~/.srv/secret/key`(Linux 0600 / Windows 限制 DACL 到当前 SID)。读侧自动检测明文/密文混存,平滑迁移。
- **Tunnel 独立进程模式**:`independent: true` 让 `srv tunnel up` 起 `srv _tunnel_run <name>` 独立子进程(状态 `~/.srv/tunnels/<name>.json` + 日志 `<name>.log`)。daemon 重启不杀该 tunnel。`cmdDown` fan-out 同时尝试 daemon-hosted + independent。
- **Windows 一等公民**:6 个 gap 补齐 —— TCP loopback fallback(老 Win10 / Server 2016 没 AF_UNIX)、SIGWINCH 等价物(250ms 轮询)、PID + 创建时间防 PID 复用、`~/.srv/secret/key` 明确 DACL 到当前 SID(NTFS)、`CTRL_BREAK_EVENT` 优雅停 tunnel、显式 ENABLE_VIRTUAL_TERMINAL_PROCESSING。
- **新 `internal/platform` 包**:7 个接口(Process / Console / Crypto / SystemStats / Notifier / Opener / Shell)+ 每 OS 一个文件。加新 OS = 写一个 `platform_<goos>.go`,不改任何已有文件。HZ 检测覆盖 100/250/1000 三种主流 Linux 内核;MemAvailable 缺失自动回退 MemFree+Buffers+Cached;容器场景文档化。

### Changed
- **删除 mosh / UDP 传输**:整个 `internal/moshx` 包删除(~1.7k 行),`srv mosh` / `srv mosh-server` 命令、README "Mosh-style UDP" 段、help 表条目全部移除。项目方向坚定 TCP-only。
- **STATS 面板重写**:从 SSH 远端采样改为本地采样(`/proc` on Linux,sysctl + vm_stat on macOS,PowerShell CIM on Windows);从单点 scatter 改为 Braille 字符的连续折线图(Bresenham 插值,2×4 子像素密度);移除颜色"新点"标记。
- **UI 引入 Source 接口**:`srv ui` 和 `srv ui demo` 走同一份渲染代码,只在 Source 实现层分歧(live vs demo);demo 自然变成 live 路径的 screenshot fixture。
- **Tunnel 加 `TunnelHost` 策略接口**:cmdUp / cmdDown / LoadStatuses 不再 3 处独立 branch on `def.Independent`;改为 `hostFor(def)` 选 strategy + `allHosts()` fan-out + `ErrNotHosted` sentinel。加第三种宿主(systemd unit / launchd plist / k8s pod)只需写一个 struct + 一行加进 allHosts。
- **Daemon op 改 registry**:`dispatch()` 15 路 switch 换成 `map[string]opHandler{...}`。加新 op = 一个 method + 一行 map。和 MCP 包的工具 registry 风格一致。
- **`hooks.buildShell` / `install.openDefaultBrowser` 迁移到 platform 包**:前者用 `platform.Sh.Command()`,后者直接用 `platform.Open.Open()` 去除和 `launcher.openLocal` 的重复。

### Fixed
- **STATS 面板各种 UI bug**:JOBS 标题闪烁(切片别名)、TUNNELS/JOBS/GROUPS 空时消失、对齐错位、DETAIL 高度不固定。
- **测试 `TestListStatusesSkipsDeadPIDs` 硬编码 PID=1 在 Linux CI 失败**:Linux PID 1 是 init/systemd,永远活着。改为 `exec.Command("sh", "-c", "")` 起子进程后用其退出后的 PID。
- **Stream resume Suppress 一次性边界去重**:journal 重连后第一行如果和上次最后一行 byte-for-byte 相同则跳过(journalctl `--since` 含边界秒,会重发那行)。

### Notes
- 几乎所有新 feature 都是 lazy + opt-in(`autoconnect` / `compress_streams` / `pool_size` / `independent` / `SRV_AT_REST_ENCRYPT`),默认行为不变,老用户零感知。
- platform 拆分后跨平台代码不再有 `runtime.GOOS` 分支泄漏到业务代码;新加 OS 一个文件搞定。
- 这版的重构跨度是历史最大:从 mosh 完全移除到 platform 接口化、Tunnel 策略化、Daemon registry 化,等于把第二季度积累的设计债集中清算了一次。

---

## [Go 2.6.6] - 2026-05-10

### Added
- **`srv guard` —— MCP 高危操作确认开关(默认关闭,per-shell 开启)**。开启后 MCP 的 `run`/`detach` 命令命中高危 pattern(`rm -rf` / `dd of=` / `mkfs` / `shutdown`/`reboot`/`halt`/`poweroff` / `drop database/table` / `truncate -` / `> /dev/sd|nvme` / `chattr -i` 等)时拦截,需传 `confirm: true` 才放行;`sync` 在 `delete=true` 且非 dry-run 时同样拦截。`status` 工具回显 guard 状态便于模型先查再决定。优先级:`SRV_GUARD=1` env > session record > 默认关。CLI:`srv guard [on|off|status]`。
- **`srv color` —— CLI 远端命令彩色输出(默认开启,所有平台)**。在非 TTY 模式给 `srv <cmd>` 注入 prologue:`export CLICOLOR_FORCE=1 FORCE_COLOR=1` + ls/grep/egrep/fgrep 函数包装(强制 `--color=always`)。MCP 路径**完全不走 prologue**,模型只看到纯文本。`srv color [on|off|use <name>|list|status]`,关闭即 `srv color off`。
- **5 个内置主题**:`dracula`(默认)、`gruvbox-dark`、`nord`、`solarized-dark`、`tokyonight`。每个都是嵌入二进制的 `.sh` 文件(`//go:embed colors/*.sh`),用户也能 `cp` 到 `~/.srv/init/<name>.sh` 后修改作起点。
- **6 种 drop-in 主题文件格式**(放 `~/.srv/init/`):`.sh`(原始 shell)、`.itermcolors`(iTerm2 plist)、`.toml`(Alacritty 新)、`.yml`/`.yaml`(Alacritty 老)、`.conf`(Kitty)、`.Xresources`(xterm/URxvt)。基本能消化 [iTerm2-Color-Schemes](https://github.com/mbadolato/iTerm2-Color-Schemes) 整个仓库。所有 parser 都是字符串扫描,**零第三方依赖**。

### Changed
- **MCP 内存/token 大幅优化**:
  - `run` 工具的 stdout/stderr 不再同时塞进 text Content **和** `structuredContent`(之前是双份,模型历史里直接翻倍)。
  - `run` 输出 cap 在 64 KiB 合并,超出截断 + marker 提示用 `head/tail/grep`(避免 `cat large.log` 撑爆模型上下文)。
  - 7 个工具(`status`/`list_profiles`/`doctor`/`daemon_status`/`env`/`list_jobs`/`detach`)的响应从 pretty-printed JSON 双份改为 compact JSON 单份(去重 + 去 indent 共省 ~65%)。
  - `tail_log` 不再把 tail 内容同时放 text 和 `structured.tail`。
  - `sync --dry-run` / `sync_delete_dry_run` 的 `structuredContent` 不再带完整 files/deletes 数组(text 已截断显示),改为 `*_count`。
  - 工具描述字符串瘦身,空 description 字段不输出。
- **MCP `run` 改走 daemon**(之前每次都直接 `Dial`,~2.7s 冷握手;现在复用 daemon 池化连接)。
- **per-window session(Windows)**:`SessionID()` 不再跳过 `cmd.exe`,每个 cmd / PowerShell / bash 窗口独立 session id。`srv color` / `srv guard` / `srv use` / `srv cd` 真正成为 per-window 状态。Python launcher 仍跳过(它确实是透明层)。

### Fixed
- **`runRemoteCapture` 的 keepalive goroutine 延迟退出**:`Client.Close()` 后要等下一个 keepalive_interval(默认 30s)才退,高频 MCP 调用时短期堆积大量空转 goroutine。加 `stopCh` + `closeOnce`,Close 立刻通知。
- **daemon `lsCache` map 永不删除条目**:TTL 只在读时检查,key 一直堆积。`gc()` 加扫描清理。
- **`prefetchSubdirs` 无界**:一次 tab 补全可能触发上百次后台 ls。加 `daemonPrefetchLimit = 24`。
- **`sync_watch` 重叠上传**:debounce 期间事件密集时可能并行起两个 `tarUploadStream`。加 `running` 标志互斥 + 排队重试。
- **`runCheck` 内层 Dial 超时未受外层 15s 上限约束**:用户改大 `connect_timeout` 时 goroutine 比 runCheck 活得久。`dialTimeout = min(connect_timeout, 14s)`。
- **远端 `LS_COLORS=""` 空串导致 GNU ls 即使 `--color=always` 也不出色**(典型场景:zsh `eval "$(dircolors -b)"` 在 dircolors 数据库缺失时刷成空)。prologue 自带 dircolors 默认色板兜底。

### Notes
- 默认行为变化仅限 CLI:之前没颜色,现在有(可 `srv color off` 关)。
- MCP 行为只是变快变省 token,**没新增任何对模型的 prompt 内容**。
- guard 默认关闭,不开启不影响任何现有 MCP 流程。

---

## [Go 2.6.5] - 2026-05-09

### Added
- **命令拼写提示(默认开)**:输错本地子命令时,`srv` 单行 stderr 提示一下(`'staus' 像是 'status'`),命令照常在远端跑。两个触发点:dispatch 前(token 不是 reserved 但接近某个内置)、远端退出 127("command not found")。Levenshtein 距离 ≤1(短 token)或 ≤2(长 token),首字母必须一致,过滤内部子命令。MCP 路径不触发。**关闭三种**:`SRV_HINTS=0` env(优先级最高)/ `--no-hints` flag / `srv config global hints false`。
- **UI 语言:中英双语 + 系统 locale 自动检测**。`srv help` 整段、高频 fatal / usage / hint 字符串都翻译;技术输出(`srv check`/`srv doctor`/daemon proto/MCP tool 响应)**保留英文**,术语不漂移、grep 友好。检测顺序:`config.lang` → `SRV_LANG` → `LC_ALL`/`LC_MESSAGES`/`LANG` → 英文 fallback。
- **`srv config global <key> [<value>|--clear]`** —— 改顶层配置(对应 per-profile 的 `srv config set <prof> <key> <value>`)。当前 keys:`hints`、`lang`、`default_profile`(后两个分别对应新加的 i18n 和原 `srv config default` 的 alias)。无参列出所有顶层 key 当前值。
- **新全局 flag `--no-hints`**:per-call 关命令拼写提示。

### Changed
- 高频 fatal/usage 字符串(`error: profile %q not found`、`error: no profile selected`、`usage: srv push ...` 等共 ~20 条)走 i18n 表。新加错误字符串请用 `t("err.xxx")` / `t("usage.xxx")`。
- `helpText` 常量拆成 `helpEN` 和 `helpZH` 两份。

### Notes
- 英文用户(默认 fallback)行为完全不变。
- Config schema 兼容:`Hints *bool` 和 `Lang string` 都是 `omitempty`,旧 config 文件读起来没问题,首次 save 时自动加。

---

## [Go 2.6.4] - 2026-05-09

### Added
- **`srv install`** —— 跨平台浏览器图形化安装器。子命令起本地 HTTP server,自动开浏览器,UI 里勾选:加 PATH / 注册为 Claude Code MCP / 跑 `srv init`。`install.ps1 -Gui` / `install.sh --gui` 引导调用。HTML embed(`go:embed`),无外部资源。
- **浏览器优先 `--app` 模式**:openBrowser 先找 Edge / Chrome / Chromium(系统位置 + PATH),用 `--app=URL --window-size=...` 起,得到无 tab/无地址栏的"原生窗口"观感;**找不到任何 Chromium 系列时回落系统默认浏览器**(`start` / `open` / `xdg-open`),保证安装器在裸装系统上也能用。
- **A. 拨号重试**(profile `dial_attempts` / `dial_backoff`,opt-in)。
- **B. OS-level TCP keepalive**(15s,无配置)。
- **C. `srv push` / `srv pull` 单文件断点续传**(目标严格小于源时 seek + append)。
- **G. `srv check --rtt`** —— SSH 级 RTT 探测,出 min/med/avg/max + 丢包率 + verdict。
- 安装脚本 `install.ps1` / `install.sh`(自适配脚本所在目录,幂等,`--uninstall` 干净移除)。

### Removed
- **`python/` 整个目录** —— Python 实现(冻结在 0.7.5)清出仓库。仓库最后一次 Python 版本对齐是 Go 2.0.x 时代,之后 Go 单边演进,Python 版本只是"留个能跑的存档"。已经几个版本没人 touch,git 历史里还能找回。两份 README + go/README + memory 全部清掉相关引用。

### Changed
- 安装文档全部用引导脚本(`install.ps1` / `install.sh`),硬编码 `D:\WorkSpace\server\srv` 路径示例不再出现。

---



## [Go 2.6.3] - 2026-05-08

### Added
- **`srv tunnel -R`**: added reverse forwarding (`srv tunnel -R 9000:3000`) so a remote loopback port can reach a local service over the existing SSH connection.
- **`srv diff --changed`**: compares changed git files against their remote counterparts.
- **`srv doctor --json`**: emits the local diagnostics as machine-readable JSON.
- **MCP tool coverage**: exposed `doctor`, `daemon_status`, `env`, `diff`, and `sync_delete_dry_run`; `sync` also supports remote delete previews/deletes through structured arguments.

### Changed
- **`srv edit` conflict guard**: save-back now re-stats the remote file and refuses to overwrite if size or mtime changed while the local editor was open.
- **`srv sync --delete` safeguards**: non-dry-run deletes are capped at 20 files by default; use `--yes` or `--delete-limit N` after reviewing a dry run.
- **Dependency baseline**: Go module dependencies and CI were moved to Go 1.25.x.

### Docs
- Removed the stale environment-variable limitation because profile-level `srv env` now persists injected remote env values.
- Documented reverse tunnels, changed-file diffs, JSON diagnostics, delete safeguards, and the expanded MCP tool list.

---

## [Go 2.6.2] - 2026-05-08

### Fixed
- **Heredoc through `srv` no longer breaks parse**:`wrapWithCwd` 现在在 `(<cmd>)` 闭合 `)` 之前显式插一个 `\n`,这样 `bash <<'EOF' ... EOF` 类命令的终止符 `EOF` 不再和 `)` 同行而被识别为 `EOF)`(导致 `parse error near '\n'`)。普通命令完全不受影响。
- **池化 SSH 连接长闲置后被无声重用**:`daemon.getClient` 对 `lastUsed > 30s` 的池条目先发一次 `keepalive@openssh.com`,失败则 evict + re-dial。不再把已经被 NAT / 服务端 idle-kill 的死连接交给调用方。

### Added
- **`$SRV_CWD` 环境变量**:`GetCwd` 的回退顺序从`session cwd → profile.default_cwd` 改为 `session cwd → $SRV_CWD → profile.default_cwd`。给 MCP 注册用 —— 在 `.mcp.json` / Claude Code 的 per-project mcpServers 段里写 `"env": {"SRV_CWD": "/mnt/project/foo"}`,每次新 MCP 会话直接落到该项目目录,不用再每次先 `srv cd`。

### Notes
- 这一版纯解决"已知 MCP 翻车"那张表上能落到 srv 这边修的项;`-32700 parse error`(客户端 JSON 编码问题)和 `psql -c` 多 SELECT(psql 行为)不在 srv 责任域,只在 troubleshooting 文档里写了 workaround。

---

## [Go 2.6.1] - 2026-05-08

### Removed
- **`srv profiles` 整套(`profiles` / `profiles use` / `profiles edit`)**:2.6.0 加的别名层,纯重复;直接删,无 deprecation。

### Changed (BREAKING)
- **`srv config use <name>` 改名为 `srv config default <name>`**:旧名和 `srv use`(本 shell pin)同动词不同语义,经典翻车点。直接换名,旧形不再识别。`srv config default` 无参 + TTY 时弹 ↑↓ 选择器;非 TTY 打印当前默认值。

### Added
- **`srv use` / `srv config default` 无参 + TTY 弹出交互式 ↑↓ 选择器**:
  - `↑` / `↓` 或 `j` / `k` 移动,`Enter` 选中,`q` / Ctrl-C 取消
  - `/` 进入过滤模式,边输边过滤;`ESC` 退过滤、`Backspace` 删字
  - 行尾标记区分作用域:`[this shell]`(黄,本 shell pin)、`[default]`(青,全局默认),两者可同时出现
  - 实现:`golang.org/x/term` raw mode + ANSI;无新依赖。非 TTY(管道 / CI)保持原行为
- 命令面整体减少 4 条 → 8 条 profile 相关命令;不丢功能。

---

## [Go 2.6.0] - 2026-05-07

### Added

- Added local workflow helpers: `srv doctor`, `srv open`, `srv code`, and `srv diff`.
- Added profile convenience commands: `srv profiles ...`, `srv config edit [profile]`, and `srv env list|set|unset|clear`.
- Added `srv sync --delete` for git-mode removal of remote files deleted locally. `--delete --dry-run` previews deletions first.
- Added daemon management helpers: `srv daemon restart`, `srv daemon logs`, `srv daemon prune-cache`, and `srv daemon status --json`.
- Added profile-level `env` values, injected before remote commands and detached jobs.
- Added daemon response `data` / `error` fields while keeping the old flat fields for compatibility.
- `srv sync --watch` now prints the active profile, target, and sync mode when it starts.

### Changed

- Completion cache writes now use the same atomic write helper as JSON state files.
- MCP `tools/call` now returns JSON-RPC `-32602` for invalid params instead of continuing with empty arguments.
- Auto-spawned daemon stdout/stderr is captured in `~/.srv/daemon.log`.

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

## [Go 2.5.0] — 2026-05-07

### Added
**两个本地工作流缺口补齐 —— `srv tunnel` 和 `srv edit`**

#### `srv tunnel <localPort>[:[<remoteHost>:]<remotePort>]` —— SSH 端口转发(`ssh -L` 等价)

```
srv tunnel 8080            # 本地 8080 -> 远端 127.0.0.1:8080
srv tunnel 8080:9090       # 本地 8080 -> 远端 127.0.0.1:9090
srv tunnel 8080:db:5432    # 本地 8080 -> db:5432(远端解析)
```

实现:本地 `net.Listen` + `c.Conn.Dial("tcp", remote)` 双向 `io.Copy`,每个连接一个 goroutine。Ctrl-C 关 listener;SSH 连接断开时通过 `c.Conn.Wait()` 监听,自动停。**仅本地→远端方向**,反向(`-R`)按需后加。

适用场景:远端 Jupyter / dev server / 数据库,本地浏览器或客户端访问。

#### `srv edit <remote_path>` —— 远端文件本地编辑器编辑

流程:SFTP 拉到 `os.MkdirTemp` 临时目录(基名保留,编辑器靠扩展名识别语法)→ 启动 `$VISUAL`/`$EDITOR`(支持 `code --wait` 这种带参数的形式 —— 按空格切分)→ 编辑器退出后,对比本地文件 mtime+size:有改动则 SFTP 推回,没改动打印 "no changes; not uploading" 不动。

Editor fallback 顺序:`$VISUAL` → `$EDITOR` → Windows: `notepad.exe` → 其它: `vim` / `vi` / `nano`。

**已知限制**(README 已注明):
- 不上锁。如果同一文件正被另一会话编辑,save-back 会无声覆盖。共享盒子上请直接 ssh 进去 vim。
- VS Code 默认 *不* 阻塞,必须配 `EDITOR='code --wait'`,否则 srv 在编辑器还开着的时候就走完 mtime 比对、得出 "no changes"。
- Notepad 在 Windows 上把 LF 转 CRLF —— 整文件都被识别为"已修改"。建议设 `$EDITOR` 用其它编辑器。

#### Tab 补全

`cd` 用 dir-only,`edit` 走 all-entries(含文件)。bash / zsh / PowerShell 三套都加了。

---

## [Go 2.4.2] — 2026-05-07

### Fixed
v2.4.1 tag 触发的 release workflow 在 linux / macos runner 上撞 `gofmt -l .` 失败 —— 7 个文件本地写时未走 gofmt(Windows 上 IDE 没自动跑)。`go fmt ./...` 一遍后 vet + test 全清。同时把 release 流程在本地更严的 lint 门校一遍,杜绝下次重发。

无功能改动,纯 CI 修复 + 重发 release。

---

## [Go 2.4.1] — 2026-05-07

### Added
**数据格式 future-proofing + sync 压缩**——三处轻量改动:

1. **JSON 文件加 `_version` 字段**(常量 `SchemaVersion = 1`):`config.json` / `sessions.json` / `jobs.json` 加载时若 `_version > SchemaVersion` 给一行 stderr 警告但仍尝试用,保存时总是写当前版本。日后字段语义重命名(比如 cwds 嵌套结构变化)有显式迁移点;新 srv 读老文件无 `_version` → 当作 v0,下次保存自动升级。
2. **sync tar 流加 gzip 压缩**(`compress/gzip`,Level 默认 5):typical 文本/代码 ~70% 体积减少。Profile 键 `compress_sync`,默认 `true`;远端命令对应 `tar -xzf -`。LAN 上的 CPU 代价是单位毫秒级(本机 SHA256 测试也类似量级),不可感知;弱网 sync 时间显著下降。
3. **Daemon 协议加 `v` 字段**(常量 `DaemonProtoVersion = 1`):`daemonRequest` / `daemonResponse` / `streamChunk` 都带上。CLI 收到 `v > DaemonProtoVersion` 给一行 stderr 警告(`Restart the daemon or upgrade srv`)。流式响应只在第一帧检查,不每帧 spam。

### Notes
- 三处改动全是**前向 / 后向兼容**的:老 srv 看不懂 `_version`/`v` 字段会被 json.Unmarshal 默默忽略;新 srv 看到没字段当 v0 处理。无破坏性。
- 我刻意没引入"硬性版本拒绝"——目前 SchemaVersion=1,DaemonProtoVersion=1,只有"警告 + 尽力执行"的语义。实际有迁移需要时再升版本号 + 写 migration code。

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
