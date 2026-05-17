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
cd srv
go build -o srv.exe ./cmd/srv    # Windows
go build -o srv     ./cmd/srv    # macOS / Linux
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
| 诊断与本地辅助 | `check`, `doctor`, `disconnect`, `prune` |
| 集成、服务与界面 | `mcp`, `guard`, `color`, `daemon`, `ui` |
| 历史与钩子 | `history`, `hooks` |
| 剧本 | `recipe` |

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
| `srv settings` | 列出所有应用级配置及当前值。 |
| `srv settings <key> <value>` | 设置应用级配置，key 为 `hints`、`lang`、`default_profile`。 |
| `srv settings <key> --clear` | 清除某个应用级配置，回到默认行为。 |

### 快速切换与 cwd

| 命令 | 作用 |
|---|---|
| `srv use` | TTY 下打开 profile 选择器；非 TTY 下显示当前 pin / 默认 profile / 生效 profile。 |
| `srv use <profile>` | 只为当前 shell 会话 pin 一个 profile。 |
| `srv use --clear` | 清除当前 shell 的 profile pin，回落到环境变量或全局默认值。 |
| `srv cd <path>` | 在远端校验路径并把绝对 cwd 保存到当前 shell 会话。 |
| `srv cd` | 把远端 cwd 设置为 `~`。 |
| `srv cd -` | 回到上一次的 cwd(类似 shell 的 `cd -`,每次成功 `cd` 后自动记录)。 |
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
| `srv sync --diff` | 文件级预览(rsync-itemize 风格):`+ 新文件 > 更新 < 本地比远端旧 = 不变`,自带每条目的大小变化。等价于一个更详细的 dry-run。 |
| `srv sync --diff -v` | 同时列出 `=` 行(完全一致的文件)。 |
| `srv sync --pull --files a b` | 反向(远端 → 本地),显式列表。 |
| `srv sync --pull --include "*.log"` | 反向 + 远端 `find` 通配匹配。 |
| `srv sync --pull` | 远端是 git 仓库时,自动 `git ls-files --modified` 拉远端改动的文件。 |
| `srv sync --delete --dry-run` | 预览远端将被删除的 tracked 文件。 |
| `srv sync --delete` | 同步时删除本地已删除的 tracked 远端文件。 |
| `srv sync --delete --yes` | 超过默认删除安全限制时仍执行。 |
| `srv sync --delete-limit 50` | 修改删除安全上限。 |
| `srv sync --exclude "*.log"` | 增加排除规则，可重复。 |
| `.srvignore` 文件 | 放在 sync 根目录，gitignore 风格规则；启动 `srv sync` 时自动合并到排除列表。`!` 开头的模式反向放行，可覆盖 `DefaultExcludes` 和 `profile.sync_exclude`。 |
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
| `srv jobs --watch [-n 2s]` | 自刷新 TUI。`+ alive  x exited  ? unreachable`,`q`/`Esc`/`Ctrl-C` 退出。`-n` 控制刷新间隔(默认 2s,最小 100ms)。 |
| `srv jobs notify` | 查看通知配置。 |
| `srv jobs notify on` / `off` | 开关 OS 本地通知(macOS osascript / Linux notify-send / Windows PowerShell)。 |
| `srv jobs notify webhook <URL>` | 设置 webhook,job 完成时 POST JSON。`-` 清空。 |
| `srv jobs notify test` | 发一次样本通知,验证配置。 |
| `srv logs <id>` | 查看后台任务远端日志。 |
| `srv logs <id> -f` | 持续跟踪后台任务日志。 |
| `srv kill <id>` | 向远端任务发送 SIGTERM。 |
| `srv kill <id> -9` | 向远端任务发送 SIGKILL。 |
| `srv kill <id> --signal=USR1` | 发送自定义信号。 |

### Supervisor / 资源限制(`srv run` 和 `srv -d` 都支持)

