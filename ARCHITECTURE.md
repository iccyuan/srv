# Architecture

`srv` 是单文件 Python 工具,刻意避免引入第三方依赖。这份文档介绍代码结构和怎么扩展。用户向文档在 [README.md](./README.md)。

---

## 文件布局

```
D:\WorkSpace\server\srv\
├── srv.py            所有逻辑(单文件,~1300 行)
├── srv.cmd           Windows shim:找 python / py 后调 srv.py
├── srv               POSIX bash shim:调 python3 srv.py
├── README.md         用户文档(中文)
├── README.en.md      用户文档(英文)
├── CHANGELOG.md      版本历史
└── ARCHITECTURE.md   本文
```

运行时数据在 `~/.srv/`(可用 `$SRV_HOME` 改):`config.json` / `sessions.json` / `jobs.json` / `cm/<host>.sock`。

---

## srv.py 模块组织(自上而下)

| 段 | 内容 |
|---|---|
| 1 | 顶部 docstring(同时是 `srv help` 的输出) |
| 2 | stdlib imports |
| 3 | 路径常量、`VERSION`、`RESERVED_SUBCOMMANDS`、`HANDSHAKE_FAILURE_WINDOW_S`、`_INTERMEDIATE_EXES` |
| 4 | session 检测:`_walk_windows_processes` / `_session_id` / `_pid_alive` |
| 5 | JSON I/O:`_read_json` / `_write_json` |
| 6 | 配置 / 会话 / 作业的 `load_*` / `save_*` |
| 7 | 会话记录助手:`_touch_session`、`resolve_profile`、`get_cwd` / `set_cwd`、`set_session_profile` |
| 8 | SSH / scp 命令构造:`_default_ssh_options` / `build_ssh_cmd` / `build_scp_cmd` |
| 9 | 带重试的进程包装:`_ssh_call`(stream)/ `_ssh_run`(capture) |
| 10 | 远端操作原语:`run_remote` / `run_remote_capture` / `change_remote_cwd` / `remote_target` / `resolve_remote_path` |
| 11 | 子命令:`cmd_init` / `cmd_config` / `cmd_use` / `cmd_cd` / `cmd_pwd` / `cmd_status` / `cmd_check` / `cmd_run` / `cmd_push` / `cmd_pull` / `cmd_sessions` / `cmd_list_profiles_internal`(连通诊断:`_ssh_check` / `_check_advice`) |
| 11b | 批量同步:`_find_git_root` / `_git_changed_files` / `_mtime_changed_files` / `_glob_files` / `_matches_any_exclude` / `_normalize_for_tar` / `_tar_pipe_upload` / `_parse_sync_opts` / `_collect_sync_files` / `cmd_sync` |
| 12 | 后台作业:`_gen_job_id` / `_find_job` / `_spawn_detached` / `cmd_detach` / `cmd_jobs` / `cmd_logs` / `cmd_kill` |
| 13 | shell 补全脚本(三份字符串模板)+ `cmd_completion` |
| 14 | MCP server:`_mcp_tool_defs` / `_mcp_handle_tool` / `_mcp_send` / `_mcp_response` / `cmd_mcp` |
| 15 | `parse_global_flags` + `main()` 派发 |

---

## 核心概念

### Profile

一台已配置的服务器。存在 `config.json` 的 `profiles[name]` 下。键见 README 的 profile 表。

### Session

一次"逻辑 shell 实例",识别方式:

- **Unix**:`os.getppid()`
- **Windows**:从 python 自身向上走进程树,跳过 `_INTERMEDIATE_EXES` 里列的层(`cmd.exe` shim、Windows Store python launcher 等),返回第一个真实祖先的 PID
- **覆盖**:`$SRV_SESSION`(任意字符串)

每个 session 有自己 pin 的 profile 和按 profile 分桶的 cwd map,存 `sessions.json`。

### 弹性默认

`_default_ssh_options(profile)` 根据 profile 键拼出 `-o` 列表(ControlMaster / ConnectTimeout / ServerAlive / Compression 等),`build_ssh_cmd` 和 `build_scp_cmd` 都调用它。Profile 用户可调,见 README profile 键表。

### 重试政策

`_ssh_call` 和 `_ssh_run`:exit==255 且 < `HANDSHAKE_FAILURE_WINDOW_S`(5 秒)→ 判定为握手失败 → 重试,1s/2s 退避,最多 3 次。`-t`(交互)和 `-d`(spawn)关闭重试以免重放副作用。

### 批量同步

`cmd_sync` 流程:解析 opts → 选 local_root(git toplevel / cwd / 显式 `--root`) → 选 mode(显式 flag / 在 git 仓库内自动 git) → 用 `_git_changed_files` / `_mtime_changed_files` / `_glob_files` 之一收文件 → 应用排除(`list` 模式只用用户排除,其它模式叠加 `DEFAULT_SYNC_EXCLUDES` + profile.sync_exclude) → 过滤掉不存在的(deleted-in-worktree、stale glob)→ `_tar_pipe_upload` 起 `tar -cf - -C <root> file...` 子进程,`stdout` 接到 `ssh ... "cd <remote> && tar -xf -"`。

