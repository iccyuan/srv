# srv

[English](./README.en.md) | 中文

`srv` 是一个跨平台 SSH 工作流工具：在本机配置 profile，在远端执行命令，并保留每个 shell 会话自己的远端 cwd。它支持连接复用、文件传输、批量同步、后台任务、端口转发、日志跟踪、MCP Server（Claude Code / Codex）等能力。单个 Go 二进制运行，不依赖 Python，也不要求系统安装 `ssh.exe`。

## 目录

- [安装与快速开始](#安装与快速开始)
- [命令总览](#命令总览)
- [1. 基础与帮助](#1-基础与帮助)
- [2. Profile 与会话](#2-profile-与会话)
- [3. 远端执行](#3-远端执行)
- [4. 文件传输与同步](#4-文件传输与同步)
- [5. 后台任务](#5-后台任务)
- [6. 日志与实时视图](#6-日志与实时视图)
- [7. 端口转发](#7-端口转发)
- [8. 批量执行与 sudo](#8-批量执行与-sudo)
- [9. 本地辅助与诊断](#9-本地辅助与诊断)
- [10. 集成、服务与界面](#10-集成服务与界面)
- [11. 配置文件与环境变量](#11-配置文件与环境变量)
- [开发](#开发)

## 安装与快速开始

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/iccyuan/srv/main/get.sh | sh
```

Windows PowerShell:

```powershell
iwr -useb https://raw.githubusercontent.com/iccyuan/srv/main/get.ps1 | iex
```

安装后可以运行浏览器安装向导：

```sh
srv install
```

从源码构建：

```sh
git clone https://github.com/iccyuan/srv
cd srv/go
go build -o ../srv.exe .    # Windows
go build -o ../srv .        # macOS / Linux
```

最小使用流程：

```sh
srv init                     # 创建 SSH profile
srv check                    # 检查连接
srv status                   # 查看当前 profile / cwd
srv ls -la                   # 在远端当前 cwd 执行命令
srv cd /opt/app              # 设置当前 shell 的远端 cwd
srv "ps aux | grep nginx"    # 带管道的命令需要本地 shell 引号
```

## 命令总览

| 分类 | 命令 |
|---|---|
| 基础与帮助 | `help`, `version`, `completion`, `install` |
| Profile 与会话 | `init`, `config`, `use`, `cd`, `pwd`, `status`, `sessions`, `project`, `env` |
| 远端执行 | `run`, `exec`, 隐式执行 `srv <cmd>`, `shell` |
| 文件传输与同步 | `push`, `pull`, `sync`, `edit`, `open`, `code`, `diff` |
| 后台任务 | `-d`, `jobs`, `logs`, `kill` |
| 日志与实时视图 | `tail`, `journal`, `watch`, `top` |
| 端口转发 | `tunnel` |
| 批量与提权 | `group`, `-G`, `sudo` |
| 诊断与本地辅助 | `check`, `doctor`, `disconnect` |
| 集成、服务与界面 | `mcp`, `guard`, `color`, `daemon`, `ui` |

常用全局选项：

| 选项 | 作用 |
|---|---|
| `-P, --profile <name>` | 本次调用临时指定 profile，优先级最高。 |
| `-G, --group <name>` | 对指定 profile group 并行执行命令。 |
| `-d` | 把远端命令作为后台任务启动。 |
| `-t` | 分配 TTY，用于 `vim`、`htop`、需要交互的 sudo 等。 |
| `--no-hints` | 禁用本次调用的命令拼写提示。 |

## 1. 基础与帮助

| 命令 | 作用 |
|---|---|
| `srv help` / `srv --help` / `srv -h` | 显示完整帮助。 |
| `srv version` / `srv --version` | 显示当前版本。 |
| `srv completion bash` | 输出 Bash 补全脚本。 |
| `srv completion zsh` | 输出 Zsh 补全脚本。 |
| `srv completion powershell` | 输出 PowerShell 补全脚本。 |
| `srv completion <shell> --install` | 自动安装对应 shell 的补全脚本。 |
| `srv install [--no-browser]` | 启动浏览器安装向导，用于 PATH、Claude/Codex MCP、初始 profile 等配置。`--no-browser` 只启动本地服务并打印 URL。 |

## 2. Profile 与会话

### Profile 配置

| 命令 | 作用 |
|---|---|
| `srv init` | 交互式创建或更新 SSH profile。 |
| `srv config list` | 列出所有 profile。 |
| `srv config show [name]` | 查看某个 profile 的完整 JSON 配置。 |
| `srv config default` | TTY 下打开选择器设置全局默认 profile；非 TTY 下打印当前默认值。 |
| `srv config default <name>` | 设置全局默认 profile。 |
| `srv config remove <name>` | 删除 profile。 |
| `srv config set <profile> <key> <value>` | 设置 profile 的某个字段，布尔值和整数会自动转换。 |
| `srv config edit [name]` | 用 `$EDITOR` 编辑 profile JSON。 |
| `srv config global <key> <value>` | 设置全局配置，例如 `lang`、`hints`。 |
| `srv config global <key> --clear` | 清除某个全局配置，回到默认行为。 |

### 快速切换与 cwd

| 命令 | 作用 |
|---|---|
| `srv use` | TTY 下打开 profile 选择器；非 TTY 下显示当前 pin / 默认 profile / 生效 profile。 |
| `srv use <profile>` | 只为当前 shell 会话 pin 一个 profile。 |
| `srv use --clear` | 清除当前 shell 的 profile pin，回落到环境变量或全局默认值。 |
| `srv cd <path>` | 在远端校验路径并把绝对 cwd 保存到当前 shell 会话。 |
| `srv cd` | 把远端 cwd 设置为 `~`。 |
| `srv pwd` | 显示当前解析到的远端 cwd。 |
| `srv status` | 显示当前 profile、target、cwd、会话和关键默认值。 |

Profile 解析优先级：

```text
-P/--profile > srv use 会话 pin > SRV_PROFILE > .srv-project > 全局默认 profile
```

Cwd 解析优先级：

```text
会话 cwd > SRV_CWD > .srv-project.cwd > profile.default_cwd
```

### 会话与项目 pin

| 命令 | 作用 |
|---|---|
| `srv sessions` | 列出所有会话记录。 |
| `srv sessions show` | 查看当前 shell 的会话记录。 |
| `srv sessions clear` | 删除当前 shell 的会话记录。 |
| `srv sessions prune` | 清理 PID 已不存在的旧会话。 |
| `srv project` | 显示当前目录解析到的 `.srv-project` pin。 |

`.srv-project` 示例：

```json
{ "profile": "prod", "cwd": "/srv/app" }
```

### 远端环境变量

| 命令 | 作用 |
|---|---|
| `srv env list` | 列出当前 profile 的远端环境变量。 |
| `srv env set KEY value` | 为当前 profile 设置远端环境变量。 |
| `srv env unset KEY` | 删除一个远端环境变量。 |
| `srv env clear` | 清空当前 profile 的远端环境变量。 |

这些变量会被加到普通远端命令和后台任务前面。

## 3. 远端执行

| 命令 | 作用 |
|---|---|
| `srv <cmd>` | 默认执行形式，在当前远端 cwd 执行命令。 |
| `srv run <cmd>` | 显式执行远端命令，适合命令名和本地子命令冲突时使用。 |
| `srv exec <cmd>` | `run` 的别名。 |
| `srv -t <cmd>` | 分配 TTY 执行命令，例如 `srv -t htop`。 |
| `srv -P <profile> <cmd>` | 本次调用临时指定 profile。 |
| `srv shell` | 打开定位到当前远端 cwd 的交互式远端 shell。 |

带管道、重定向、变量赋值时，需要让本地 shell 把整段命令作为一个参数交给 `srv`：

```sh
srv "ls /var/log | grep error"
srv 'find . -name "*.go"'
srv "FOO=1 python script.py"
srv "bash -ic 'myalias arg'"
```

## 4. 文件传输与同步

### 单次传输

| 命令 | 作用 |
|---|---|
| `srv push <local> [remote]` | 上传文件或目录到远端，远端相对路径基于当前 cwd。 |
| `srv push <local_dir> [remote] -r` | 递归上传目录；目录通常会自动识别。 |
| `srv pull <remote> [local]` | 从远端下载文件或目录。 |
| `srv pull <remote_dir> [local] -r` | 递归下载目录。 |

示例：

```sh
srv push ./app.py
srv push ./dist /opt/app
srv pull logs/app.log
srv pull /etc/hosts ./hosts
```

### 批量同步

| 命令 | 作用 |
|---|---|
| `srv sync` | 在 git 仓库中同步 modified + staged + untracked 文件。 |
| `srv sync --staged` | 只同步 staged 文件。 |
| `srv sync --modified` | 只同步工作区已修改文件。 |
| `srv sync --untracked` | 只同步未跟踪文件。 |
| `srv sync --since 2h` | 按 mtime 同步最近改动文件。 |
| `srv sync --include "src/**/*.go"` | 按 glob 同步，参数可重复。 |
| `srv sync --files a.go --files b/c.go` | 显式指定文件列表。 |
| `srv sync -- a.go b/c.go` | 另一种显式文件列表写法。 |
| `srv sync --dry-run` | 预览将同步的文件，不传输。 |
| `srv sync --delete --dry-run` | 预览远端将被删除的 tracked 文件。 |
| `srv sync --delete` | 同步时删除本地已删除的 tracked 远端文件。 |
| `srv sync --delete --yes` | 超过默认删除安全限制时仍执行。 |
| `srv sync --delete-limit 50` | 修改删除安全上限。 |
| `srv sync --exclude "*.log"` | 增加排除规则，可重复。 |
| `srv sync /opt/app` | 指定远端根目录。 |
| `srv sync --root ./subproject` | 指定本地根目录。 |
| `srv sync --no-git` | 禁用 git 自动模式。 |
| `srv sync --watch` | 监听本地文件变化并持续同步。 |

### 编辑、打开、比较

| 命令 | 作用 |
|---|---|
| `srv edit <remote_file>` | 拉取远端文件到临时目录，用 `$EDITOR` 编辑，保存后如有变化再上传回远端。 |
| `srv open <remote_file>` | 拉取远端文件到临时目录并用本地默认应用打开，只读查看。 |
| `srv code [remote_dir]` | 用 VS Code Remote SSH 打开远端目录。 |
| `srv diff <local_file> [remote_file]` | 比较本地文件和远端文件。 |
| `srv diff --changed` | 把当前 git 改动文件逐个和远端对应文件比较。 |

## 5. 后台任务

| 命令 | 作用 |
|---|---|
| `srv -d <cmd>` | 在远端用 `nohup` 启动后台任务，立即返回 job id 和 pid。 |
| `srv jobs` | 列出本地记录的后台任务。 |
| `srv logs <id>` | 查看后台任务远端日志。 |
| `srv logs <id> -f` | 持续跟踪后台任务日志。 |
| `srv kill <id>` | 向远端任务发送 SIGTERM。 |
| `srv kill <id> -9` | 向远端任务发送 SIGKILL。 |
| `srv kill <id> --signal=USR1` | 发送自定义信号。 |

Job 日志保存在远端 `~/.srv-jobs/<id>.log`。job id 支持前缀匹配，前提是没有歧义。

## 6. 日志与实时视图

| 命令 | 作用 |
|---|---|
| `srv tail <path>...` | 查看远端文件尾部。 |
| `srv tail -f <path>...` | 持续跟踪远端文件，SSH 断开后自动重连。 |
| `srv tail -n N <path>` | 指定初始行数。 |
| `srv tail --grep RE <path>` | 只显示匹配正则的行。 |
| `srv journal` | 查看远端 systemd journal。 |
| `srv journal -u UNIT` | 查看指定 systemd unit 的日志。 |
| `srv journal --since TIME` | 从指定时间开始查看 journal。 |
| `srv journal -f` | 持续跟踪 journal。 |
| `srv journal -g RE` | 用 journalctl grep 过滤。 |
| `srv journal -n N` | 指定 journal 行数。 |
| `srv watch <cmd>` | 周期性执行远端命令并原地刷新。 |
| `srv watch -n SECS <cmd>` | 指定刷新间隔。 |
| `srv watch --diff <cmd>` | 高亮变化行。 |
| `srv top` | 从远端流式输出 `top -b`。 |
| `srv top -n SECS` | 指定刷新间隔。 |

## 7. 端口转发

| 命令 | 作用 |
|---|---|
| `srv tunnel 8080` | 一次性本地转发：本地 `127.0.0.1:8080` 到远端 `127.0.0.1:8080`。 |
| `srv tunnel 8080:9090` | 本地 `8080` 到远端 `127.0.0.1:9090`。 |
| `srv tunnel 8080:db:5432` | 本地 `8080` 到远端可访问的 `db:5432`。 |
| `srv tunnel -R 9000:3000` | 反向转发：远端 `9000` 到本地 `3000`。 |
| `srv tunnel add <name> -L <spec> [-P profile] [--autostart]` | 保存一个本地转发定义。 |
| `srv tunnel add <name> -R <spec> [-P profile] [--autostart]` | 保存一个反向转发定义。 |
| `srv tunnel up <name>` | 通过 daemon 启动已保存的 tunnel。 |
| `srv tunnel down <name>` | 停止已保存的 tunnel。 |
| `srv tunnel list` | 列出保存的 tunnel 和运行状态。 |
| `srv tunnel show <name>` | 查看 tunnel 定义详情。 |
| `srv tunnel remove <name>` | 删除 tunnel 定义。 |

一次性 tunnel 前台运行，按 `Ctrl-C` 停止。保存的 tunnel 由 daemon 承载，CLI 退出后仍可继续运行；daemon 退出时 tunnel 也会停止。

## 8. 批量执行与 sudo

### Profile group

| 命令 | 作用 |
|---|---|
| `srv group set <group> <profile...>` | 创建或覆盖一个 profile group。 |
| `srv group list` | 列出所有 group。 |
| `srv group show <group>` | 查看 group 成员。 |
| `srv group remove <group>` | 删除 group。 |
| `srv -G <group> <cmd>` | 对 group 中所有 profile 并行执行远端命令。 |

示例：

```sh
srv group set web web-1 web-2 web-3
srv -G web "uptime"
srv -G web "systemctl restart nginx"
```

### Remote sudo

| 命令 | 作用 |
|---|---|
| `srv sudo <cmd>` | 远端通过 `sudo -S` 执行命令，本地无回显读取密码。 |
| `srv sudo --no-cache <cmd>` | 不使用也不写入 daemon 内存密码缓存。 |
| `srv sudo --cache-ttl 10m <cmd>` | 设置本次密码缓存 TTL，daemon 有上限。 |
| `srv sudo --clear-cache` | 清除当前 profile 的 sudo 密码缓存。 |

密码只缓存在 daemon 进程内存里，不落盘。

## 9. 本地辅助与诊断

| 命令 | 作用 |
|---|---|
| `srv check` | 主动探测 SSH 连接并诊断 key、host、port 等常见问题。 |
| `srv check --rtt` | 测量 SSH 层 RTT、抖动和丢包。 |
| `srv check --rtt --count N` | 指定 RTT 采样次数。 |
| `srv check --rtt --interval 50ms` | 指定 RTT 采样间隔。 |
| `srv doctor` | 输出本地配置、daemon、active profile 等诊断报告。 |
| `srv doctor --json` | 以 JSON 输出诊断报告。 |
| `srv disconnect [profile]` | 关闭某个 profile 的 daemon 连接池。 |
| `srv disconnect --all` | 关闭所有 daemon 连接池。 |

`srv check` 的常见 diagnosis：

| diagnosis | 含义 |
|---|---|
| `no-key` | 服务端拒绝 publickey。 |
| `host-key-changed` | known_hosts 中的 host key 不匹配。 |
| `dns` | 主机名解析失败。 |
| `refused` | 端口拒绝连接。 |
| `no-route` | 网络不可达。 |
| `tcp-timeout` | TCP 连接超时。 |
| `perm-denied` | 通用认证失败。 |

## 10. 集成、服务与界面

### MCP 与安全 guard

| 命令 | 作用 |
|---|---|
| `srv mcp` | 以 stdio MCP server 模式运行，供 Claude Code / Codex 调用。 |
| `srv mcp serve` | 显式启动 MCP server，等价于 `srv mcp`。 |
| `srv mcp stats` | 查看 MCP 相关统计信息。 |
| `srv guard status` | 查看 MCP 高风险操作确认 guard 状态。 |
| `srv guard on` | 打开当前会话的 MCP 高风险确认 guard。 |
| `srv guard off` | 关闭当前会话的 MCP 高风险确认 guard。 |

Claude Code 示例：

```sh
claude mcp add srv --scope user -- /path/to/srv mcp
claude mcp list
```

Codex `~/.codex/config.toml` 示例：

```toml
[mcp_servers.srv]
command = "D:\\WorkSpace\\server\\srv\\srv.exe"
args = ["mcp"]
```

### 颜色主题

| 命令 | 作用 |
|---|---|
| `srv color status` | 查看当前颜色状态。 |
| `srv color on` | 打开 CLI 远端命令输出配色。 |
| `srv color off` | 关闭当前 shell 的配色。 |
| `srv color list` | 列出内置和自定义颜色 preset。 |
| `srv color use [name]` | 使用指定颜色 preset；TTY 下无参数会打开选择器。 |

自定义 preset 放在 `~/.srv/init/*.sh`。

### Daemon 与 UI

| 命令 | 作用 |
|---|---|
| `srv daemon` | 前台运行 daemon，保持 SSH 连接池。 |
| `srv daemon status` | 查看 daemon 状态和连接池。 |
| `srv daemon status --json` | 用 JSON 输出 daemon 状态。 |
| `srv daemon stop` | 停止 daemon。 |
| `srv daemon restart` | 重启后台 daemon。 |
| `srv daemon logs` | 打印自动启动 daemon 的日志。 |
| `srv daemon prune-cache` | 清理 daemon 缓存。 |
| `srv ui` | 打开一屏式 TUI dashboard，显示 profiles、daemon、tunnels、jobs、MCP recent calls 等状态。 |

## 11. 配置文件与环境变量

默认本地目录是 `~/.srv/`，可用 `SRV_HOME` 覆盖。

| 文件 | 作用 |
|---|---|
| `config.json` | profile、groups、tunnels 和全局配置。 |
| `sessions.json` | 每个 shell 会话的 profile pin 和 cwd。 |
| `jobs.json` | 本地记录的后台任务索引。 |
| `mcp.log` | MCP server 生命周期和工具调用日志。 |
| `cache/` | 本地缓存。 |
| `cm/` | ControlMaster socket 目录。 |

远端后台任务日志保存在 `~/.srv-jobs/<id>.log`。

| 环境变量 | 作用 |
|---|---|
| `SRV_HOME` | 覆盖本地配置目录。 |
| `SRV_PROFILE` | 为当前进程指定默认 profile，优先级低于 `-P` 和 `srv use`。 |
| `SRV_SESSION` | 显式指定 session id，适合 CI 或脚本多次调用共享 cwd。 |
| `SRV_CWD` | 没有 session cwd 时的 cwd fallback。 |
| `SRV_LANG` | 指定 UI 语言：`zh`、`en`、`auto`。 |
| `SRV_HINTS` | 设置为 `0`、`false`、`off` 可关闭命令拼写提示。 |
| `SRV_GUARD` | 设置为 `1`、`true`、`on`、`yes` 可强制启用 MCP guard。 |

常用 profile key：

| Key | 默认值 | 作用 |
|---|---|---|
| `host` | 必填 | 远端主机。 |
| `user` | 当前 OS 用户 | SSH 用户名。 |
| `port` | `22` | SSH 端口。 |
| `identity_file` | 空 | 私钥路径；空表示使用默认 SSH key 搜索。 |
| `default_cwd` | `~` | 新 session 的初始远端 cwd。 |
| `multiplex` | `true` | 启用连接复用。 |
| `compression` | `true` | 启用 SSH 压缩。 |
| `connect_timeout` | `10` | 连接超时秒数。 |
| `keepalive_interval` | `30` | SSH keepalive 间隔秒数。 |
| `keepalive_count` | `3` | 允许失败的 keepalive 次数。 |
| `control_persist` | `10m` | ControlMaster 空闲保留时间。 |
| `dial_attempts` | `1` | TCP/SSH 初始连接重试次数。 |
| `dial_backoff` | `500ms` | 初始重试退避时间。 |
| `sync_root` | 空 | `srv sync` 默认远端根目录。 |
| `sync_exclude` | `[]` | profile 级同步排除规则。 |
| `compress_sync` | `true` | 同步 tar 流是否 gzip 压缩。 |
| `env` | `{}` | 远端命令前置环境变量。 |
| `jump` | `[]` | ProxyJump 链。 |
| `ssh_options` | `[]` | 原始 SSH `-o` 选项，最后追加。 |

## 开发

```sh
cd go
go test ./...
go build -o ../srv.exe .    # Windows
go build -o ../srv .        # macOS / Linux
```

启用仓库自带 pre-commit hook：

```sh
git config core.hooksPath .githooks
```

发布由 GitHub Actions + goreleaser 驱动。打 `vX.Y.Z` tag 后会构建 Linux、macOS、Windows 的 release 包并生成 checksums。