| 选项 | 作用 |
|---|---|
| `--restart-on-fail [N]` | 命令非零退出时自动重启;`N` 省略 = 不限次数。`SIGINT/SIGTERM` 退出会跳出循环。 |
| `--restart-delay <duration>` | 重启间隔(默认 `5s`,接受 `2s`/`30s`/`1m` 等)。 |
| `--cpu-limit <pct>` | 通过 `systemd-run --user --scope --property=CPUQuota=...` 限制 CPU(`50%`、`200%` 等);未安装 systemd-run 时打 warning 仍执行。 |
| `--mem-limit <size>` | 同上,`MemoryMax`(`512M`、`2G` 等)。 |

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
| `srv tunnel add <name> -L <spec> [-P profile] [--autostart] [--on-demand]` | 保存一个本地转发定义。`--on-demand` 让 daemon 先开本地监听,**第一次客户端连进来才**发起 SSH 拨号(后续复用,SSH 断开会自动重拨)。 |
| `srv tunnel add <name> -R <spec> [-P profile] [--autostart]` | 保存一个反向转发定义。(反向不支持 `--on-demand` —— 远端监听必须在 SSH 会话上注册。) |
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
| `srv check --rotate-key` | 生成新的 ed25519 key 推到远端 `authorized_keys`,验证可用,然后把 `profile.identity_file` 切到新 key。新 key 落到 `~/.srv/keys/<profile>-<time>{,.pub}`。 |
| `srv check --rotate-key --revoke-old` | 完成上述流程后,把原来 `profile.identity_file` 对应的公钥从 `authorized_keys` 删掉。 |
| `srv check --bandwidth [--duration 5s]` | 双向流量测量:远端 `dd /dev/zero` 流给本地 / 本地 `/dev/zero` 流给远端 `cat > /dev/null`,各方向跑 `duration` 秒(每方向上限 256 MiB),输出 Mbps 并给出 verdict 与方向差判定。 |
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

### 清理累积缓存 (`srv prune <target>`)

留活/留近、只删陈旧 —— 每个 target 只丢弃陈旧部分,保留运行中/最近的;整文件清空是另一个动词(如 `srv stats --clear` / `srv sessions clear`)。target 可 Tab 补全。

| 命令 | 作用 |
|---|---|
| `srv prune jobs` | 删本地账本(`jobs.json`)里**已完成**的 job 记录,运行中的保留。 |
| `srv prune jobs <id>` | 只删指定 id 的已完成记录(该 job 仍在运行则报错,先 `srv kill`)。 |
| `srv prune sessions` | 删 PID 已死的 session 记录,存活的保留(等价于 `srv sessions prune`)。 |
| `srv prune mcp-log` | 把 `mcp.log` 裁剪到最近约 256 KB 尾部(按行边界)。 |
| `srv prune mcp-stats` | 删 `mcp-stats.jsonl` 里超过 7 天的遥测行(含轮转副本 `.1`)。 |
| `srv prune all` | 上面四项一次跑完。 |
| `srv prune jobs --remote` / `srv prune all --remote` | 额外删服务器 `~/.srv-jobs/` 里**已完成**作业的 `*.log` + `*.exit`(以远端 `.exit` 标记判定完成,运行中的绝不触碰)。`--remote` 必须显式 opt-in,绝不隐含,且只对 `jobs`/`all` 生效。 |

## 10. 集成、服务与界面

### MCP 与安全 guard

guard **默认开启**。内置拦截集(命中后该次 MCP 调用需带 `confirm=true` 才放行):

- **不可逆破坏**:`rm -rf`、`dd of=`/`if=/dev/{zero,random,urandom}`、`mkfs`、`:> /path`、`> /dev/{sd,nvme,r?disk,hd}`(`/dev/rdisk0` 等 macOS 裸盘也算;`>/dev/null`、`2>/dev/null`、`/dev/zero` 不受影响)。
- **数据库**:SQL `DROP DATABASE/TABLE/SCHEMA/KEYSPACE`、`TRUNCATE TABLE`;MongoDB `dropDatabase()`/`db.<coll>.drop()`;Redis `FLUSHALL`/`FLUSHDB`;PostgreSQL `dropdb`。
- **macOS 磁盘**:`newfs_*`、`diskutil erase*/partitionDisk/zeroDisk/secureErase/apfs delete*`(`diskutil list/info/mount` 不拦)。
- **主机电源**:`shutdown`、`reboot`、`halt`、`poweroff`。

