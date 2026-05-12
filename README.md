# srv

[English](./README.en.md) | 中文

> 跨平台 SSH 命令运行器。一次配置，本地输入命令，远端执行；支持持久 cwd、连接复用、按 shell 隔离会话、后台任务、文件同步、端口转发，并可作为 MCP server 给 Claude Code / Codex 使用。Go 单文件二进制，无需 Python，也不依赖系统 `ssh.exe`。

## 快速速查

| 目标 | 命令 |
|---|---|
| 首次配置并验证连接 | `srv init && srv check` |
| 远程执行命令 | `srv ls -la` / `srv "ps aux \| grep x"` |
| 记住远端工作目录 | `srv cd /opt/app` |
| 当前 shell 切换服务器 | `srv use <profile>` |
| 同步本地变更到远端 | `srv sync` |
| 上传单个文件 | `srv push ./a.py` |
| 下载远端文件 | `srv pull logs/app.log` |
| 对比本地和远端文件 | `srv diff ./a.py` |
| 编辑远端文件 | `srv edit /etc/foo.conf` |
| 打开远端目录到 VS Code | `srv code /opt/app` |
| 端口转发 | `srv tunnel 8080` |
| 后台运行长任务 | `srv -d ./build.sh` |
| 查看后台任务日志 | `srv jobs` / `srv logs <id> -f` |
| 跟踪任意远端日志 | `srv tail [-f] [--grep RE] <path>` |
| 跟踪 systemd 服务日志 | `srv journal -u <unit> [-f]` |
| 周期性命令视图 | `srv watch -n 1 "ps aux \| head"` |
| 远端 top | `srv top` |
| 并行多机执行 | `srv -G <group> <cmd>` |
| 远程 sudo(本地无回显输密码)| `srv sudo <cmd>` |
| 状态总览(profile/daemon/tunnel/job)| `srv ui` |
| 诊断连接问题 | `srv check` / `srv check --rtt` |
| 诊断本地配置 | `srv doctor` |
| 交互式命令 | `srv -t htop` |
| MCP 集成 | `srv mcp` |

## 目录

