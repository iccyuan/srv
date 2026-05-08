# srv (Go source)

> `golang.org/x/crypto/ssh` 自实现 SSH 客户端,**不依赖系统 ssh.exe**,跨平台单二进制部署。

## 构建

需要 Go 1.25+。**默认编译到仓库根目录**,这样 `srv` 就是顶层入口、PATH 上有仓库根就可用:

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

## 子命令

```
srv init                       配置 profile
srv config <list|default|remove|show|set|edit>
srv use <profile> | --clear
srv cd <path>          srv pwd          srv status        srv check
srv <args...>          srv -t <cmd>     srv -d <cmd>      srv -P <prof> <cmd>
srv push <l> [<r>]     srv pull <r> [<l>]    srv sync [...]
srv doctor [--json]    srv env [list|set|unset|clear]
srv open <remote>      srv code [remote_dir]  srv diff [--changed] <local> [remote]
srv tunnel [-R] <port-spec>
srv install                    浏览器图形化安装器(PATH / Claude MCP / 第一个 profile)
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

或者直接 `srv install` 在浏览器里勾选项。

## 文件 / 包结构

```
go/
├── main.go             入口、全局 flag、派发
├── config.go           ~/.srv/config.json + Profile + ResolveProfile
├── session.go          ~/.srv/sessions.json + 会话 ID 检测公共部分
├── session_unix.go     Unix: os.Getppid()
├── session_windows.go  Windows: 进程树游走,跳过中间 shim 层
├── helpers.go          intToStr, uintToStr, randHex4 等小工具
├── term.go             tty 判定、原始模式、shQuote、base64
├── client.go           SSH 客户端封装(crypto/ssh + sftp)
├── ops.go              高层操作:cd / push / pull / runStream / runCapture
├── check.go            连通性诊断,9 类失败模式 + 修复建议;--rtt 链路质量
├── jobs.go             ~/.srv/jobs.json + spawn / list / log / kill
├── sync.go             4 种模式 + tar 流 + git/mtime/glob/list 选择器
├── completion.go       bash / zsh / powershell 补全模板
├── regex.go            cached glob→regex
├── mcp.go              stdio MCP server,19 个工具
├── install.go          srv install 浏览器图形化安装器(HTTP server + embedded HTML)
├── install.html        安装器 UI(go:embed)
├── install_unix.go     Unix PATH 修改(~/.local/bin 软链或 rc 文件)
├── install_windows.go  Windows User PATH 修改(走 PowerShell)
├── go.mod / go.sum
└── README.md           本文
```

## 已实现的功能清单(2.6 基线)

- [x] `init` / `config list / default / remove / show / set / edit`(布尔/数字/null 自动转型)
- [x] `use` / `use --clear` / `use`(无参 + TTY = ↑↓ 选择器,2.6.1)
- [x] `cd` / `pwd` / `status` / `check`(9 类诊断 + 修复建议,`--rtt` 链路质量)
- [x] `doctor [--json]` —— 本地配置 + daemon + SSH 准备状态
- [x] `run` / `exec`(默认子命令)、`-t` TTY、`-P` profile 覆盖
- [x] `push` / `pull`(本地是目录自动 -r,SFTP 实现,**单文件断点续传**)
- [x] `sync` —— git(--all/--staged/--modified/--untracked)、mtime(--since)、glob(--include)、list(--files)、`--watch`、`--delete` git 模式 + 删除保护
- [x] `-d` detach + `jobs` / `logs <id> [-f]` / `kill <id> [-9 | --signal=NAME]`
- [x] `sessions list / show / clear / prune`
- [x] `tunnel` —— ssh -L / -R 等价的本地↔远端 TCP 转发
- [x] `edit` —— SFTP 拉到 temp / `$EDITOR` / mtime+remote-unchanged 才推回
- [x] `open` / `code` / `diff [--changed]` —— 本地工作流辅助
- [x] `env list / set / unset / clear` + profile 级 `env` 字段,运行远端命令前自动注入
- [x] `install` —— 浏览器图形化安装器(优先 Edge/Chrome --app 模式,回落系统默认浏览器)
- [x] `completion bash / zsh / powershell` —— 含远端 tab 补全
- [x] `mcp` —— **19 工具**,协议 2024-11-05
- [x] `daemon status [--json] / restart / stop / logs / prune-cache` —— 后台连接池
- [x] **网络弹性**:OS-level TCP keepalive(自动)+ SSH-level keepalive(可调)+ daemon 池健康检查 + `dial_attempts/dial_backoff` 拨号重试(opt-in)
- [x] ProxyJump 跳板链 `profile.jump`
- [x] 全局 flag `-P` / `-t` / `-d`,环境变量 `SRV_HOME` / `SRV_PROFILE` / `SRV_SESSION` / `SRV_CWD`
- [x] known_hosts accept-new、ssh-agent + identity_file + 默认密钥 fallback + passphrase 交互