- **DB 客户端引号内 payload**:`mysql -e "DROP DATABASE x"`、`psql -c "..."`、`cqlsh -e`、`mongosh --eval "db.dropDatabase()"`/`db.x.drop()` 也会拦(匹配锚在未加引号的客户端二进制上;`echo "mysql -e ..."` 整体被引号包住时仍不误杀)。

纯前置类 `chattr -i` **不在**默认集,需要的话用 `srv guard rules add` 自行加。**残留局限**:仅 DB 客户端的 `-e/--eval/-c` 直传形式被覆盖,把 SQL 藏进文件 `mysql < f.sql` 或 heredoc 这类间接形式看不到(命中可带 `confirm=true` 绕过)。

**关闭 guard 的两种粒度**(生效优先级:`SRV_GUARD` 环境变量 > 当前 shell 的 `srv guard on/off` > 全局 config > 内置默认开):

- `srv guard off` —— 只关**当前 shell**。注意:MCP server 是 Claude Code 拉起的子进程,session id(Unix 下按父进程 pid 取)和你的交互 shell 不一样,所以这条**关不掉 MCP server 的 guard**。
- `srv guard off --global` —— 写进 `config.json`(机器级),**MCP server 也会读到**且每次调用实时重读,无需重启。这是要对 AI/MCP 路径关闭 guard 时应该用的命令。`srv guard on --global` 恢复。
- `srv guard status` —— 显示当前生效状态及它来自哪一层(env / session / global / default)。

`SRV_GUARD=on|off` 环境变量优先级最高,适合写进 `claude mcp add` 注册里。

| 命令 | 作用 |
|---|---|
| `srv mcp` | 以 stdio MCP server 模式运行，供 Claude Code / Codex 调用。 |
| `srv mcp serve` | 显式启动 MCP server，等价于 `srv mcp`。 |
| `srv mcp stats` | 查看 MCP 相关统计信息。 |
| `srv guard status` | 查看 MCP 高风险操作确认 guard 状态。 |
| `srv guard on` | 打开当前会话的 MCP 高风险确认 guard。 |
| `srv guard off` | 关闭当前会话的 MCP 高风险确认 guard。 |
| `srv guard test "<cmd>"` | dry-run:对照当前规则集判断 `<cmd>` 会不会被 guard 拦截。 |
| `srv guard rules list` | 看当前规则 + allow 列表。`defaults: on/off` 行控制是否启用内置规则。 |
| `srv guard rules add <name> <regex>` | 新增/替换一条 deny 规则。 |
| `srv guard rules rm <name>` | 删除一条 deny 规则。 |
| `srv guard rules allow <regex>` | 加一条放行正则(命中 allow 的命令绕过所有 deny)。 |
| `srv guard rules allow rm <regex>` | 删除一条 allow 正则。 |
| `srv guard rules defaults off` | 关掉内置 deny 规则(只用自定义规则)。 |
| `srv mcp replay` | 列出最近 20 条 MCP `tools/call`(args + result 完整记录)。`-n N` 改条数;`--tool NAME` 过滤;`--since 1h` 时间过滤;`--json` 输出 JSONL。 |
| `srv mcp replay show <idx>` | 看某一条 call 的完整参数 + 结果。 |
| `srv mcp replay clear` / `path` | 清空回放文件 / 打印路径。回放写入 `~/.srv/mcp-replay.jsonl`(单独于 `mcp-stats.jsonl`,因为 args 里可能含敏感命令)。 |

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
| `srv ui` | 打开一屏式 TUI dashboard，显示 profiles、daemon、tunnels、jobs、MCP recent calls 等状态。dashboard 内按 `H` 弹出最近 CLI 命令历史(来自 `~/.srv/history.jsonl`),↑/↓ 或 `j/k` 滚动,任意键关闭。 |

### 命名剧本 (`srv recipe`)

把多步常用流程命名保存,后续 `srv recipe run <name> [args]` 一键执行。变量替换:

