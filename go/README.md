# srv (Go,默认版本)

> 起步版本 **2.0.0**。`golang.org/x/crypto/ssh` 自实现 SSH 客户端,**不依赖系统 ssh.exe**。Python 实现仍保留在 [`../python/`](../python),功能完全对等。

## 为什么有这个版本

Python 版本(`v0.7.x`)在 Windows 上撞了一连串系统 OpenSSH 9.5p2 的 bug:`ControlMaster` 把 stdout 管道占着不放、`stdin` 默认转发让 `srv ls` 要按两次 Enter、`getsockname failed: Not a socket`、`Read from remote host: Unknown error` 等等。这些都是**包装系统 ssh** 的代价。

Go 版本通过 `crypto/ssh` 直接做 SSH 协议,彻底绕开这些坑;同时编译成单二进制,部署等于"复制文件"。

## 构建

需要 Go 1.22+。**默认编译到仓库根目录**,这样 `srv` 就是顶层入口、PATH 上有仓库根就可用:

```sh
cd D:\WorkSpace\server\srv\go
go build -o ../srv.exe .          # Windows
go build -o ../srv     .          # macOS / Linux
```

跨平台编译:

```sh
GOOS=windows GOARCH=amd64 go build -o ../srv.exe .
GOOS=linux   GOARCH=amd64 go build -o ../srv     .
GOOS=darwin  GOARCH=arm64 go build -o ../srv     .
```

## 与 Python 版本的差异

| 项 | Python 0.7.5 | Go 2.0.0 |
|---|---|---|
| SSH 客户端 | 系统 `ssh.exe` | 内置 `golang.org/x/crypto/ssh` |
| 文件传输 | 系统 `scp.exe` | `github.com/pkg/sftp` |
| 多路复用 | OpenSSH ControlMaster(Win 上要禁) | 进程内连接复用,无平台限制 |
| 安装 | Python + 复制目录 + 设 PATH | 单二进制 + 设 PATH |
| 运行时依赖 | Python 3.9+ + OpenSSH client | 无(纯静态) |
| Windows ssh.exe quirks | 要 workaround | **不存在** |
| 行为 / 子命令 / 文件布局 / MCP 协议 | — | **逐项对齐,无遗漏** |
| 数据兼容 | `~/.srv/{config,sessions,jobs}.json` | **共用同一组文件**,可来回切 |

## 子命令

完全等价于 Python 版本。所有命令、所有参数、所有 MCP 工具都对齐。详见父目录的 [README.md](../README.md) — 用法部分一字未动。

```
srv init                       配置 profile
srv config <list|default|remove|show|set|edit>
srv use <profile> | --clear
srv cd <path>          srv pwd          srv status        srv check
srv <args...>          srv -t <cmd>     srv -d <cmd>      srv -P <prof> <cmd>
srv push <l> [<r>]     srv pull <r> [<l>]    srv sync [...]
srv doctor             srv env [list|set|unset|clear]
srv open <remote>      srv code [remote_dir]  srv diff <local> [remote]
srv jobs               srv logs <id> [-f]    srv kill <id> [-9]
srv sessions [list|show|clear|prune]
srv daemon [status|restart|logs|prune-cache|stop]
srv completion <bash|zsh|powershell>     srv mcp
srv help               srv version
```

## MCP 注册

```sh
# 编译到仓库根:
cd go && go build -o ../srv.exe . && cd ..

# 注册(Windows 路径示例)
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp

# 验证
claude mcp list   # srv: ✓ Connected
```

无需 `python xxx.py` 转一层。

## 文件 / 包结构

```
go/
├── main.go             入口、全局 flag、派发
├── config.go           ~/.srv/config.json + Profile + ResolveProfile
├── session.go          ~/.srv/sessions.json + 会话 ID 检测公共部分
├── session_unix.go     Unix: os.Getppid()
├── session_windows.go  Windows: 进程树游走,跳过 cmd.exe / python.exe 中间层
├── helpers.go          intToStr, uintToStr, randHex4 等小工具
├── term.go             tty 判定、原始模式、shQuote、base64
├── client.go           SSH 客户端封装(crypto/ssh + sftp)
├── ops.go              高层操作:cd / push / pull / runStream / runCapture
├── check.go            连通性诊断,9 类失败模式 + 修复建议
├── jobs.go             ~/.srv/jobs.json + spawn / list / log / kill
├── sync.go             4 种模式 + tar 流 + git/mtime/glob/list 选择器
├── completion.go       bash / zsh / powershell 补全模板
├── regex.go            cached glob→regex
├── mcp.go              stdio MCP server,14 个工具
├── go.mod / go.sum
└── README.md           本文
```