为什么用 tar 管道而不是 N 次 scp:单条 ssh 连接(配合 ControlMaster 几乎零握手)、保留相对路径、文件名带空格/中文/Unicode 都不用考虑引号转义。Windows 10+ 自带 `tar.exe`(bsdtar),跨平台都行。

### 后台作业

`_spawn_detached` 把用户命令 base64 编码后塞进:

```
mkdir -p ~/.srv-jobs && cd <cwd> && (
    nohup bash -c "$(echo BASE64 | base64 -d)" </dev/null >LOG 2>&1 & echo $!
)
```

远端打印 `$!`(后台 PID)回来,本地写入 `jobs.json`。base64 完全规避嵌套引号问题。

---

## 怎么扩展

### 加新子命令

1. 名字加进 `RESERVED_SUBCOMMANDS`
2. 写 `cmd_<name>(rest, cfg, profile_override)` 或类似签名
3. `main()` 加 `if sub == "<name>": return cmd_<name>(...)`
4. 三份补全模板都加上(`_BASH_COMPLETION` / `_ZSH_COMPLETION` / `_POWERSHELL_COMPLETION`)
5. 顶部 docstring 加用法行(就是 `srv help` 的输出)
6. 想做 MCP 也能用,看下一节

### 加新 MCP 工具

1. `_mcp_tool_defs()` 数组追加 entry:`name` + `description` + `inputSchema`(JSON Schema 子集)
2. `_mcp_handle_tool()` 加 `if name == "<your_tool>": ...`
3. 返回 dict 含 `content`(必,文本数组)、`isError`(可选)、`structuredContent`(可选,推荐——Agent 客户端能用)

### 加新 profile 键

1. 在用它的地方读(常见在 `_default_ssh_options` 或某个 `cmd_*`),带默认值
2. README 的 profile 键表加一行
3. 用户通过 `srv config set <prof> <key> <value>` 写入。`cmd_config` 的 set 分支会自动把 `true`/`false`/数字字符串/`null` 转成对应类型——纯字符串值原样存

### 加新 ssh 弹性选项

直接在 `_default_ssh_options` 里加 `-o` 项,从 profile 取值带默认。同时 README profile 表加一行说明。用户的 `ssh_options` 数组在 builder 里**最后**附加,所以可以覆盖任何默认。

### 改 session 检测

主逻辑在 `_session_id`。Windows 那段如果遇到新的中间进程类型(比如未来某个 launcher),把 exe 名加到 `_INTERMEDIATE_EXES` 集合即可。

---

## 没有真服务器时能测什么

`SRV_HOME=/tmp/srv-test` 把配置目录隔离开,然后:

- **配置流**:`init` / `config list` / `config set` / `config show` / `config remove`
- **会话流**:`use <name>` / `use --clear` / `use`(无参) / `sessions list` / `sessions show` / `sessions prune`
- **作业流(空态)**:`jobs`(空)/ `kill` 错误处理 / `logs` 错误处理
- **补全脚本**:`completion bash` / `completion zsh` / `completion powershell`(看输出格式)
- **MCP**:用 echo 喂 JSON-RPC 行,验 `initialize` / `tools/list` / 不连远端的 `tools/call`(`status` / `list_profiles` / `use`)
- **build_ssh_cmd 输出**:`python -c "import srv; cfg=srv.load_config(); ... print(srv.build_ssh_cmd(p, 'echo'))"` 看实际下发的 argv

要验真实 ssh / scp / `cd` / `run` / `push` / `pull` / `-d`,需要一台可登的服务器,profile 配上 host/key 后跑。

---

## 编码风格

- **类型注解**:函数签名加,函数体里别教条
- **致命错误**:`sys.exit("error: ...")`,stderr 输出,非零退出
- **单文件原则**:除非有强理由,不拆模块——简化分发
- **注释**:只解释 *why*(非显然不变量、workaround、约束),不解释 *what*——代码说了
- **路径**:用 `pathlib.Path`;`as_posix()` 给 ssh `-o ControlPath=` 这种需要正斜杠的场景
- **f-string**:优先 f-string,不混 `.format` / `%`
- **读写最少化**:JSON 文件读改写一次性完成,别多次

---

## 为什么不要第三方依赖

单文件 + stdlib + 系统 `ssh`/`scp`,安装等于"复制文件 + 加 PATH",对 Windows / macOS / Linux 同样简单。没有 `pip install`,没有 venv,没有版本冲突。Windows 上靠 `ctypes` 走 Win32 API 拿进程树,也不需要 `psutil`。

代价:Windows 进程检测有点繁琐(50 行 ctypes),JSON-RPC for MCP 是手写而非用 `mcp` 库。可以接受。

---

## 非目标

- 不做完整的 ssh client(替代 `~/.ssh/config` 全部特性)。复杂场景应该让用户写自己的 `~/.ssh/config`,然后 `srv` 的 profile 用 `Host` alias 引用。
- 不做 GUI / TUI。命令行就够用。
- 不做远端文件 watch / 持续 sync。`scp push/pull` 对一次性传输够用,持续同步是 rsync / mutagen 的事。
- 不做 ssh 跳板/隧道(可以用 `ssh_options` 带 `ProxyJump=...` 自己加)。