- 位置参数 `$1`..`$9` —— 来自命令行尾部的位置参数。
- 命名参数 `${KEY}` 或 `$KEY` —— 来自 `KEY=value` 形式的 kw 参数。
- 引用未定义变量自动塌缩成空串。

| 命令 | 作用 |
|---|---|
| `srv recipe list` / `show <name>` | 列出所有剧本 / 查看一个剧本(`variables:` 行列出它需要的参数)。 |
| `srv recipe save <name> [--profile P] [--desc "..."] [--ignore-errors] -- step1 ;; step2 ...` | 保存剧本。步骤用 `;;` 切分(留出 `;` 给 shell 用)。`--ignore-errors` 让单步失败也继续。 |
| `srv recipe run <name> [pos...] [KEY=val...]` | 执行,按顺序跑每个步骤;非零退出且未 `--ignore-errors` 时立即中止。 |
| `srv recipe rm <name>` | 删除剧本。 |

示例:

```sh
srv recipe save deploy --desc "rolling deploy" -- \
  'rsync ./build/ /srv/app/' ';;' \
  'systemctl restart ${SVC}' ';;' \
  'tail -n 20 /var/log/${SVC}.log'
srv recipe run deploy SVC=myapp
```

### 命令历史

`srv` 把每次走 CLI 远端执行(包括隐式 `srv <cmd>` 和显式 `srv run`)记录到 `~/.srv/history.jsonl`,字段含 profile、host、cwd、命令、退出码、shell session id。MCP 走自己的统计通道,不写入这里。

| 命令 | 作用 |
|---|---|
| `srv history` | 列出当前 shell 最近 50 条远端命令。`!` 标记非零退出。 |
| `srv history -n 200` | 改最大条数。 |
| `srv history --profile prod` | 只看某个 profile 的历史。 |
| `srv history --grep RE` | 按命令字符串正则过滤。 |
| `srv history --all` | 跨所有 shell 看全量历史。 |
| `srv history --json` | 原始 JSONL,适合 `jq` / 脚本消费。 |
| `srv history clear` | 截断历史文件。 |
| `srv history path` | 打印 `history.jsonl` 路径。 |

文件超过 ~25k 条时,自动保留最近 20k 条;无需手动清理。

### 生命周期钩子

`srv hooks` 允许在 `srv` 自己的命令前后跑本地 shell 命令,常用场景:同步前/后跑 lint、cd 后发通知、push 完触发部署等。钩子在本地 `sh -c`(Unix)或 `cmd /c`(Windows)中执行,失败不影响主命令。

事件:`pre-cd` `post-cd` `pre-sync` `post-sync` `pre-run` `post-run` `pre-push` `post-push` `pre-pull` `post-pull`。

| 命令 | 作用 |
|---|---|
| `srv hooks` / `srv hooks list` | 列出所有已配置的钩子。 |
| `srv hooks events` | 列出可用事件名。 |
| `srv hooks set <event> <cmd>` | 替换该事件的全部命令为 `<cmd>`。 |
| `srv hooks add <event> <cmd>` | 在该事件上追加一条命令。 |
| `srv hooks show <event>` | 看某个事件的所有命令。 |
| `srv hooks rm <event>` | 删除该事件的所有命令。 |
| `srv hooks rm <event> <idx>` | 只删第 `<idx>` 条。 |
| `srv hooks run <event>` | 手动触发,便于调试。 |

钩子可读到以下环境变量(只有当前事件相关的会被设置):

| 变量 | 含义 |
|---|---|
| `SRV_HOOK` | 事件名(`pre-cd` 等)。 |
| `SRV_PROFILE` | 当前生效的 profile 名。 |
| `SRV_HOST` / `SRV_USER` / `SRV_PORT` | 远端主机信息。 |
| `SRV_CWD` | 远端 cwd(对 sync 来说是本地根目录)。 |
| `SRV_TARGET` | cd 的新目录 / sync 远端根 / push、pull 的绝对路径 / run 的命令。 |
| `SRV_LOCAL` | push / pull 的本地路径(或 sync 的本地根)。 |
| `SRV_EXIT_CODE` | 仅 `post-*`:远端命令退出码。 |

