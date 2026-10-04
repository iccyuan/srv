# 架构

[English](./ARCHITECTURE.en.md) | 中文

这是 `srv` 的技术参考文档:不只是高层架构,还包括各个非显然实现选择背后的**原因**(网络/性能、跨平台行为、guard 闸、包分层)。README 是面向用户的使用指南;本文面向贡献者,以及任何想知道"为什么这样实现"的人。

## 目录

- [概览](#概览)
- [整体模型](#整体模型)
- [仓库结构](#仓库结构)
- [核心概念](#核心概念)
- [命令分发](#命令分发)
- [SSH 客户端](#ssh-客户端)
- [Daemon](#daemon)
- [网络与性能](#网络与性能)
- [Sync](#sync)
- [MCP](#mcp)
- [Guard](#guard)
- [跨平台说明](#跨平台说明)
- [安装器](#安装器)
- [状态文件](#状态文件)
- [扩展清单](#扩展清单)
- [测试](#测试)

## 概览

`srv` 是一个单二进制 Go CLI,用来在远端 SSH 主机上执行命令。本机状态放在 `~/.srv`,直接走 `golang.org/x/crypto/ssh` 说 SSH(不依赖系统 `ssh`,不依赖 Python),同时对人提供 CLI、对 AI 编码 agent 提供 stdio MCP server。

## 整体模型

```text
本机 shell / MCP 客户端
        |
        v
   srv CLI / srv mcp
        |
        +-- 本地状态: config.json, sessions.json, jobs.json
        |
        +-- daemon 客户端 -> srv daemon -> 池化的 SSH 连接
        |
        +-- 直连 SSH -> 远端主机
```

daemon 是优化项,不是必需。重型流式操作在更简单或更安全时仍可走直连 SSH。

## 仓库结构

标准 Go 布局:module 在仓库根,入口在 `cmd/srv`,其余都在 `internal/`(40 个职责单一的包)。

```text
cmd/srv/                入口、全局 flag 解析、命令分发
internal/config         配置 schema、profile 解析、原子 JSON 写、
                        GuardActive(guard 生效状态解析)
internal/session        每 shell 记录、session id 推导、cwd 持久化、
                        GuardPref(env+session 层)
internal/sshx           SSH 拨号/认证/known_hosts/keepalive/ProxyJump、
                        SFTP、capture 与流式执行辅助
internal/daemon         连接池 daemon + CLI 侧协议
internal/transfer       push/pull、并行分块传输、断点续传
internal/syncx          sync 文件收集、tar 流、删除、watch
internal/remote         裸 CLI 远端执行路径(流式)
internal/runwrap        远端命令包装:cwd、失败重启、cpu/mem 限制
internal/mcp            stdio MCP server、工具处理、执行前 gate
internal/guard          `srv guard` CLI(开关 / 规则 / status)
internal/group          profile group、并行扇出(-G)
internal/sudo           远端 `sudo -S` 密码处理
internal/streams        tail / journal 流式 + 自动重连;journal 在
                        `--prefer log` / `prefer_log: true` 下分派到 macOS
                        `/usr/bin/log show` 或 `log stream`(NSPredicate 翻译)
internal/tunnel         端口转发定义
internal/tunnelproc     独立进程模式 tunnel
internal/jobs           后台任务记录(jobs.json)
internal/jobcli         jobs / logs / kill CLI
internal/jobnotify      任务完成 OS 通知 / webhook(叶子包)
internal/check          连接诊断、RTT、带宽、换 key
internal/prune          `srv prune` 各 target(选择性清理)
internal/recipe         命名多步剧本
internal/hooks          生命周期钩子(pre/post cd/sync/run/push/pull)
internal/history        ~/.srv/history.jsonl CLI 命令记录
internal/atrest         history / mcp-replay 的 AES-256-GCM 静态加密
internal/completion     本地 + 远端 shell 补全
internal/picker         TTY 选择器 UI
internal/ui             一屏式 TUI dashboard
internal/theme          颜色 preset
internal/progress       传输进度条
internal/platform       按 OS 的 stats/notify(platform_*.go 拆分)
internal/install        浏览器安装器 + 平台 PATH 辅助
internal/launcher       后台 daemon / detached 进程拉起辅助
internal/srvtty         TTY / raw 模式 / shell 引用辅助
internal/srvutil        共享工具:路径、文件锁、原子 JSON
internal/mcplog         mcp.log 生命周期 + prune
internal/i18n           帮助文本、中/英本地化
internal/project        .srv-project pin 解析
internal/diff           本地/远端文件 diff
internal/editcmd        srv edit / open / code
internal/hints          命令拼写提示
```

## 核心概念

### Profile

一个 SSH 目标:host/user/port/identity、默认 cwd、网络设置(keepalive、拨号重试、连接池大小、压缩)、sync 默认值、ProxyJump 链、profile 级远端 env。存在 `~/.srv/config.json`。

### Session

一个本机 shell 或 MCP 进程。存:可选 pin 的 profile、按 profile 的 cwd map、上一次 cwd(给 `srv cd -`)、每 shell 的 guard 三态、last-seen 元数据。存在 `~/.srv/sessions.json`。

session id 的推导方式(对 guard 和 cwd 都是关键 —— 见[跨平台说明](#跨平台说明)):

- 设了 `SRV_SESSION` 环境变量则用它。
- Unix:父进程 pid(`os.Getppid()`)—— 你交互的那个 shell。
- Windows:沿进程树上溯,跳过启动器包装(`python.exe` 等),停在第一个真正的 shell。

profile 解析优先级:

```text
-P/--profile > session pin > SRV_PROFILE > .srv-project > config 默认
```

### cwd

远端 `cd` 没法跨多次独立 SSH 命令保持,所以 `srv cd` 在远端校验路径、把解析出的绝对路径存到本地,后续命令用 `cd <cwd> && (...)` 包起来。

## 命令分发

`cmd/srv` 先解析全局 flag(`-P/--profile`、`-G/--group`、`-t`、`-d`、`--no-hints`)。保留子命令本地处理;不在保留集里的第一个参数当作对当前 profile 的远端命令。检测到 AI agent shell(`CLAUDECODE` / `CODEX_*` 标记)时,裸 CLI 的远端子命令被硬拒绝并指向 MCP server(逃生开关 `SRV_ALLOW_AI_CLI=1`)—— 裸 CLI 会绕过 MCP 的 token/sync/guard gate,所以 agent 必须走 MCP。

## SSH 客户端

`internal/sshx` 负责 SSH 行为:

- 设了 `SSH_AUTH_SOCK` 走 SSH-agent 认证,然后 profile `identity_file`,再默认 key 路径。
- known_hosts 校验,首连 accept-new;key 变了一律拒。
- 可选 ProxyJump 链;可选 SOCKS5/HTTP-CONNECT 代理(仅第一跳)。
- TCP keepalive(SO_KEEPALIVE,15s)+ SSH 层 keepalive。
- SFTP 客户端懒初始化,归 `*Client` 所有。

`Client.Close()` 拆掉 SFTP、主连接、ProxyJump 链(反序)、keepalive goroutine(用 stop channel,这样短命的 MCP 客户端不会堆积空闲 goroutine)。

### ProxyJump 链 / per-hop key

`profile.jump` 是有序 hop 列表。两种 JSON 形态共存,序列化时按"最简"形式回写:

- `"jump": ["A"]` —— **profile-name 引用**。`config.ResolveJumps()` 在 `Load()` 时把名字展开成具体连接信息,A 自带的 `jump` 链会被递归前置(B → A 实际拨号变成 A 的 jump → A → B),环引用(A→B→A)被检测后短路并保留字面值让 dial 报清晰错误,不会无限递归。
- `"jump": [{"spec": "user@host:port", "identity_file": "~/.ssh/keyA"}]` —— **字面 hop + 可选 per-hop key**。CLI 简写:`srv config set B jump A+~/.ssh/keyA`(逗号分隔多条)。

判别规则在 `looksLikeProfileName`:**hop spec 含 `@` 或 `:` 就按字面 SSH host 解析,否则尝试 profile 查表**。命名冲突极少 —— profile 名通常是 `prod` / `bastion`,合法 hostname 而又恰好不含标点的极少见。

**Auth 隔离是设计核心**(`internal/sshx.hopKeyPath`):hop 的 auth 永远不读父 profile 的 `identity_file`。hop 自己有 key 就用它,没有就走默认 key chain(agent + `~/.ssh/id_ed25519` / `id_rsa` / `id_ecdsa`)—— **父 profile 的 key 只用于最终目标**。这匹配 OpenSSH ProxyJump 语义,也是为什么"Mac 用 Mac 专属 key、走 bastion 中转,bastion 不认那把 key"能正常工作:bastion 那一跳压根不会被试到 Mac 的 key。`hopKeyPath` 签名故意不接收父 profile,从类型层面杜绝泄露。

## Daemon

监听 `~/.srv/daemon.sock`,按 profile 池化 SSH 连接,提供 `ls`/`cd`/`pwd`/`run`/`stream_run`/`status`/`shutdown`。

设计规则:

- 拨号或跑远端命令时绝不持有 `daemonState.mu`。
- 空闲超 30s 的池化连接复用前先健康探测,绝不把已死连接交出去。
- 并发相同的 `ls`/`run` 请求单飞合并。
- GC 时丢弃过期补全缓存;空闲 30 分钟自退出。

连接池大小:`pool_size` 默认 4(clamp 到 `[1,16]`)。单条 SSH 连接的流控窗口在高带宽时延积链路上会卡住吞吐;并发 MCP 调用 / 大 sync 树 / 繁忙的 `srv ui` 会在一条连接上排队。4 条并行连接填满管道,又不至于触到远端 `sshd` 的 `MaxStartups`/`MaxSessions` 预算。`GetPoolSize()` 把 unset 和 `<1` 当默认;`pool_size: 1` 回到旧的单连接行为。`autoconnect: true` 在 daemon 启动时把 profile 预热进池(首条命令从约 200-800ms 握手降到 0-RTT)。

## 网络与性能

- **并行分块传输**:≥32 MiB 且无可续传 partial 的文件拆成 8 MiB 块,在一条 SSH 连接上走 N 路并行 `WriteAt`/`ReadAt`,让窗口刷新往返互相重叠。高 RTT 链路约 3-5×,LAN 上无副作用。`SRV_TRANSFER_CHUNK_{THRESHOLD,BYTES,PARALLEL}` 可调。
- **目录并行**:递归 push/pull/sync 把文件扇到 `SRV_TRANSFER_WORKERS` 个 goroutine(默认 4,范围 1-32),共用同一条连接。
- **断点续传 + 哈希前缀校验**:用远端 `sha256(head -c N)`(~80 字节回包)确认 partial 是真前缀,而不是把它重新下载来比对。
- **size + mtime 跳过**:push/pull 遇到同尺寸文件先比两端 mtime(±2s,吸收 SFTP / FAT 的秒级粒度),一致即跳过,零额外往返;只有 mtime 不一致才回落到哈希前缀校验。每次传完都把本地 mtime 盖到远端(`Chtimes`),所以升级后第一次重推仍走哈希,之后就全是快路径。`SRV_TRANSFER_ALWAYS_HASH=1` 退回逐文件哈希。
- **sync 复用进程内共享连接**:syncx 的每一步远端操作(远端 stat、tar 上传、删除、pull 侧 git/glob 列举)曾各自 `sshx.Dial`,一次 `sync --delete --diff` 要握手三四次,`--watch` 每次保存都重拨。现在全部借用 transfer 的 per-profile 共享客户端(`transfer.AcquireSharedClient`),连接层错误时驱逐缓存重试一次;所有操作都是幂等的,盲重试安全。
- **压缩**:`compress_sync`(默认开)对 sync tar 流 gzip;`compress_streams`(默认关)在网络上 gzip 抓取的 stdout —— 只在慢/跨区域链路划算,解码失败回落明文。
- **拨号重试**:`dial_attempts` / `dial_backoff`(指数退避,封顶 30s);认证和 host-key 错误绝不重试 —— 再来一次答案不变。
- **Keepalive**:TCP SO_KEEPALIVE(内核快速发现死对端)+ SSH 层 keepalive(`keepalive_interval`/`keepalive_count`)。

## Sync

四种收集模式:git(modified/staged/untracked)、mtime(某时长内改动)、glob(支持 `**`)、显式列表。传输是 Go tar 流管进远端 `tar -xf -`(`compress_sync` 时 `-xzf -`)。删除支持刻意只在 git 模式,带预览纪律和默认安全上限。`sync --watch` 给非排除目录装 fsnotify;事件去抖、运行串行,最多排一个后续。

## MCP

`internal/mcp` 是 stdio 上的 JSON-RPC,暴露远端执行、cwd/profile、sync、传输、jobs、诊断、daemon 状态等结构化工具。token 纪律很重要,因为客户端把工具 schema 和结果都留在上下文里:

- `run` 输出有上限(64 KiB)。
- 大 payload 不在 text + structured 两处重复。
- `sync` 返回计数而非完整路径列表。
- 工具描述刻意短。
- 无界源(`cat`、裸 `journalctl`/`find /`、`tail -f`)执行前被拒,并指向有界写法 / 后台任务。

## Guard

高危操作确认闸默认开启。非显然部分的原因:

**规则集刻意收窄。** 只拦不可逆破坏(`rm -rf`、`dd of=`、`mkfs`、`DROP`/`TRUNCATE`、写裸盘、对应的 NoSQL、macOS `diskutil`/`newfs_*`)外加主机电源控制。可恢复但有破坏性的操作和纯前置动作(`chattr -i`)刻意排除:默认开下,误报只是带 `confirm=true` 重试一次,但日常操作上的持续摩擦会逼用户彻底关掉闸。漏报不可逆,所以偏向"规则少而全部无歧义"。

**真人确认(MCP elicitation)。** 命中风险规则时,如果客户端在 `initialize` 声明了 `elicitation` capability,guard 不再直接硬拒,而是向客户端发 `elicitation/create` 弹 Allow/Deny,按真人答案放行或拦截 —— 决定权回到人手里,而不是模型自己带 `confirm=true` 绕过。客户端没声明该能力 / 管道已断时退回原来的硬拒(文档化的降级路径),绝不因为问不到人就放行。传输上有个坑:请求 loop 严格串行、tool handler 跑在读 goroutine 里,"发请求再等回包"会死锁(handler 阻塞时没人读 stdin)。所以 `elicitConfirm` 复用 loop 那个**同一个 `bufio.Reader`** 内联抽帧,直到匹配回包到达,顺带处理串行客户端可能插进来的少数帧(ping / 通知)—— 保持"一次只做一件事、裸 global 无竞争"的不变量。结果信封上 `guardDenied`(人明确拒绝:别重试、别找绕过)和 `guardBlocked`(问不到 / 未确认:这是绕过方式)语义不同,但都保留 `guard_blocked=true`。

**引号 payload 匹配。** `codePositions` 把每个字节分类为代码位 vs 字符串字面量,所以 `echo "rm -rf /"` 不触发 —— 引号内容视为惰性。同一规则会放过 `mysql -e "DROP DATABASE x"`。DB 客户端规则的做法是:把正则锚在**未加引号的客户端二进制**(`mysql`/`psql`/`mongosh` 等)上,它在代码位,再向前伸进引号参数。闸检查的是匹配起点,所以引号里的 verb 被抓到,而整体被 echo 包住的形式(客户端名自己在引号里)仍被抑制 —— 不放大误报。客户端→flag、flag→verb 两段都用 `[^|;&\n]`,verb 必须和客户端在同一条简单命令里,后面的 `&& echo "...drop database..."` 不会触发。有界量词保持 RE2 线性。

**三层状态,以及 `--global` 为什么存在。** 生效状态解析为:`SRV_GUARD` env > 每 session 记录 > 全局 config(`GuardConfig.GlobalOff`)> 内置默认开。每 session 记录按 ppid 推导的 session id 取。MCP server 是 AI 客户端的子进程,不是你交互 shell 的子进程,所以它的 session id 永远对不上。因此每 shell 的 `srv guard off` 到不了模型那条路径 —— 这正是 `srv guard off --global` 存在的全部理由。它写 `config.json`,而 MCP server 每次调用都重读(实时,无需重启)。

**包分层。** `config` import `session`,所以 env+session 这层放在 `session.GuardPref()`(三态:enabled/disabled/unset,不带默认),全局+默认层放在 `config.GuardActive()`。`session` 不能 import `config`(成环),所以 `GuardActive` 是唯一真相源;凡是手里有 `*config.Config` 的 guard 消费方都必须调它,而不是只看 env+session 的 `session.GuardOn()`。

## sudo(CLI + MCP)

`srv sudo <cmd>` 远端跑 `sudo -S` 把密码从 stdin 喂进去。本地 `term.ReadPassword` 关回显读密码;daemon 进程内存按 profile 缓存 password,默认 TTL 5 min,daemon 硬上限 60 min,**绝不落盘**。`exit 1` + 错密文案的常见片段命中时自动清缓存,避免下次仍用错密触发账户锁定。

**MCP `sudo` 工具:两条强制 + 一个 per-profile opt-in。**

- **强制 1:每次都 elicit。** 不像 `run` 的 guard,sudo 没有 `confirm=true` 这种 model-side bypass —— 跨越权限边界永远问真人。客户端没 elicitation capability 直接 hard-deny。
- **强制 2:写缓存路径只通过 srv 自己的两条受控入口。** 密码进 daemon 缓存只有两条路:TTY 的 `srv sudo` CLI(`internal/sudo.cacheSet`,小写,不导出),以及 MCP sudo handler 的密码 elicitation(`sudo.CacheSet`,大写,单一调用点)。MCP 路径不暴露任何通用写权 —— 模型不会被指示去问你密码,也没有"贴密码进 chat"的回路;唯一的"写"是经过下面那条**受控** elicitation 通路。
- **默认行为:密码 elicitation 默认开启。** cache miss → `handleSudo` 发**第二次 `elicitation/create`**(schema 是一个 required string 字段),客户端弹密码框 → 用户答复 `{action: accept, content: {password: "..."}}` → `handleSudo` 把 password 直接喂给 `sudo.CacheSet`(5min TTL)+ 继续 dial+run。密码走 client UI → srv → daemon,**不进任何 tool result,模型不可见**。mcplog 只记 `action=accept`,密码/长度/任何衍生都不落日志。`profile.enable_mcp_password_prompt = false` 显式退出这条路径,回到 TTY-seed-only 模式(`{cached: false}` 提示去终端 seed)。

**为什么默认 on(以及 off 什么时候有意义)。** 早先默认 off 的论据是"opt-in 流程比 TTY-seed 多一条 prompt-injection 表面" —— 恶意 tool result 诱导模型调 `sudo` 会触发同样的密码框,用户可能分不清是不是自己发起的。但是**强制 1 的 Allow/Deny 弹窗永远在密码框之前**:真人不点 Allow,密码框根本不会渲染。所以 injection 想拿到密码框,必须先骗过用户点 Allow —— 在这个前提下,"密码框是不是自己触发的"已经被前一步的 Allow/Deny 确认覆盖了。默认 off 的代价是每个新 session 都得切回终端 seed 一次,真实负担远大于残留收益,所以默认翻成 on。仍然存在更强属性"密码进入系统必须是一条真人主动跑的终端命令" —— injection 即使能弹 Allow/Deny 也跨不过去 —— 这条边界对生产机 / 共享 bastion 有意义,在那些 profile 上设 `enable_mcp_password_prompt: false` 退回 TTY-seed-only。继承上故意不传染(`EnableMCPPasswordPrompt` 不写进 `mergeFrom`),避免 child profile 隐式继承 opt-out(或新的默认 on)。

**Schema 兼容性(2026-05-24 live verify 验证项)。** 最初设计带 `format: "password"` 期望客户端渲染掩码;Claude Code 的 MCP client 校验 `requestedSchema` 时只接受 `format ∈ {email,uri,date,date-time}`,带 `format:"password"` 的请求被它在渲染前就 `-32602 invalid params` 拒掉,整个 elicit-pw 静默失败(`asked=false`)。最终方案是 schema 不带 `format`,纯 string 字段加 `description` 标注敏感性,跨客户端通用;代价是支持 `format:"password"` 的客户端(比如 MCP Inspector)也走明文渲染。`elicit_test.go` 把这点固化成反向不变量(如果将来谁加回 `format:"password"` 就是回归)。

**传输上的两个 elicit 共享同一个 reader。** `elicitPassword` 跟 `elicitConfirm` 一样,复用 loop 的 `bufio.Reader` 内联抽帧,避免"发请求等回包"在串行 loop 里死锁,顺带处理客户端可能插进来的 ping / notification 帧。这两个函数有相当多重复——MVP 时刻意没抽公共抽象,先看 opt-in 路径是否真有人在用。

## 跨平台说明

`srv` 用一个二进制覆盖 Windows、macOS、Linux、BSD。会咬人的不可移植细节:

- **session id**:Unix 用父进程 pid;Windows 沿进程树上溯跳过启动器包装。后果:MCP server 的 session 永远对不上交互 shell —— `srv guard --global` 为什么存在见 [Guard](#guard)。
- **base64 解码**:GNU/busybox 是 `base64 -d`,macOS/BSD 是 `-D`。detached 任务的 spawn 行先试 `-d` 再回落 `-D`;没有它,macOS 上 detached 任务解码出空。
- **`setsid`**:仅 util-linux。spawn 行用 `command -v setsid` 判断,macOS/BSD 回落到纯 `nohup`(此时 kill 只到包装 pid,`kill_job` 已处理这种回落)。
- **裸盘节点**:macOS 用 `/dev/rdiskN` 做裸访问;guard 的 `> /dev/...` 规则匹配 `r?disk` 覆盖 macOS 形式。`dd of=` 不论目标都拦。
- **按 OS 的代码**:`internal/platform` 和 `internal/install` 按 build tag 拆(`*_unix.go` / `*_darwin.go` / `*_bsd.go` / `*_windows.go`);业务代码不出现 `runtime.GOOS` 分支。Windows 上 `go test ./...` **不会**编译非 Windows 文件 —— 跨平台改动要用 `GOOS=darwin/linux go build ./... && go vet ./...` 验证。

## 安装器

`srv install` 在 localhost 上提供内嵌的 `install.html`:配 PATH、注册 Claude Code MCP、建第一个 profile。PATH/浏览器辅助在 `install_unix.go` / `install_windows.go`。

## 状态文件

默认根 `~/.srv`,`SRV_HOME` 可覆盖。

```text
config.json          profile、group、tunnel、hooks、全局配置
sessions.json        每 session 的 pin/cwd/guard 状态
jobs.json            后台任务记录
history.jsonl        CLI 远端命令记录
mcp.log              MCP 生命周期 + 工具调用日志
cache/               远端补全缓存
daemon.sock          daemon socket
daemon.log           自动拉起的 daemon 输出
```

远端任务日志:`~/.srv-jobs/<job-id>.log`(+ `.exit` 标记)。

## 扩展清单

**加 CLI 命令**:在 `cmd/srv` 分发注册;在合适的 `internal/` 包实现处理;更新帮助文本 + README;加补全条目;加解析/行为测试。

**加 MCP 工具**:在 `internal/mcp` 加紧凑的工具定义 + 处理分支;text/structured 输出不重复;大输出截断或汇总;更新 README 的 MCP 列表。

**加 profile 配置**:给 `Profile` 加字段;加访问器默认值;保证老配置仍有效;在 README(面向用户)和本文(非显然时写原因)记录。

## 测试

```sh
go test ./...
```

Windows 上 `go test ./...` 只编译 Windows build tag。跨平台改动用以下验证:

```sh
GOOS=darwin GOARCH=arm64 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...
GOOS=linux  GOARCH=amd64 go build ./... && GOOS=linux  GOARCH=amd64 go vet ./...
```

通常需要真实 SSH 的部分:`check`、`run`/`shell`、`push`/`pull`、`sync`(+`--watch`)、`tunnel`、MCP 注册。Live SSH 测试必须有界且 loop-safe。Windows 上 Go 写不了默认 build cache 时,把 `GOCACHE` 指到可写目录。
