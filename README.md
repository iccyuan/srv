# srv

[English](./README.en.md) | 中文

> 跨平台 SSH 命令工具:本地配置,远端执行。持久 cwd / 连接复用 / 会话隔离 / 后台作业。Claude Code / Codex 可通过 Bash 或 MCP 调用。**默认 Go 二进制**(零运行时依赖,内置 SSH 协议),Python 实现保留在 [`python/`](./python) 但**不再继续维护**。

## 速查

| 想做的事 | 命令 |
|---|---|
| 首次配置 + 验证 | `srv init && srv check` |
| 远端执行命令 | `srv ls -la` / `srv "ps aux \| grep x"` |
| 持久化 cwd | `srv cd /opt/app` |
| 切服务器(本 shell) | `srv use <profile>` |
| 推已变更文件 | `srv sync` |
| 推单个文件 | `srv push ./a.py` |
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

**默认就是 Go 版本** —— `srv` 是仓库根目录下的单二进制(由 [`go/`](./go) 源码编译)。Python 实现仍保留在 [`python/`](./python),需要时可以显式 `python python/srv.py`。

### 前置

- Go 1.22+(只构建时需要——https://go.dev/dl/)
- 远端服务器跑了 OpenSSH(本地不需要 ssh 客户端,Go 二进制自实现 SSH 协议)

### 编译

```sh
cd go
go build -o ../srv.exe .          # Windows
go build -o ../srv     .          # macOS / Linux
```

### Windows — 加 PATH

```powershell
[Environment]::SetEnvironmentVariable(
    "Path",
    "$([Environment]::GetEnvironmentVariable('Path','User'));D:\WorkSpace\server\srv",
    "User"
)
```

新开 PowerShell,`srv version` 应当显示 `srv 2.x.x`。

### macOS / Linux — 加 PATH

```sh
echo 'export PATH="$PATH:/path/to/srv"' >> ~/.bashrc   # 或 ~/.zshrc
exec $SHELL && srv version
```

或软链到现有 PATH 目录:

```sh
ln -s /path/to/srv/srv ~/.local/bin/srv
srv version
```

### Python 版本(已冻结,不再维护)

`python/srv.py` 停在 **v0.7.5**,**后续不会有功能更新或 bug 修复**。仍能跑(共享 `~/.srv/{config,sessions,jobs}.json`,功能与 Go 2.0.1 等价),适合:

- 需要在没有 Go 工具链的机器上临时使用
- 检查迁移过程中的行为差异

```sh
python python/srv.py status
python python/srv.py cd /opt
# ...
```

新功能、bug 修复、Windows OpenSSH 类问题的 workaround 一律只在 Go 版本进行。Python 版本未来某个版本可能会从仓库移除,届时 git 历史里仍可找回。

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
srv config use <name>               # 设全局默认
srv config remove <name>            # 删 profile
srv config set <prof> <key> <val>   # 改单个键(true/false/数字/null 自动转型)
```

### profile 快切

```
srv use <profile>     # 把 <profile> pin 到当前 shell,后续 srv 调用都用它
srv use --clear       # 取消 pin
srv use               # 显示当前 shell 的 pin / 默认 / active
```

**优先级**(高 → 低):

```
-P/--profile (单条命令)
  > srv use 设的 session pin
  > $SRV_PROFILE 环境变量
  > srv config use 设的全局默认
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
srv sync --exclude "*.log"            # 追加排除,可重复
srv sync /opt/app                     # 显式远端根(默认 = sync_root 或当前 cwd)
srv sync --root ./subproject          # 显式本地根(默认 = git 顶层 / 当前目录)
srv sync --no-git                     # 在 git 仓库里也不走 git 模式
```

默认排除:`.git`、`node_modules`、`__pycache__`、`.venv`、`venv`、`.idea`、`.vscode`、`.DS_Store`、`*.pyc`、`*.pyo`、`*.swp`。`list` 模式(`--files`)不应用默认排除——显式用户列表无条件传。

文件以 git 顶层(git 模式)或当前目录(其它模式)为锚点,远端在 `remote_root/<relative_path>` 落盘。

### 后台作业

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
| `srv config use <TAB>` | 已配置 profile 名 |
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
| `keepalive_interval` | 30 | KeepAlive 探测间隔(秒) |
| `keepalive_count` | 3 | 连续多少次失败后判定断线 |
| `control_persist` | `10m` | ControlMaster socket 闲置保留时长 |
| `sync_root` | null | `srv sync` 的默认远端根(命令行不带位置参数时用) |
| `sync_exclude` | `[]` | `srv sync` 的 profile 级追加排除(与默认排除合并) |
| `ssh_options` | `[]` | 任意原始 `-o` 选项数组,**最后**附加(覆盖前面的默认) |

---

## 多服务器、多终端

### 模型

- **profile** = 一台服务器(host + user + port + key + default_cwd 等)
- **session** = 一次 shell 启动。session id = 该 shell 进程的 PID(Windows 下自动跳过 .cmd shim 和 python launcher 中间层,定位到真 shell)
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

每次 ssh / scp 自动带上:

```
-o ControlMaster=auto
-o ControlPath=~/.srv/cm/%C.sock
-o ControlPersist=10m
-o ConnectTimeout=10
-o ServerAliveInterval=30
-o ServerAliveCountMax=3
-o TCPKeepAlive=yes
-o Compression=yes
```

**复用**:第一次握手后 socket 留存 10 分钟,后续 `srv` 调用走 socket 直接发送命令,跳过 TCP/SSH 握手。延迟从 100–300ms 降到 <30ms。

**重试**:握手失败(ssh exit==255 且 5 秒内退出)自动重试,退避 1s / 2s,共 3 次。`-t` 和 `-d` 跳过重试以避免重放风险。

**断线判定**:30 秒一次 KeepAlive,3 次失败(共 90 秒)判定断线退出,而不是无限挂着。

---

## Claude Code / Codex 集成

### 方式 1:Bash 调用

PATH 里有 `srv` 就行,无需额外配置:

```
srv ls /opt
srv -d "python long.py"
```

### 方式 2:MCP server(结构化工具)

Claude Code 通过 stdio MCP 拿到 14 个工具(run/cd/pwd/use/status/check/list_profiles/push/pull/sync/detach/list_jobs/tail_log/kill_job)。MCP 服务器实例的 session id = Claude Code 进程 PID,每个 Claude Code 实例独立。

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
- 通过 `srv` shim(`srv.cmd`)调用应该自动稳定
- 直接 `python srv.py` 调用也稳定
- 如果链路异常(嵌套多层 shell),手动 `$env:SRV_SESSION = $PID`

### `srv -d` 起的进程立刻退出
- 远端必须有 `bash`、`base64`、`nohup`(coreutils 一般都有)
- 看 `srv logs <id>` 看远端 stderr

### Claude Code 看不到新加的 MCP 工具
MCP 服务器在 Claude Code 会话启动时加载。**新开 Claude Code 会话**或 `/mcp` 重连后才会生效。

### `srv config set` 之后命令行为没变
- 检查 `~/.srv/config.json`,修改是否落到了正确的 profile
- 当前是不是有更高优先级的 `-P` flag / session pin / `SRV_PROFILE` 在覆盖

---

## 设计取舍 / 已知限制

- **不持久化环境变量**:每次 `srv` 都是独立 ssh 进程。要带 env 就 inline:`srv "FOO=1 python x.py"`。
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

当前 **Go 2.4.x**(`srv version` 输出)。版本号在破坏性变更时增加;Python 实现停在 0.7.5 后不再更新。完整变更记录见 [CHANGELOG.md](./CHANGELOG.md)。

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