示例:

```sh
srv hooks add post-sync 'notify-send "synced to $SRV_PROFILE ($SRV_EXIT_CODE)"'
srv hooks add post-push 'curl -fsSL https://hooks.example.com/deploy/$SRV_PROFILE'
srv hooks set pre-sync 'cd $SRV_LOCAL && go vet ./...'
```

## 11. 配置文件与环境变量

默认本地目录是 `~/.srv/`，可用 `SRV_HOME` 覆盖。

| 文件 | 作用 |
|---|---|
| `config.json` | profile、groups、tunnels、hooks 和全局配置。 |
| `sessions.json` | 每个 shell 会话的 profile pin、cwd 和上一次 cwd(供 `srv cd -`)。 |
| `jobs.json` | 本地记录的后台任务索引。 |
| `history.jsonl` | 每次 CLI 远端执行的命令记录(供 `srv history`)。 |
| `mcp.log` | MCP server 生命周期和工具调用日志。 |
| `cache/` | 本地缓存。 |
| `cm/` | ControlMaster socket 目录。 |

仓库根目录可放一个 `.srvignore`,gitignore 风格,自动合并到 `srv sync` 的排除列表。

`~/.srv/cache/remote-path-<profile>.txt` 缓存远端 `$PATH` 下的可执行文件名,被 shell 补全在 `srv <TAB>` 第一个位置上读出,让 srv 子命令和远端命令(`ls`/`htop`/…)出现在同一份候选里。TTL 1 小时,过期自动重拉,无需手动维护。

远端后台任务日志保存在 `~/.srv-jobs/<id>.log`。

| 环境变量 | 作用 |
|---|---|
| `SRV_HOME` | 覆盖本地配置目录。 |
| `SRV_PROFILE` | 为当前进程指定默认 profile，优先级低于 `-P` 和 `srv use`。 |
| `SRV_SESSION` | 显式指定 session id，适合 CI 或脚本多次调用共享 cwd。 |
| `SRV_CWD` | 没有 session cwd 时的 cwd fallback。 |
| `SRV_LANG` | 指定 UI 语言：`zh`、`en`、`auto`。 |
| `SRV_HINTS` | 设置为 `0`、`false`、`off` 可关闭命令拼写提示。 |
| `SRV_GUARD` | 强制覆盖 MCP guard:`1`/`true`/`on`/`yes` 强制开,`0`/`false`/`off`/`no` 强制关;优先级高于 session 设置。未设时 guard 默认开启。 |
| `SRV_ALLOW_AI_CLI` | 设置为 `1`、`true`、`on`、`yes` 解除“AI agent 禁用裸 CLI 远端操作”限制。默认:检测到 AI 编码 agent 环境(`CLAUDECODE` / `CLAUDE_CODE_ENTRYPOINT` / Codex `CODEX_*` 标记)时,`srv` 的远端子命令(run/push/pull/sync/edit/diff/tail/watch/journal/top/sudo/shell/logs/kill/tunnel/recipe/ui 及隐式 `srv <cmd>` 远端执行)会被**硬拒绝**并提示改用 srv MCP server(MCP 路径不受影响,且带 token/高危 gate)。在 agent 终端里手动操作的人可设此变量绕过。 |
| `SRV_TRANSFER_WORKERS` | 调整 `srv push`/`srv pull`/`srv sync` 目录递归时的并发 goroutine 数(默认 4,范围 1~32)。 |

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
| `agent_forwarding` | `false` | `srv -t <cmd>` 和 `srv shell` 时请求 SSH agent forwarding。需要本地有 `SSH_AUTH_SOCK`。 |
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
go test ./...
go build -o srv.exe ./cmd/srv    # Windows
go build -o srv     ./cmd/srv    # macOS / Linux
```

启用仓库自带 pre-commit hook：

```sh
git config core.hooksPath .githooks
```

发布由 GitHub Actions + goreleaser 驱动。打 `vX.Y.Z` tag 后会构建 Linux、macOS、Windows 的 release 包并生成 checksums。
