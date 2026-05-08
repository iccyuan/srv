# srv

[English](./README.en.md) | 中文

> 跨平台 SSH 命令工具:本地配置,远端执行。持久 cwd / 连接复用 / 会话隔离 / 后台作业。Claude Code / Codex 可通过 Bash 或 MCP 调用。零运行时依赖的单 Go 二进制,内置 SSH 协议(不依赖系统 ssh.exe)。

## 速查

| 想做的事 | 命令 |
|---|---|
| 首次配置 + 验证 | `srv init && srv check` |
| 远端执行命令 | `srv ls -la` / `srv "ps aux \| grep x"` |
| 持久化 cwd | `srv cd /opt/app` |
| 切服务器(本 shell) | `srv use <profile>` |
| 推已变更文件 | `srv sync` |
| 推单个文件 | `srv push ./a.py` |
| 本地诊断 | `srv doctor` |
| 对比本地/远端文件 | `srv diff ./a.py` |
| 端口转发(看远端 dev server) | `srv tunnel 8080` |
| 远端文件本地编辑器改 | `srv edit /etc/foo.conf` |
| VS Code 远程打开远端目录 | `srv code /opt/app` |
| 打开远端文件本地副本 | `srv open logs/app.log` |
| 后台长任务 | `srv -d ./build.sh` |
| 查后台任务 | `srv jobs` / `srv logs <id> -f` |
| 连不上排查 | `srv check` |
| 交互式(vim/htop) | `srv -t <cmd>` |
| Claude Code 集成 | 见 [Claude Code / Codex 集成](#claude-code--codex-集成) |

## 目录

1. [解决什么问题](#解决什么问题)
2. [安装](#安装)
3. [快速开始](#快速开始)
4. [子命令参考](#子命令参考)
5. [profile 可调键](#profile-可调键)
6. [多服务器、多终端](#多服务器多终端)
7. [网络弹性](#网络弹性)
8. [Claude Code / Codex 集成](#claude-code--codex-集成)
9. [文件布局](#文件布局)
10. [环境变量](#环境变量)
11. [故障排查](#故障排查)
12. [设计取舍 / 已知限制](#设计取舍--已知限制)

---

## 解决什么问题

本地没有服务器环境,要反复 `ssh user@host "cd /opt && python test.py"` 太啰嗦,而且每次 ssh 都要走完整握手,弱网下慢。`srv` 把这些工作流抽象成几条命令:

- `srv cd /opt` 之后 `srv python test.py` 自动在 /opt 跑
- 连接自动复用(ControlMaster),后续命令秒回
- 多终端 / 多台服务器互不干扰
- 长任务可以 `srv -d` detach,日志远端落盘
- AI 客户端(Claude Code / Codex)开箱即用

---

## 安装

`srv` 是仓库根目录下的单二进制,由 [`go/`](./go) 源码编译。

### 前置

- Go 1.25+(只构建时需要——https://go.dev/dl/)
- 远端服务器跑了 OpenSSH(本地不需要 ssh 客户端,Go 二进制自实现 SSH 协议)

### 编译

```sh
cd go
go build -o ../srv.exe .          # Windows
go build -o ../srv     .          # macOS / Linux
```

### 安装(把 srv 加到 PATH)

仓库根有一键脚本,**自动识别脚本所在目录**(不依赖 D:\WorkSpace 这种硬编码路径),克隆到哪儿都能直接跑;**幂等**,跑两次不会重复加。

**Windows(PowerShell)**:

```powershell
.\install.ps1                    # 加到 User PATH
.\install.ps1 -Uninstall         # 卸载
```

新开 PowerShell,`srv version` 应当显示 `srv 2.x.x`。**已开的窗口需要重开**才能看到 PATH 变化。

**macOS / Linux**:

```sh
./install.sh                     # 装(优先 ~/.local/bin 符号链接,否则改 rc 文件)
./install.sh --uninstall         # 卸
```

策略:
- `~/.local/bin` 已经在 PATH 里 → 在那建 `srv` 符号链接(最干净,后续 `go build` 自动生效)
- 否则 → 在合适的 rc 文件(zsh/bash/fallback `~/.profile`)追加一行 `export PATH=...`,带 marker 注释,卸载时干净移除

装完按提示 `exec $SHELL -l` 或新开终端,`srv version` 验证。

**手动加(不想跑脚本时)**:把仓库根的绝对路径加到 PATH 即可,任何方式都行。

---

## 快速开始

```sh
$ srv init
profile name [prod]:
host (ip or hostname): 1.2.3.4
user [admin]: ubuntu
port [22]:
identity file (blank = ssh default):
default cwd [~]: /opt
saved profile 'prod' to ~/.srv/config.json

$ srv status
profile : prod (default)
target  : ubuntu@1.2.3.4:22
cwd     : /opt
session : 11872
defaults: multiplex=True  compression=True  connect_timeout=10s

$ srv ls -la                     # 在远端 /opt 下跑 ls
$ srv cd app                     # 切到 /opt/app
/opt/app
$ srv "ps aux | grep python"     # 含管道:本地引号,远端执行
$ srv -t htop                    # 交互式
$ srv -d ./long-build.sh         # 后台
```

---

## 子命令参考

### profile 管理

```
srv init                            # 交互式向导添加 profile
srv config list                     # 列出 profile;* = 全局默认,@ = 当前 session 已 pin
srv config show [name]              # 输出 profile 的完整 JSON
srv config default <name>           # 设全局默认(persists 到 ~/.srv/config.json,所有 shell 共用)
srv config default                  # TTY 下:↑↓ 弹窗选默认;非 TTY:打印当前默认
srv config remove <name>            # 删 profile
srv config set <prof> <key> <val>   # 改单个键(true/false/数字/null 自动转型)
srv config edit [name]              # 用 $EDITOR 编辑单个 profile JSON
srv env list                        # 列出 profile 级远端环境变量
srv env set KEY value               # 运行远端命令前自动注入 KEY=value
srv env unset KEY                   # 删除一个环境变量
srv env clear                       # 清空当前 profile 的环境变量
```

### profile 快切

**两个作用域要分清**(选错了就是经典翻车):

| 命令 | 作用范围 | 持久化 |
|---|---|---|
| `srv use <name>` | **当前 shell 一会话**(pin 在 `~/.srv/sessions.json`) | 该 shell 退出即没 |
| `srv config default <name>` | **全局**(写 `~/.srv/config.json` 的 `default_profile`) | 永久,所有 shell 共用 |

`srv use` 是临时切,`config default` 是改默认。两者不冲突 —— shell pin 优先级高于 default。

```
srv use                # TTY 下:↑↓ 弹窗选(/ 过滤,Enter 选,q 取消)
srv use <profile>      # 直接 pin
srv use --clear        # 取消 pin
```

弹窗里行尾标记会区分 `[this shell]`(黄,本 shell 已 pin)和 `[default]`(青,全局默认),两者可同时出现。

`srv use` 在非 TTY(管道、脚本、CI)下保持原行为:打印当前 pin / default / active 状态。

**优先级**(高 → 低):

```
-P/--profile (单条命令)
  > srv use 设的 session pin
  > $SRV_PROFILE 环境变量
  > srv config default 设的全局默认
```

### 远端命令执行

```
srv <args...>            # 默认在当前 cwd 跑
srv run <args...>        # 显式语法(用于子命令名冲突,如 srv run pwd)
srv -t <cmd>             # 分配 TTY(vim / htop / sudo 输密码)
srv -P <profile> <cmd>   # 单次命令切 profile
srv -d <cmd>             # 后台执行(见下文)
```

含 shell 元字符的命令要**本地引号**——`srv` 把所有 args join 后整段交给远端 shell:

```sh
srv "ls /var/log | grep error"
srv 'find . -name "*.py"'
srv "FOO=1 python script.py"            # 一次性环境变量
srv "bash -ic 'myalias arg'"            # 走交互 shell 取别名
```

### 连通性诊断

```
srv check        # 用 BatchMode=yes 短超时探一次连接,失败时给出针对性修复指引
```

不会 hang(关掉 ControlMaster + 不读 stdin),自动接受首次连接的 host key。失败分类:

| diagnosis | 含义 | 提示输出 |
|---|---|---|
| `no-key` | 服务器拒绝 publickey 认证 | 给出 `ssh-copy-id` 命令和 PowerShell 等价管道 |
| `host-key-changed` | host key 不匹配 | 给出 `ssh-keygen -R` + `ssh-keyscan` 命令 |
| `dns` | 主机名解析失败 | 提示检查 host 拼写 |
| `refused` | 连接被拒 | sshd 没起 / 端口错 / 防火墙 |
| `no-route` | 网络不可达 | VPN / 路由问题 |
| `tcp-timeout` | TCP 超时 | 服务器宕 / 防火墙静默丢包 |
| `perm-denied` | 一般 auth 失败 | 检查 key 配对 |

`srv init` 完成后会提示你紧接着跑一次 `srv check`,初次配完立刻知道能不能用。

### cwd

```
srv cd <path>    # 远端验证 cd <path> && pwd,把绝对路径写到当前 session
srv cd           # cd 到 ~
srv pwd          # 显示
```

`srv cd` 是被本地拦截的子命令——不会真在远端 bash 跑 cd(那种 cd 不会跨 ssh 调用持久化)。状态存到 sessions.json,**每个终端独立**。

### 文件传输(scp)

```
srv push <local> [<remote>] [-r]    # 上传(local 是目录时自动 -r)
srv pull <remote> [<local>] [-r]    # 下载
```

远端路径相对当前 cwd:
- `srv push ./a.py` → 上传到 `<cwd>/a.py`
- `srv push ./dist /opt/app` → 上传到 `/opt/app`
- `srv pull logs/app.log` → 从 `<cwd>/logs/app.log` 下载到本地 `.`
- 绝对路径(`/...`)和 `~/...` 原样传

### 批量同步已变更文件

走 `tar -cf - | ssh remote tar -xf -` 单条 ssh 流式传输,保留相对路径,配合 ControlMaster 几乎零握手开销。

```
srv sync                              # 在 git 仓库:modified+staged+untracked
srv sync --staged                     # 只传 git add 过的
srv sync --modified                   # 只传 working-tree 改动
srv sync --untracked                  # 只传 untracked
srv sync --since 2h                   # mtime 选(2h/30m/1d/90s)
srv sync --include "src/**/*.py"      # glob 选,可重复
srv sync --files a.py --files b/c.py  # 显式列表;也可 `srv sync -- a.py b.py`
srv sync --dry-run                    # 预览要传的文件,不真传
srv sync --delete --dry-run           # 预览本地已删除 tracked 文件对应的远端删除
srv sync --delete                     # 同步后删除这些远端文件
srv sync --delete --yes               # 超过默认删除保护阈值时仍执行
srv sync --delete-limit 50            # 调整删除保护阈值(默认 20)
srv sync --exclude "*.log"            # 追加排除,可重复
srv sync /opt/app                     # 显式远端根(默认 = sync_root 或当前 cwd)
srv sync --root ./subproject          # 显式本地根(默认 = git 顶层 / 当前目录)
srv sync --no-git                     # 在 git 仓库里也不走 git 模式
srv sync --watch                      # 持续监听本地变更并同步
```

默认排除:`.git`、`node_modules`、`__pycache__`、`.venv`、`venv`、`.idea`、`.vscode`、`.DS_Store`、`*.pyc`、`*.pyo`、`*.swp`。`list` 模式(`--files`)不应用默认排除——显式用户列表无条件传。

文件以 git 顶层(git 模式)或当前目录(其它模式)为锚点,远端在 `remote_root/<relative_path>` 落盘。

### 端口转发(`srv tunnel`)

`ssh -L` / `ssh -R` 等价。常见场景:远端跑 dev server / Jupyter / DB,本地浏览器或客户端连;也可以把远端端口反向打到本地服务。

```
srv tunnel 8080            # 本地 127.0.0.1:8080  ->  远端 127.0.0.1:8080
srv tunnel 8080:9090       # 本地 127.0.0.1:8080  ->  远端 127.0.0.1:9090
srv tunnel 8080:db:5432    # 本地 127.0.0.1:8080  ->  db:5432(在远端解析主机名)
srv tunnel -R 9000:3000    # 远端 127.0.0.1:9000 -> 本地 127.0.0.1:3000
```

行为:`Ctrl-C` 停;远端 SSH 连接断开会被检测到并自动停。每个进入连接独立一个 goroutine 双向 copy。本地端口固定绑 `127.0.0.1`,不暴露到 LAN。

> 反向(`-R`,本地服务暴露给远端)按需后加,目前未实现。

### 远端文件本地编辑(`srv edit`)

```
srv edit /etc/nginx/conf.d/api.conf      # 拉到本地 -> $EDITOR -> 改完自动推回
```

流程:SFTP 拉到 `os.MkdirTemp` 临时目录(基名保留方便编辑器识别语法)→ 启 `$VISUAL` / `$EDITOR`(按空格切分,所以 `EDITOR='code --wait'` 这种带参数的 OK)→ 编辑器退出后比 mtime+size,变了就推回,没变打 "no changes"。

Editor 选取顺序:`$VISUAL` → `$EDITOR` → Windows: `notepad.exe` → 其它:`vim` / `vi` / `nano`。

**已知坑**:

- **不上锁**。`srv edit` 保存前会检查远端 size/mtime,发现期间被别的会话改过就拒绝覆盖;共享盒子的强并发编辑仍建议直接 ssh 进去用 vim。
- **VS Code 必须 `--wait`**。`EDITOR=code` 不阻塞,srv 会立刻看到"没改"然后退出。改成 `EDITOR='code --wait'`。
- **Notepad 转 CRLF**,Windows 上等同于"整文件都修改了"。建议设 `$EDITOR` 用 vim / notepad++ / `code --wait`。

### 后台作业

### 本地辅助命令

```
srv doctor                         # 本地配置 / daemon / active profile 诊断
srv doctor --json                  # JSON 诊断输出
srv open logs/app.log              # 拉远端文件到临时目录并本地打开
srv code /opt/app                  # 用 VS Code Remote SSH 打开远端目录
srv diff ./app.py app.py           # 对比本地文件和远端文件
srv diff --changed                 # 对比 git 变更文件与远端对应文件
```

`srv open` 只打开本地副本,不会推回;需要保存回远端时用 `srv edit`。`srv code` 如果找到 VS Code CLI 会直接执行,否则打印可手动运行的命令。

```
srv -d <cmd>      # nohup + 输出重定向到 ~/.srv-jobs/<id>.log,立即返回 job id 和 pid
srv jobs          # 列出本地 job 记录
srv logs <id>     # cat 远端日志
srv logs <id> -f  # tail -f
srv kill <id>     # SIGTERM
srv kill <id> -9                  # SIGKILL
srv kill <id> --signal=USR1       # 自定义信号
```

job id 形如 `20260506-143052-abc1`(精确到秒 + 随机后缀),可用前缀简写。

实现:命令通过 base64 编码塞进 spawn 行,完全规避嵌套引号问题。

### 会话管理

```
srv sessions          # 列出所有 session 记录(标 alive/dead)
srv sessions show     # 当前 session 的完整 JSON
srv sessions clear    # 删当前 session 记录
srv sessions prune    # GC:删所有 PID 已不存在的 session
```

### daemon 管理

`srv` 在执行 `_ls` / 非 TTY 命令 / `cd` 时会自动起一个 daemon(`~/.srv/daemon.sock`),把 SSH 连接 pool 起来,后续命令省去 ~2.7s 握手。多数情况你不用管它,需要直接操作时:

```
srv daemon                          # 前台跑(主要给调试)
srv daemon status                   # 看池里的 profile / uptime,可读格式
srv daemon status --json            # 同上,机器可读 JSON
srv daemon restart                  # 停掉再后台拉起
srv daemon stop                     # 停
srv daemon logs                     # cat 自动起的 daemon 的 stdout/stderr 日志(~/.srv/daemon.log)
srv daemon prune-cache              # 清掉 _ls 远端补全缓存(~/.srv/cache/)
```

socket 在 `~/.srv/daemon.sock`(Windows 上 AF_UNIX,需 Win10 1803+)。Daemon 30 分钟全空闲会自停;每条 SSH 连接 10 分钟未用会被回收。

### shell 补全(tab 自动补全)

**PowerShell**(永久生效——加到 `$PROFILE`,新开 shell 即用):

```powershell
# 一次性写入,新开 PowerShell 自动加载
"`n# srv tab completion`nsrv completion powershell | Out-String | Invoke-Expression" |
    Add-Content $PROFILE
```

或者只在当前 session 临时启用:

```powershell
srv completion powershell | Out-String | Invoke-Expression
```

**bash**(写进 `~/.bashrc` 永久生效):

```sh
echo 'source <(srv completion bash)' >> ~/.bashrc
```

**zsh**(同 bash,写进 `~/.zshrc`):

```sh
echo 'source <(srv completion zsh)' >> ~/.zshrc
```

**覆盖范围**:

| 你输入 | 补全结果 |
|---|---|
| `srv <TAB>` | 所有子命令(init/config/use/cd/pwd/status/check/run/...) |
| `srv c<TAB>` | 前缀过滤(config/cd/check/completion) |
| `srv config <TAB>` | list/use/remove/show/set |
| `srv config default <TAB>` | 已配置 profile 名 |
| `srv config remove <TAB>` | 已配置 profile 名 |
| `srv config show <TAB>` | 已配置 profile 名 |
| `srv use <TAB>` | profile 名 + `--clear` |
| `srv -P <TAB>` | profile 名 |
| `srv sessions <TAB>` | list/show/clear/prune |
| `srv completion <TAB>` | bash/zsh/powershell |
| `srv push <TAB>` | 本地文件 |
| `srv push <local> <TAB>` | **远端**目录 / 文件 |
| `srv cd <TAB>` / `srv cd /opt/<TAB>` | **远端目录**(只 dirs) |
| `srv pull <TAB>` / `srv pull /etc/<TAB>` | **远端**目录 / 文件 |
| `srv edit <TAB>` / `srv edit /etc/<TAB>` | **远端**目录 / 文件 |

**远端补全**机制:`srv _ls <prefix>` 内部命令在远端跑 `ls -1Ap`,把结果缓存到 `~/.srv/cache/`(5 秒 TTL)。第一次 tab 走完整 SSH 握手(典型 2-3 秒),之后命中缓存秒回(~60ms)。每次 tab 都会用最新 cwd / pinned profile,所以 `srv use` 切换后远端补全自动跟着切。

PowerShell 的脚本会**烧入 srv.exe 的绝对路径**(因为 ArgumentCompleter 作用域里 PATH 不一定可见),所以从任何目录跑都能查 profile 名和远端目录。

---

## profile 可调键

`srv config set <profile> <key> <value>`。布尔字符串(`true`/`false`)和纯数字串自动转型。

| 键 | 默认 | 说明 |
|---|---|---|
| `host` | (必填) | 远端主机 |
| `user` | 当前用户 | SSH 用户名 |
| `port` | 22 | SSH 端口 |
| `identity_file` | null | 私钥路径,留空用 ssh 默认查找 |
| `default_cwd` | `~` | 新 session 进入时的初始 cwd |
| `multiplex` | true | 启用 ControlMaster 连接复用 |
| `compression` | true | SSH 传输压缩 |
| `connect_timeout` | 10 | 握手超时(秒) |
| `keepalive_interval` | 30 | SSH 应用层 KeepAlive 探测间隔(秒);弱网调小到 10-15 |
| `keepalive_count` | 3 | 连续多少次失败后判定断线 |
| `dial_attempts` | 1 | 初始 TCP 拨号 / SSH 握手失败时的重试次数(2.6.4+);弱网设 3-5。Auth / host-key 错误永远不重试 |
| `dial_backoff` | `500ms` | 重试之间的等待起点,每次翻倍至 30s 封顶。`time.ParseDuration` 格式 |
| `control_persist` | `10m` | ControlMaster socket 闲置保留时长 |
| `sync_root` | null | `srv sync` 的默认远端根(命令行不带位置参数时用) |
| `sync_exclude` | `[]` | `srv sync` 的 profile 级追加排除(与默认排除合并) |
| `compress_sync` | true | `srv sync` 的 tar 流走 gzip 压缩(代码 / 文本约 -70%;CPU 单位毫秒) |
| `env` | `{}` | profile 级远端环境变量,在每条远端命令 / detached job 前注入(`srv env ...` 维护) |
| `jump` | `[]` | ProxyJump 跳板链,每项 `[user@]host[:port]`,按数组顺序逐跳 |
| `ssh_options` | `[]` | 任意原始 `-o` 选项数组,**最后**附加(覆盖前面的默认) |

---

## 多服务器、多终端

### 模型

- **profile** = 一台服务器(host + user + port + key + default_cwd 等)
- **session** = 一次 shell 启动。session id = 该 shell 进程的 PID(Windows 下自动跳过 cmd.exe 等中间层,定位到真 shell)
- cwd 按 **(session, profile)** 双键存

### 隔离矩阵

| | 终端 A pin prod | 终端 B pin prod | 终端 C pin dev |
|---|---|---|---|
| 在 A 跑 `srv cd /a` | A.prod.cwd=/a | 不变 | 不变 |
| 在 B 跑 `srv cd /b` | 不变 | B.prod.cwd=/b | 不变 |
| 在 C 跑 `srv cd /c` | 不变 | 不变 | C.dev.cwd=/c |
| 在 A 跑 `srv -P dev cd /x` | A.dev.cwd=/x,A.prod.cwd 不动 | — | — |

A 和 B 用同一个 profile 也不互踩;A 临时切 dev 不会动 dev 在 C 里的 cwd。

### 显式覆盖 session id

```sh
# CI / 脚本里固定 session,跨多个 srv 调用共享 cwd 状态
$ SRV_SESSION=ci-build-42 srv cd /opt
$ SRV_SESSION=ci-build-42 srv ./run.sh
```

---

## 网络弹性

弱网(高 ping、丢包、NAT idle 杀连接)下 srv 的四层防护,从内核到协议:

| 层 | 默认 | 调参 | 作用 |
|---|---|---|---|
| **TCP keepalive(OS 层)** | 始终开,15 秒探测 | 不可配,无副作用 | NAT/防火墙 idle 杀掉的死连接,内核几十秒内察觉,SSH 立刻收到 EOF 而非挂死 |
| **SSH keepalive(应用层)** | 30 秒一次,连续 3 次失败判断线(=90s 内 down) | profile 的 `keepalive_interval` / `keepalive_count` | 服务端正常但 SSH 通道静默(中间节点丢包),客户端主动 ping;同时阻止远端 idle-kill |
| **daemon 池化连接** | 自动起,30 分钟全空闲自停 | 无 | 一次握手多次复用,绕过 ~2.7s 冷握手成本 |
| **拨号重试**(2.6.4+) | 关(`dial_attempts=1`) | profile 的 `dial_attempts` / `dial_backoff` | 第一次 SYN 丢包 / RST 时自动重试,弱网首发失败 90% 自愈 |

**典型弱网调参**(以"高 ping ~250ms,偶尔丢包"为例):

```sh
srv config set <profile> keepalive_interval 15      # 更激进的 SSH 探测
srv config set <profile> keepalive_count 4          # 多容忍一次抖动
srv config set <profile> dial_attempts 4            # 头两次失败也再试两次
srv config set <profile> dial_backoff 800ms         # 退避起步,后续 1.6s / 3.2s / 6.4s,30s 封顶
srv config set <profile> connect_timeout 20         # 单次握手 timeout 抬高(默认 10s)
```

**测链路质量** —— 不知道是 srv 慢还是网慢时:

```
srv check --rtt                  # 默认跑 10 个 SSH 级 RTT 探测,出 min/med/avg/max + loss%
srv check --rtt --count 30       # 跑更长的样本
srv check --rtt --interval 50ms  # 加密采样,看抖动
```

输出末尾的 verdict 会标 `link looks healthy` / `high latency` / `noticeable jitter` / `packet loss is high`,对应该往哪个方向调。

**断点续传** —— `srv push` / `srv pull` 大文件传到一半断网时,2.6.4+ 自动续(只续单文件,目录递归是文件粒度续)。检测条件:远端已有部分文件且 size 严格小于本地 → 从断点续;不匹配 → 全量重传。

**`srv -d` 是真正抗断网的姿势** —— 后台任务用 nohup + 输出落 `~/.srv-jobs/<id>.log`,本地连接断了不影响远端进程,事后 `srv logs <id> -f` / `srv kill <id>` 接回来。

---

## Claude Code / Codex 集成

### 方式 1:Bash 调用

PATH 里有 `srv` 就行,无需额外配置:

```
srv ls /opt
srv -d "python long.py"
```

### 方式 2:MCP server(结构化工具)

Claude Code 通过 stdio MCP 拿到 19 个工具(run/cd/pwd/use/status/check/list_profiles/doctor/daemon_status/env/diff/push/pull/sync/sync_delete_dry_run/detach/list_jobs/tail_log/kill_job)。MCP 服务器实例的 session id = Claude Code 进程 PID,每个 Claude Code 实例独立。

**Claude Code 注册** —— 3 种作用域,按使用场景选一个:

| Scope | 配置写到 | 适用场景 |
|---|---|---|
| `user` | `~/.claude.json` | 所有项目共享,**个人机器推荐** |
| `project` | `<repo>/.mcp.json` | **团队共享**——提交 git 后队友 clone 即用 |
| `local` | 项目+用户级私有文件 | 只在某个项目用,且不想入库 |

```sh
# 1) 个人全局(任何目录里都能用)
claude mcp add srv --scope user -- D:\WorkSpace\server\srv\srv.exe mcp

# 2) 项目级共享(在 repo 根目录跑;生成 .mcp.json,可入 git)
cd <your-project>
claude mcp add srv --scope project -- D:\WorkSpace\server\srv\srv.exe mcp

# 3) 项目级私有(不写进 .mcp.json,只你能看到)
cd <your-project>
claude mcp add srv --scope local -- D:\WorkSpace\server\srv\srv.exe mcp

# 验证(任一 scope 之后都能跑)
claude mcp list   # 应显示  srv: ✓ Connected
```

> macOS / Linux 把路径换成 `/path/to/srv/srv`(无 `.exe`)。或者直接用 `srv mcp` —— 如果 `srv` 在 PATH 上,这是最简的形式。

新开 Claude Code 会话即生效;已运行的会话需要 `/mcp` 重连。

**Codex CLI** ——`~/.codex/config.toml`:

```toml
[mcp_servers.srv]
command = "D:\\WorkSpace\\server\\srv\\srv.exe"
args = ["mcp"]
```

---

## 文件布局

`~/.srv/`(可用 `$SRV_HOME` 改路径,主要用于隔离测试):

```
config.json          所有 profile 定义 + 全局默认
sessions.json        {session_id: {profile, cwds: {profile: cwd}, last_seen, started}}
jobs.json            后台作业本地索引
cm/                  ControlMaster socket,每个 host+user+port 一个 .sock
```

远端 `~/.srv-jobs/<id>.log` 是后台任务的日志(srv 自动创建该目录)。

---

## 环境变量

| 变量 | 作用 |
|---|---|
| `SRV_HOME` | 配置目录的覆盖路径(默认 `~/.srv`) |
| `SRV_PROFILE` | 当前 shell 的默认 profile(优先级低于 `srv use`) |
| `SRV_SESSION` | 显式 session id;脚本/CI 跨多个 srv 调用共享状态时用 |
| `SRV_CWD` | 没有 session cwd 时的回退目录(2.6.2)。MCP 注册里 `"env": {"SRV_CWD": "/mnt/project/foo"}` 让每次新 MCP 会话直接落到该项目目录,不用每次先 `srv cd`。优先级低于 session pin,高于 `profile.default_cwd` |

---

## 故障排查

### `error: 'ssh' not found in PATH`
没装 OpenSSH client。
- Windows: `Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0`(管理员 PowerShell)
- Linux: `apt install openssh-client` 等
- macOS: 默认有

### 握手仍然很慢 / 看起来没复用
- `srv status` 看 `multiplex=True`
- `~/.srv/cm/` 第一次连后该有 `.sock` 文件
- 某些服务器禁用了多路复用:`srv config set <prof> multiplex false`
- `~/.ssh/config` 里有冲突的 ControlPath 配置可能影响

### Windows session id 不稳定 / 每次都不同
- 通过 PATH 上的 `srv.exe` 直接调用应当自动稳定
- 如果链路异常(嵌套多层 shell),手动 `$env:SRV_SESSION = $PID`

### `srv -d` 起的进程立刻退出
- 远端必须有 `bash`、`base64`、`nohup`(coreutils 一般都有)
- 看 `srv logs <id>` 看远端 stderr

### Claude Code 看不到新加的 MCP 工具
MCP 服务器在 Claude Code 会话启动时加载。**新开 Claude Code 会话**或 `/mcp` 重连后才会生效。

### MCP `run` 调用复杂命令时返回 `-32700 parse error`
**客户端 JSON 编码问题**(深嵌 shell substitution + 中文 + 多层引号的组合让 Claude Code 自己生成的 tool-call JSON 失效),不是 srv 的 bug。Workaround:

1. 把命令拆多步,每步只一层引号
2. `export VAR=...` 提前到第一次调用,后续 `$VAR` 引用,降低单次复杂度
3. 复杂脚本走 `srv push script.sh /tmp/ && srv "bash /tmp/script.sh"`

### MCP `run` 跑 heredoc 报 `parse error near '\n'`
**已修(2.6.2)**。`wrapWithCwd` 现在在子 shell 闭合 `)` 之前加了换行,heredoc 终止符不再被挤到 `EOF)` 同行。升 2.6.2 即可。

### MCP 每次开始都在 `~`,要每次先 `srv cd`
**已修(2.6.2)**:`$SRV_CWD` 优先级高于 `profile.default_cwd`。Claude Code 注册时,在该项目的 mcpServers 段加:

```json
"srv": {
  "type": "stdio",
  "command": "D:\\WorkSpace\\server\\srv\\srv.exe",
  "args": ["mcp"],
  "env": { "SRV_CWD": "/mnt/project/alpha-bot" }
}
```

### MCP 长闲置后下一次调用挂住 / 报 EOF
**已缓解(2.6.2)**:daemon `getClient` 对池里 `lastUsed > 30s` 的连接先 ping,失败则 evict + redial。第二次调用稳。2.6.1 及之前需要手动重试一次。

### MCP `run` 链式命令里 `token=$(login)` 失败但 srv 报 exit 0
**Bash 语义**,不是 srv 报错丢失。`$(...)` 失败默认不让脚本退出,`curl -s ...` HTTP 错误也返回 0,最终 srv 看到的就是最后一条 curl 的 0。三种解法:

1. **拆成两个 `srv run`**,login 失败立刻冒成非 0 exit,链断在第一步
2. **加 `set -euo pipefail`**:`srv "set -euo pipefail; token=\$(login) && curl ..."` 让任何子命令非 0 直接挂
3. **curl 用 `-fsS`** 而不是 `-s`,HTTP 错误也会非 0 退出

### MCP `run` inline 后台启动(`& disown` / `nohup &`)进程没起来
**用 `srv -d` 代替** —— `srv -d <cmd>` 是为后台跑设计的,内置 `nohup` + 输出落 `~/.srv-jobs/<id>.log` + 记录 PID,稳定。Inline `&` 在 non-TTY SSH 上有 race 窗口(channel 关闭→SIGHUP / stdout 阻塞),不是 srv bug 是 SSH+shell 的固有行为。

```
srv -d ./svc                   # 起,立刻返回 job id
srv jobs                       # 看在跑的
srv logs <id> -f               # tail 远端日志
srv kill <id>                  # SIGTERM
```

### MCP `psql -c 'SELECT a; SELECT b;'` 只返回最后一条
psql 的固有行为(`-c` 模式只保留最后语句的结果集),不是 srv 的事。Workaround:

- DO block + `RAISE NOTICE` 输出中间步骤
- 写到文件后 `psql -f /tmp/multi.sql`(配合 `srv push`)
- 多次 `psql -c` 单独跑

### MCP 长输出 / 含特殊字符时偶发吞字符(中文、commit hash)
**没有干净的根因**(零星出现,难复现)。Workaround:`srv "cmd > /tmp/out.txt"` 后 `srv pull /tmp/out.txt` 或 `srv "head -n 100 /tmp/out.txt"`。

### `srv config set` 之后命令行为没变
- 检查 `~/.srv/config.json`,修改是否落到了正确的 profile
- 当前是不是有更高优先级的 `-P` flag / session pin / `SRV_PROFILE` 在覆盖

---

## 设计取舍 / 已知限制

- **非交互 ssh 不 source `.bashrc`**:别名/PATH 默认拿不到。`srv "bash -ic '<cmd>'"` 强制走交互 shell。
- **scp 中途断网**:文件可能半写。`srv push/pull` 失败重跑会覆盖,接受这个不强求"resume"语义。
- **长 ssh 命令断网就死**:只有 `srv -d` 起的 nohup 进程能跨断网存活。
- **ControlMaster 兼容性**:Windows OpenSSH 9.5+ 完整可用。老版本可能要 `multiplex=false`。
- **session id 在异常嵌套 shell 下可能不稳**:`SRV_SESSION` 兜底。
- **同 (session, profile) 单一 cwd**:不维护 cd 历史栈。

---

## 进一步阅读

- [README.en.md](./README.en.md) —— 英文版
- [CHANGELOG.md](./CHANGELOG.md) —— 版本变更历史
- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) —— 代码组织和扩展指南

---

## 版本

当前 **Go 2.6.x**(`srv version` 输出)。版本号在破坏性变更时增加。完整变更记录见 [CHANGELOG.md](./CHANGELOG.md)。

## 开发(给贡献者)

仓库自带一个 pre-commit hook(`.githooks/pre-commit`),提交前跑 `gofmt -l` + `go vet`,不干净就拒掉(可用 `--no-verify` 应急绕过)。clone 后**一次性激活**:

```sh
git config core.hooksPath .githooks
```

之后每次 `git commit` 自动校验 Go 文件。只有当本次提交动了 `go/*.go` 时才走检查,改 docs 不会被拖慢。

## 发版(给维护者)

发布走 GitHub Actions + goreleaser:推一个 `vX.Y.Z` tag 就自动产 5 平台二进制 + 校验和 + GitHub Release。

```sh
# 1) 改 CHANGELOG.md(顶部加新版本块),提交
# 2) 打 tag 并推
git tag v2.4.2
git push origin v2.4.2
```

GitHub Actions 会:
- 跨平台编译 linux/darwin/windows × amd64/arm64(共 5 个二进制——win-arm64 跳过)
- 用 `-ldflags -X main.Version={{.Version}}` 把 srv version 输出嵌成 tag 号
- 打成 `srv_<ver>_<os>_<arch>.tar.gz`(Windows 是 .zip),附带 `LICENSE` / `README*.md` / `CHANGELOG.md`
- 生成 `checksums.txt`(SHA256)
- 在 https://github.com/iccyuan/srv/releases 创建 release

本地 dry-run(不推到 GitHub):

```sh
# 需要先装 goreleaser:https://goreleaser.com/install/
goreleaser release --snapshot --clean --skip=publish
# 产物落到 ./dist/
```