- [解决什么问题](#解决什么问题)
- [安装](#安装)
- [快速开始](#快速开始)
- [常用命令](#常用命令)
  - [实时观察远端](#实时观察远端)
  - [并行执行(profile groups)](#并行执行profile-groups)
  - [远程 sudo](#远程-sudo)
  - [状态总览(srv ui)](#状态总览srv-ui)
  - [项目级 profile pin(.srv-project)](#项目级-profile-pinsrv-project)
- [Profile 配置](#profile-配置)
- [会话模型](#会话模型)
- [网络稳定性](#网络稳定性)
- [Claude Code / Codex 集成](#claude-code--codex-集成)
- [本地文件](#本地文件)
- [环境变量](#环境变量)
- [排障](#排障)
- [开发](#开发)

## 解决什么问题

本地开发、远端运行时，常见写法是：

```sh
ssh user@host "cd /opt/app && python test.py"
```

这类命令有几个痛点：每次都要重复 `cd`，每次都要握手，多个终端容易互相覆盖状态，长任务断线后也容易丢。`srv` 把这些流程收敛成一组本地命令：

- `srv cd /opt/app` 后，后续 `srv python test.py` 自动在该目录执行。
- daemon 复用 SSH 连接，常用命令不再反复冷启动握手。
- cwd 按 `(shell session, profile)` 隔离，不同终端互不影响。
- `srv -d` 用远端 `nohup` 启动后台任务，日志写到远端。
- MCP 模式提供结构化工具，方便 Claude Code / Codex 调用。

## 安装

### 一行安装

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/iccyuan/srv/main/get.sh | sh
```

Windows PowerShell:

```powershell
iwr -useb https://raw.githubusercontent.com/iccyuan/srv/main/get.ps1 | iex
```

可选环境变量：

- `SRV_VERSION=2.6.5`：安装指定版本，默认 latest。
- `SRV_INSTALL_DIR=~/bin`：指定安装目录，默认 `~/.srv/bin` 或 `%USERPROFILE%\.srv\bin`。

安装后打开新终端，运行：

```sh
srv install
```

它会打开本地浏览器向导，完成 PATH、Claude MCP 注册和首个 profile 配置。

### 从 release 包安装

从 [Releases](https://github.com/iccyuan/srv/releases/latest) 下载对应平台：

| 平台 | 包名 |
|---|---|
| Linux x86_64 | `srv_<ver>_linux_x86_64.tar.gz` |
| Linux arm64 | `srv_<ver>_linux_arm64.tar.gz` |
| macOS Intel | `srv_<ver>_macos_x86_64.tar.gz` |
| macOS Apple Silicon | `srv_<ver>_macos_arm64.tar.gz` |
| Windows x86_64 | `srv_<ver>_windows_x86_64.zip` |

### 从源码构建

需要 Go 1.25+。

```sh
git clone https://github.com/iccyuan/srv
cd srv/go
go build -o ../srv.exe .    # Windows
go build -o ../srv .        # macOS / Linux
```

## 快速开始

```sh
$ srv init
profile name [prod]:
host (ip or hostname): 1.2.3.4
user [admin]: ubuntu
port [22]:
identity file (blank = ssh default):
default cwd [~]: /opt

$ srv check
OK -- connected; key authentication works.

$ srv status
profile : prod
target  : ubuntu@1.2.3.4:22
cwd     : /opt

$ srv ls -la
$ srv cd app
/opt/app
$ srv "ps aux | grep python"
$ srv -t htop
$ srv -d ./long-build.sh
```

## 常用命令

### Profile 管理

```sh
srv init
srv config list
srv config show [name]
srv config default <name>
srv config remove <name>
srv config set <profile> <key> <value>
srv config edit [name]
```

`srv use <profile>` 只影响当前 shell；`srv config default <profile>` 修改全局默认 profile。

优先级从高到低：

```text
-P/--profile > srv use session pin > SRV_PROFILE > config default
```

### 远程执行

```sh
srv <cmd>
srv run <cmd>
srv -t <cmd>
srv -P <profile> <cmd>
srv shell
```

带管道、重定向、通配符等 shell 语法时，需要本地加引号：

```sh
srv "ls /var/log | grep error"
srv 'find . -name "*.go"'
srv "FOO=1 python script.py"
```

### cwd

```sh
srv cd /opt/app
srv cd ..
srv pwd
```

`srv cd` 会在远端验证路径，然后把绝对路径保存到当前 shell session；后续命令自动使用该 cwd。

### 文件传输

```sh
srv push <local> [remote]
srv pull <remote> [local]
srv edit <remote_file>
srv open <remote_file>
srv code [remote_dir]
srv diff <local_file> [remote_file]
srv diff --changed
```

`push` / `pull` 使用 SFTP。大文件中断后再次执行会先校验已有部分是否完全匹配源文件前缀，匹配才从偏移处续传；不匹配会覆盖重传。源和目标大小相同且内容一致时会直接跳过。

### 批量同步

```sh
srv sync
srv sync --staged
srv sync --modified
srv sync --untracked
srv sync --since 2h
srv sync --include "src/**/*.go"
srv sync --files a.go --files b/c.go
srv sync --dry-run
srv sync --delete --dry-run
srv sync --delete --yes
srv sync --delete-limit 50
srv sync --exclude "*.log"
srv sync /opt/app
srv sync --root ./subproject
srv sync --no-git
srv sync --watch
```

默认排除：`.git`、`node_modules`、`__pycache__`、`.venv`、`venv`、`.idea`、`.vscode`、`.DS_Store`、`*.pyc`、`*.pyo`、`*.swp`。

`--delete` 目前只支持 git 模式。建议先运行 `--delete --dry-run`；真实删除默认最多 20 个文件，超过时需要 `--yes` 或 `--delete-limit N`。

### 后台任务

```sh
srv -d <cmd>
srv jobs
srv logs <id>
srv logs <id> -f
srv kill <id>
srv kill <id> -9
srv kill <id> --signal=USR1
```

任务在远端通过 `nohup` 运行，日志保存在 `~/.srv-jobs/<id>.log`，完成时退出码写到 `~/.srv-jobs/<id>.exit`（供 MCP `wait_job` 工具读取）。

#### MCP 长任务模式：`detach` + `wait_job`

MCP 是同步 JSON-RPC，阻塞式 `run` 会占住整个 turn，Claude Code 的 per-tool 超时（默认 60s，`MCP_TOOL_TIMEOUT` 可调）会把超时的 `run` 直接砍掉、UI 上显示红点。**任何预计超过 10s 的命令应使用 `run background=true` 或直接调用 `detach`，再用 `wait_job` 短轮询**：

```
run { command: "npm run build", background: true }  -> job_id（亚秒返回）
wait_job { id, max_wait_seconds: 8 }                -> status=running（job 还没完，调下一次）
wait_job { id, max_wait_seconds: 8 }                -> status=completed exit_code=0 + log tail
                                         （本地 jobs.json 自动清理）
```

`wait_job` 的等待循环跑在远端 bash 里(单次 SSH 往返完成 N 秒等待),`max_wait_seconds` 默认 8,硬上限 15,让 Claude Code 保持响应,不会长时间卡在单次工具调用里。模型可以在两次 wait_job 之间穿插别的工具调用。`status=killed` 表示 PID 在没写 `.exit` 的情况下消失了(被外部 SIGKILL)。

### 实时观察远端

五条命令共享同一个自动重连引擎(SSH 断了指数退避重连,1s → 2s → 4s …封顶 30s),按数据源选:

```sh
srv tail [-f] [-n N] [--grep RE] <path>...     # 任意远端文件
srv journal [-u UNIT] [--since TIME] [-f]      # systemd 服务日志
srv logs <id> [-f]                             # detached job 的输出
srv watch [-n SECS] [--diff] <cmd>             # 周期跑同一条命令,原地刷新
srv top [-n SECS]                              # 流式拉取 `top -b`
```

`tail` / `journal` / `top` 在 SSH 断线时不会退出,会自动重连续看。`watch --diff` 高亮变化行;`srv -t top` 是「pty 原地刷新」版,`srv top` 是「滚动 log」版。

### 并行执行(profile groups)

把一组 profile 命名,然后用 `-G <group>` 一次跑遍:

```sh
srv group set web web-1 web-2 web-3        # 定义组(创建或替换)
srv group list                              # 列出所有组
srv group show web                          # 显示组成员
srv group remove web                        # 删除

srv -G web "uptime"                         # 在三台机器上并行执行
srv -G web "systemctl restart nginx"
```

输出按 profile 分块,末尾有 `N succeeded, M failed.` 总结。退出码 = 所有成员里最大的非零 exit code(dial 失败显示为 255 以区分命令真 exit 1)。MCP 端有对应 `run_group` 工具。

### 远程 sudo

```sh
srv sudo systemctl restart nginx     # 本地无回显输入密码,管道喂 `sudo -S`
srv sudo apt update                  # 5 分钟内复用 daemon 内存里的缓存
srv sudo --no-cache <cmd>            # 每次都重新提示,不缓存
srv sudo --cache-ttl 10m <cmd>       # 自定义 TTL(daemon 端上限 60min)
srv sudo --clear-cache               # 主动清掉当前 profile 的缓存
```

密码用 `term.ReadPassword` 读,本地终端不回显也不进 shell 历史;只缓存在 daemon 进程内存,从不写盘;exit 1(认证失败)时自动失效缓存。

### 状态总览(srv ui)

```sh
srv ui            # 一屏 dashboard,自动刷新(无变化不重绘,不闪烁)
```

显示活跃 profile / cwd / 项目 pin、daemon 状态、saved tunnels(运行中标黄)、recent MCP tool calls(最近 5 次)、detached jobs、最近 sessions。`q` 退出,`r` 强制重绘。

### 项目级 profile pin(.srv-project)

在仓库根放一个 `.srv-project`:

```json
{ "profile": "prod-db", "cwd": "/srv/app" }
```

之后从该目录或任意子目录调用 `srv` 都会自动 pin 这个 profile + cwd,不用每次 `srv use`。对 MCP 特别有用 —— 每个 Claude Code 项目都会自动落到正确的 profile,不会用全局 default 跑错地方。

Profile 优先级:`-P` > `srv use` > `$SRV_PROFILE` > **`.srv-project`** > 全局 default。
Cwd 优先级:session cwd > `$SRV_CWD` > **`.srv-project.cwd`** > `profile.default_cwd`。

`srv project` 看当前解析到的 pin。

### 端口转发

两种用法:一次性前台 vs daemon 托管的命名持久 tunnel。

```sh
# one-shot 前台,Ctrl-C 退出
srv tunnel 8080
srv tunnel 8080:9090
srv tunnel 8080:db:5432
srv tunnel -R 9000:3000

# 命名 + 持久(daemon 托管,CLI 退出后继续运行)
srv tunnel add db -L 5432:db.internal:5432 -P prod --autostart
srv tunnel up db                   # 启动(daemon 必须在跑)
srv tunnel down db                 # 停止
srv tunnel list                    # 列出 saved + 运行状态
srv tunnel show db                 # 单个详情
srv tunnel remove db               # 删除定义
```

`--autostart` 在 daemon 启动时自动拉起。daemon 退出时所有 tunnel 一并停止;再次 `srv daemon` 起来 + autostart 即可恢复。

默认本地监听 `127.0.0.1`。`-R` 是反向转发:远端端口转到本地服务。

### daemon 和补全

daemon 会自动启动，用来复用 SSH 连接和加速远程补全。手动管理命令：

```sh
srv daemon status
srv daemon status --json
srv daemon restart
srv daemon stop
srv daemon logs
srv daemon prune-cache
```

Shell 补全：

```sh
srv completion bash
srv completion zsh
srv completion powershell
```

## Profile 配置

常用字段：

| Key | 默认值 | 说明 |
|---|---|---|
| `host` | 必填 | 远端主机 |
| `user` | 当前系统用户 | SSH 用户名 |
| `port` | `22` | SSH 端口 |
| `identity_file` | 空 | 私钥路径；空值使用默认 key 搜索 |
| `default_cwd` | `~` | 新 session 初始 cwd |
| `compression` | `true` | SSH 传输压缩 |
| `connect_timeout` | `10` | 连接超时秒数 |
| `keepalive_interval` | `30` | SSH keepalive 间隔 |
| `keepalive_count` | `3` | 多少次 keepalive 失败后判定断线 |
| `dial_attempts` | `1` | 初始拨号重试次数 |
| `dial_backoff` | `500ms` | 初始重试等待，逐次翻倍 |
| `sync_root` | 空 | `srv sync` 默认远端根目录 |
| `sync_exclude` | `[]` | profile 级同步排除 |
| `compress_sync` | `true` | sync tar 流 gzip 压缩 |
| `env` | `{}` | 注入远程命令的环境变量 |
| `jump` | `[]` | ProxyJump 链，格式 `[user@]host[:port]` |

示例：

```sh
srv config set prod keepalive_interval 15
srv config set prod dial_attempts 4
srv config set prod sync_root /opt/app
srv env set NODE_ENV production
```

## 会话模型

- Profile：一台服务器及其连接参数。
- Session：一个本地 shell。Windows 会尽量跳过中间 shim，找到真实 shell。
- cwd：按 `(session, profile)` 保存。

因此两个终端即使都使用 `prod`，也可以分别 `srv cd /a` 和 `srv cd /b`，互不影响。

脚本或 CI 可以显式固定 session：

```sh
SRV_SESSION=ci-build-42 srv cd /opt/app
SRV_SESSION=ci-build-42 srv ./run.sh
```

## 网络稳定性

`srv` 对不稳定网络做了几层处理：

| 层 | 默认 | 作用 |
|---|---|---|
| TCP keepalive | 开启，15s | 让死连接尽快暴露 |
| SSH keepalive | 30s，失败 3 次 | 应用层探活 |
| daemon 连接池 | 自动启动 | 避免每次命令重新握手 |
| dial retry | 默认关闭 | 可配置重试初始连接 |

高延迟、偶发丢包网络可参考：

```sh
srv config set <profile> keepalive_interval 15
srv config set <profile> keepalive_count 4
srv config set <profile> dial_attempts 4
srv config set <profile> dial_backoff 800ms
srv config set <profile> connect_timeout 20
srv check --rtt --count 30
```

## Claude Code / Codex 集成

### 普通命令方式

只要 `srv` 在 PATH 中，Claude Code / Codex 可以直接调用：

```sh
srv ls /opt
srv -d "python long.py"
```

### MCP server

```sh
srv mcp
```

Claude Code 示例：

```sh
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp
claude mcp list
```

Codex CLI 示例：

```toml
[mcp_servers.srv]
command = "D:\\WorkSpace\\server\\srv\\srv.exe"
args = ["mcp"]
```

MCP 工具按用途分组:

| 类别 | 工具 |
|---|---|
| Profile / 会话 | `use` `cd` `pwd` `status` `list_profiles` |
| 诊断 | `check` `doctor` `daemon_status` `list_dir` |
| 执行 | `run` `run_stream` `run_group` `detach` `wait_job` `kill_job` `list_jobs` |
| 日志查看 | `tail` `journal` `tail_log` |
| 环境 / 传输 | `env` `diff` `push` `pull` `sync` `sync_delete_dry_run` |

`wait_job` 与 `detach` 配合是长任务的推荐模式(见上文「后台任务」)。`list_dir` 给模型按结构化方式枚举远端目录,比让它拼 `run "ls ..."` 省 token 且不被 ANSI 颜色污染。

#### MCP token-economy gates

为防止"模型一行 `cat /var/log/syslog` 烧光一回合"这种事,几个流式 / 大输出工具加了硬校验,**不通过直接拒绝、message 里给可复用 example**:

| 工具 | 触发条件 | 必须传 |
|---|---|---|
| `tail` | `follow_seconds > 0`(任何值)| `grep` 非空(不能是 `.*` / `.+` 这种 bypass)|
| `journal` | `follow_seconds > 0`(任何值)| `unit` / `since` / `priority` / `grep` 至少一个 |
| `run` | `cat <file>` / bare `dmesg` / 无过滤 `journalctl` / `find /` | 下游加 `\| head`/`\| tail`/`\| grep`/`\| wc` 等限制器,或用专用工具(`tail`/`journal`)|
| `run_stream` | 同 `run` 规则 | 同 `run` 规则 |
| 通用 | `lines` 参数 | `tail` clamp 1000,`journal` clamp 2000,`follow_seconds` clamp 60 |

拒绝 toolResult 带 `rejected_reason="unbounded_streaming"` / `"unbounded_output"` 结构化字段,模型可程序化分支。

为节省上下文,MCP 的大输出会截断(`run` 类 64 KiB 顶),`sync` 等工具只返回必要摘要。

## 本地文件

默认状态目录是 `~/.srv/`，可用 `SRV_HOME` 覆盖：

```text
config.json       profiles 和全局配置
sessions.json     session pin 与 cwd
jobs.json         后台任务索引
cache/            远程补全缓存
daemon.sock       daemon unix socket
daemon.log        自动启动 daemon 的日志
```

远端后台任务日志：`~/.srv-jobs/<id>.log`。

## 环境变量

| 变量 | 说明 |
|---|---|
| `SRV_HOME` | 覆盖本地状态目录 |
| `SRV_PROFILE` | 当前 shell 默认 profile，优先级低于 `srv use` |
| `SRV_SESSION` | 显式 session id |
| `SRV_CWD` | 没有 session cwd 时的 fallback cwd，适合 MCP 项目配置 |
| `SRV_LANG` | UI 语言：`en` / `zh` / `auto` |
| `SRV_HINTS` | `0` / `false` / `off` 禁用 typo hint |
| `SRV_GUARD` | `1` / `true` / `on` / `yes` 强制开启 MCP 高风险确认 gate(优先级高于 `srv guard` session pin)|

## 排障

### `srv check` 失败

先看 diagnosis：

| diagnosis | 含义 |
|---|---|
| `no-key` | 公钥认证失败 |
| `host-key-changed` | known_hosts 中 host key 不匹配 |
| `dns` | 主机名解析失败 |
| `refused` | 端口拒绝连接 |
| `no-route` | 网络不可达 |
| `tcp-timeout` | TCP 超时 |
| `perm-denied` | 权限或认证失败 |

### MCP 看不到工具

MCP server 通常在客户端会话启动时加载。重新打开 Claude Code，或用 `/mcp` 重连。

### 复杂 shell 命令在 MCP 下 JSON parse error

通常是客户端 JSON 编码和多层 shell 引号叠加导致。建议拆成多步，或把脚本 `srv push` 到远端后运行。

### 后台命令不要手写 `nohup ... &`

直接用：

```sh
srv -d <cmd>
```

它会处理 stdout/stderr、PID、日志路径和 job 记录。

## 开发

```sh
cd go
go test ./...
go build -o ../srv.exe .
```

启用仓库自带 pre-commit hook：

```sh
git config core.hooksPath .githooks
```

发布由 GitHub Actions + goreleaser 驱动。推送 `vX.Y.Z` tag 后会生成各平台二进制和 checksums。

## 版本

当前 Go 版本线：`2.6.x`。完整历史见 [CHANGELOG.md](./CHANGELOG.md)。