## 已实现的功能清单(2.6 基线;括注里是后于 Python 0.7.5 加的部分)

**与 Python 0.7.5 对齐的核心**

- [x] `init` / `config list / use / remove / show / set`(布尔/数字/null 自动转型)
- [x] `use` / `use --clear` / `use`(无参 = 显示状态)
- [x] `cd` / `pwd` / `status` / `check`(9 类诊断 + 针对性建议)
- [x] `run` / `exec`(默认子命令)、`-t` TTY、`-P` profile 覆盖
- [x] `push` / `pull`(本地是目录自动 -r,SFTP 实现)
- [x] `sync` —— git(--all/--staged/--modified/--untracked)、mtime(--since)、glob(--include)、list(--files / `--`)、--exclude / --root / --no-git / --dry-run、profile 的 `sync_root` 和 `sync_exclude`
- [x] `-d` detach + `jobs` / `logs <id> [-f]` / `kill <id> [-9 | --signal=NAME]`
- [x] `sessions list / show / clear / prune`(Windows 进程树跳过中间 exe)
- [x] `completion bash / zsh / powershell` —— 含远端 tab 补全
- [x] `mcp` —— 14 工具,协议 2024-11-05
- [x] `_profiles` / `_ls`(补全内部用)、`help` / `version`
- [x] 全局 flag `-P` / `-t` / `-d`,环境变量 `SRV_HOME` / `SRV_PROFILE` / `SRV_SESSION`
- [x] Session 检测的 Windows 进程树游走、known_hosts accept-new、ssh-agent + identity_file + 默认密钥 fallback + passphrase 交互

**Go 版本之后追加(超出 Python 0.7.5)**

- [x] `shell`(原生 PTY 远端 shell,2.0)
- [x] ProxyJump 跳板链 `profile.jump`(2.1+)
- [x] daemon 池化连接(`srv daemon` / status / restart / stop / logs / prune-cache + auto-spawn,2.2-2.6)
- [x] daemon 协议 streaming(`stream_run`,2.4)+ schema 版本字段(2.4.1)
- [x] sync gzip 压缩 `compress_sync`(2.4.1)+ `sync --watch`(2.x)+ `sync --delete` git 模式(2.6)
- [x] **`tunnel`** —— ssh -L 等价的本地→远端 TCP 转发(2.5)
- [x] **`edit`** —— SFTP 拉到 temp / `$EDITOR` / mtime 改了推回(2.5)
- [x] **`open` / `code` / `diff` / `doctor`** —— 本地工作流辅助(2.6)
- [x] **`config edit`** + **`config default`**(原 `config use`,2.6.1 改名以避开和 `srv use` 的语义碰撞)
- [x] **`env list / set / unset / clear`** + profile 级 `env` 字段,运行远端命令前自动注入(2.6)
- [x] **`srv use` / `srv config default` 无参 + TTY 弹出 ↑↓ 选择器**(`/` 过滤、Enter 选、q 取消;`[this shell]` / `[default]` 双标记区分作用域,2.6.1)

## 已知不同(刻意为之)

- **`multiplex` / `control_persist`**:这两个 profile 键在 Go 版本里是 no-op —— Go 自己持连接,不需要 OpenSSH 的 ControlMaster 概念。读旧 config 不报错。
- **`ssh_options`**:Python 版直接转发为 `ssh -o k=v`。Go 版**当前不支持**(因为不调系统 ssh)。要等价能力可以自己加映射,但常见键(compression / connect_timeout / keepalive_*)profile 已经有专门字段。
- **`compression`**:Go 版默认开,通过 SSH 协商;profile 字段保留语义。
- **`-tt` 强制 TTY**:Go 版 `-t` 直接请求 PTY,效果一致。

## 一同测试 Python 版本

两版**共享同一份 `~/.srv/config.json`**,所以你可以来回切:

```sh
# 用 Go(默认,仓库根)
srv status
# 用 Python(显式)
python ../python/srv.py status
```

`config default <name>` 在哪一版改都对另一版生效。
