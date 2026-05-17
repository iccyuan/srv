// Package i18n is the srv translation table. T() looks up keys in
// the active language and falls back to English.
//
// helpEN / helpZH live here (not in main.go) because they are pure
// translation payload -- the only thing that reads them is the
// messages table below.
package i18n

const helpEN = `srv - run commands on a remote SSH server with persistent cwd.

Quick start:
  srv init                       configure a profile interactively
  srv config list                show profiles
  srv use                        interactive picker (TTY): pin a profile to this shell
  srv use <profile>              pin a profile for this shell (quick switch)
  srv use --clear                unpin (fall back to default)
  srv config default             interactive picker: set the global default profile
  srv config default <profile>   set the global default profile (persists)
  srv settings                   show app settings (hints / lang / default_profile)
  srv settings <key> <value>     set an app setting (e.g. srv settings lang en)
  srv cd /opt                    set persistent remote cwd (per session+profile)
  srv cd -                       swap to the previously-recorded cwd (shell-style)
  srv pwd                        show current remote cwd
  srv ls -la                     run on remote in current cwd
  srv "ps aux | grep redis"      pipes/redirects: quote at local shell
  srv -t htop                    interactive (TTY) command
  srv -P dev rsync ...           override profile for a single call
  srv check                      probe connectivity; diagnose key/host/port issues
  srv check --rtt [--count N]    measure SSH-level RTT + packet loss
  srv check --rotate-key [--revoke-old]
                                 generate a fresh ed25519 key, push to remote
                                 authorized_keys, verify, and pin profile to it
  srv check --bandwidth [--duration 5s]
                                 measure up/down SSH throughput in Mbps
  srv doctor                     local config / daemon / SSH readiness report
  srv install                    open browser-based installer (PATH, Claude MCP, first profile)
  srv doctor --json              machine-readable diagnostics
  srv shell                      interactive remote shell (cwd-positioned)
  srv tunnel 8080                forward localhost:8080 -> remote 127.0.0.1:8080
  srv tunnel 8080:db:5432        forward localhost:8080 -> db:5432 from remote
  srv tunnel -R 9000:3000        reverse forward remote 9000 -> local 127.0.0.1:3000
  srv edit /etc/foo.conf         pull, open in $EDITOR, push back if changed
  srv open logs/app.log          pull remote file to temp and open locally
  srv code /opt/app              open VS Code Remote SSH for a remote folder
  srv diff local.py remote.py    compare local file with remote file
  srv diff --changed             diff all changed git files against remote
  srv env set NODE_ENV prod      set profile-level remote env var

File transfer (uses SFTP via the same SSH session):
  srv push ./local.py            upload to current cwd
  srv push ./dist /opt/app       upload (recursive auto-detected)
  srv pull logs/app.log          download to current dir
  srv pull /etc/hosts ./hosts    explicit local target

Bulk sync of changed files (tar | ssh tar; preserves relative paths):
  srv sync                       in a git repo: modified+staged+untracked
  srv sync --staged              only ` + "`" + `git add` + "`" + `-ed files
  srv sync --since 2h            files mtime'd within 2 hours
  srv sync --include "src/**/*.go"   glob mode (repeatable)
  srv sync --files a.go b/c.go   explicit list
  srv sync --dry-run             show what would push, don't transfer
  srv sync --delete --dry-run    show tracked remote deletes before applying
  srv sync --delete --yes        apply deletes above the default safety limit
  srv sync --delete-limit 50     change delete safety limit (default 20)
  srv sync /opt/app              override remote root (else cwd or sync_root)
  srv sync --watch               keep syncing on every local file change
  srv sync --diff [-v]           itemize-style preview (new/update/older/unchanged);
                                 implies dry-run. -v shows '=' rows too.
  srv sync --pull --files a b    pull remote -> local (list mode)
  srv sync --pull --include "*.log"   pull remote -> local (remote-find glob)
  srv sync --pull                git mode: pull whatever's changed on remote
  # .srvignore at sync root: gitignore-style patterns merged into --exclude;
  # ! prefix re-includes (override DefaultExcludes / profile.sync_exclude).
  # Parallel transfer: srv push/pull/sync fan out across SRV_TRANSFER_WORKERS
  # goroutines (default 4, range 1..32).

Detached jobs (background on remote, log to ~/.srv-jobs/<id>.log):
  srv -d ./long-build.sh         kick off, return immediately, print job id
  srv jobs                       list local job records
  srv jobs --watch [-n 2s]       refreshing TUI (q / Esc / Ctrl-C to quit)
  srv jobs notify on             enable OS toast on job completion
  srv jobs notify webhook URL    POST JSON to URL on completion
  srv jobs notify test           fire a sample notification
  srv logs <id> [-f]             cat (or tail -f) the remote log
  srv kill <id>                  SIGTERM the remote process and forget it

Supervisor + resource limits (apply to srv run and srv -d):
  srv --restart-on-fail [N] <cmd>      retry on non-zero exit (default unlimited)
  srv --restart-delay 10s <cmd>        backoff between retries (default 5s)
  srv --cpu-limit 50% <cmd>            CPUQuota via systemd-run when available
  srv --mem-limit 512M <cmd>           MemoryMax via systemd-run when available

Sessions (per-shell isolation):
  srv sessions                   list session records
  srv sessions show              show this shell's session record
  srv sessions clear             drop this shell's session record
  srv sessions prune             remove records whose pid is dead

Prune accumulated caches -- keeps the live/recent part, drops the stale
part (full wipe is a different verb, e.g. srv stats --clear):
  srv prune <TAB>                jobs | sessions | mcp-log | mcp-stats | all
  srv prune jobs                 drop finished records from the local ledger
  srv prune jobs --remote        also delete completed jobs' ~/.srv-jobs/
                                 *.log + *.exit on the server (running jobs
                                 untouched; opt-in, never implied)
  srv prune all [--remote]       jobs + sessions + mcp-log + mcp-stats

Command history (this session only by default, --all for everything):
  srv history                    last 50 commands run via srv (this shell)
  srv history -n 200             different limit
  srv history --profile prod     filter by profile
  srv history --grep RE          regex over the command string
  srv history --all              include every shell's commands
  srv history --json             raw JSONL (one record per line)
  srv history clear              truncate ~/.srv/history.jsonl
  srv history path               print the on-disk path

Lifecycle hooks (local commands triggered by srv events):
  srv hooks                      list configured hooks
  srv hooks events               list event names
  srv hooks set <event> <cmd>    replace the command list for <event>
  srv hooks add <event> <cmd>    append one more command for <event>
  srv hooks rm <event> [<idx>]   remove all (or one) commands for <event>
  srv hooks run <event>          fire <event> manually (debugging)
  # events: pre-cd post-cd pre-sync post-sync pre-run post-run
  #         pre-push post-push pre-pull post-pull
  # each hook runs in your local shell; SRV_HOOK/SRV_PROFILE/SRV_HOST/
  # SRV_CWD/SRV_TARGET/SRV_LOCAL/SRV_EXIT_CODE env vars describe the event.

Integrations:
  srv completion <bash|zsh|powershell> [--install]
                                         emit shell completion script (or auto-install into the shell's rc file)
  srv project                            show the active .srv-project pin (if any)
  srv group <list|show|set|remove>       manage named profile groups (for fan-out via -G)
  srv -G <group> <cmd>                   run cmd in parallel on every profile in <group>
  srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart] [--on-demand]
                                         save a named tunnel (--on-demand defers
                                         the SSH dial until the first connection)
  srv tunnel <up|down|list|show|remove> [name]
                                         manage saved tunnels (up/down go through the daemon)
  srv sudo [--no-cache] [--cache-ttl <dur>] <cmd>
                                         run cmd via remote sudo; password prompted locally (no echo),
                                         cached in the daemon for ~5min by default
  srv ui                                 one-screen dashboard (profiles, daemon, tunnels, jobs, sessions)
  srv tail [-n LINES] [--grep RE] <remote-path>...
                                         live-follow remote file(s) with auto-reconnect on SSH drop
  srv watch [-n SECS] [--diff] <cmd>     periodic remote command with in-place refresh (BSD watch over SSH)
  srv journal [-u UNIT] [--since TIME] [-f] [-g RE] [-n LINES]
                                         remote systemd journal (one-shot or live-follow)
  srv top [-n SECS]                      stream "top -b" from the remote (auto-reconnect on drop)
  srv mcp                                run as a stdio MCP server
  srv guard [on|off|status] [--global]   MCP high-risk guard (default ON; 'off' = this shell, 'off --global' = incl. MCP server)
  srv guard test "<cmd>"                 dry-run: report which rule would block <cmd>
  srv guard [list|add|rm|allow|defaults] manage the deny-pattern set + allow-list
  srv mcp replay [list|show <i>|clear|path]
                                         full args+result log of every MCP tools/call
  srv recipe [list|show|save|rm|run]     named multi-step playbooks; positional $1..$9
                                         and named ${KEY} substitution. Steps separated
                                         by ;; in 'save -- step1 ;; step2'.
  srv color [on|off|use [name]|list|status]
                                         CLI run colour, on by default (any platform).
                                         srv color off to disable per-shell. drop *.sh
                                         into ~/.srv/init/ for custom presets, then
                                         srv color use <name>; on a TTY, srv color use
                                         with no arg opens the arrow-key picker.
                                         MCP runs stay plain text.
  srv daemon                             keep ssh sessions warm (foreground)
  srv daemon status                      show running daemon's pool
  srv daemon status --json               machine-readable daemon status
  srv daemon stop                        stop the running daemon
  srv daemon restart                     restart background daemon
  srv daemon logs                        print auto-spawn daemon log
  srv disconnect [profile]               close the pooled SSH client for a
                                         profile (--all drops every pool entry)

Profile resolution (highest first):
  -P/--profile flag  >  session pin (` + "`" + `srv use` + "`" + `)  >  $SRV_PROFILE  >  default

Session detection:
  Each shell gets its own session id (parent shell's PID, with shim layers
  skipped on Windows). Override with $SRV_SESSION=<any string>.

Config: ~/.srv/config.json   Sessions: ~/.srv/sessions.json
Jobs: ~/.srv/jobs.json
`

const helpZH = `srv - 跨平台 SSH 远端命令工具,持久 cwd / 连接复用 / 会话隔离 / 后台作业。

快速开始:
  srv init                       交互向导:配置一个 profile
  srv config list                列出已配置的 profile
  srv use                        TTY 下:↑↓ 选择器(/ 过滤,Enter 选,q 取消)
  srv use <profile>              把 profile pin 到当前 shell
  srv use --clear                取消 pin,回落到全局默认
  srv config default             TTY 下:↑↓ 选择器,设全局默认
  srv config default <profile>   设全局默认(写 ~/.srv/config.json,所有 shell 共用)
  srv settings                   查看应用设置(hints / lang / default_profile)
  srv settings <key> <value>     设置应用项(如 srv settings lang zh)
  srv cd /opt                    设持久远端 cwd(per session+profile)
  srv cd -                       回到上一个 cwd(类似 shell 的 cd -)
  srv pwd                        显示当前远端 cwd
  srv ls -la                     在远端当前 cwd 跑 ls -la
  srv "ps aux | grep redis"      含管道:本地引号,远端 shell 解析
  srv -t htop                    分配 TTY(vim / htop / sudo 输密码)
  srv -P dev rsync ...           单次命令切 profile
  srv check                      连通性诊断,9 类失败模式 + 修复建议
  srv check --rtt [--count N]    SSH 级 RTT + 丢包率
  srv check --rotate-key [--revoke-old]
                                 生成新的 ed25519 key 推到远端 authorized_keys
                                 验证后把 profile.identity_file 切到新 key
  srv check --bandwidth [--duration 5s]
                                 测量 SSH 上下行吞吐(Mbps)
  srv doctor                     本地配置 / daemon / SSH 准备状态
  srv doctor --json              机器可读诊断
  srv install                    打开浏览器图形化安装器(PATH / Claude MCP / 第一个 profile)
  srv shell                      原生 PTY 远端 shell,自动 cd 到 cwd
  srv tunnel 8080                本地 8080 -> 远端 127.0.0.1:8080
  srv tunnel 8080:db:5432        本地 8080 -> db:5432(远端解析)
  srv tunnel -R 9000:3000        反向:远端 9000 -> 本地 127.0.0.1:3000
  srv edit /etc/foo.conf         拉到本地 -> $EDITOR -> 改了再推回
  srv open logs/app.log          拉远端文件到临时目录,本地默认 app 打开
  srv code /opt/app              用 VS Code Remote SSH 打开远端目录
  srv diff local.py remote.py    对比本地 / 远端文件
  srv diff --changed             对比所有 git 改动文件 vs 远端
  srv env set NODE_ENV prod      设 profile 级远端环境变量

文件传输(SFTP,复用同一条 SSH 会话):
  srv push ./local.py            上传到当前 cwd
  srv push ./dist /opt/app       上传(目录自动 -r)
  srv pull logs/app.log          下载到当前目录
  srv pull /etc/hosts ./hosts    显式本地目标

批量同步已变更文件(tar | ssh tar 流,保留相对路径):
  srv sync                       git 仓库:modified+staged+untracked
  srv sync --staged              只 ` + "`" + `git add` + "`" + ` 过的
  srv sync --since 2h            mtime 在 2 小时内
  srv sync --include "src/**/*.go"   glob 模式(可重复)
  srv sync --files a.go b/c.go   显式列表
  srv sync --dry-run             预览要传的文件,不真传
  srv sync --delete --dry-run    预览要删的远端文件
  srv sync --delete --yes        超过删除保护阈值时仍执行
  srv sync --delete-limit 50     调整删除保护阈值(默认 20)
  srv sync /opt/app              覆盖远端根(默认 = sync_root 或当前 cwd)
  srv sync --watch               文件变化时持续同步
  srv sync --diff [-v]           itemize 风格预览(new/update/older/unchanged),
                                 等价于一个加强版 dry-run;-v 会打印 = 行。
  srv sync --pull --files a b    反向(远端 -> 本地),list 模式
  srv sync --pull --include "*.log"   反向 + 远端 find 通配匹配
  srv sync --pull                远端是 git 仓库时,拉远端 git 改动的文件
  # 在 sync 根目录放 .srvignore (gitignore 风格), 自动合并到 --exclude;
  # ! 开头的模式反向放行,覆盖 DefaultExcludes / profile.sync_exclude.
  # 并行传输:srv push/pull/sync 用 SRV_TRANSFER_WORKERS 控制并发(默认 4,1~32)。

后台作业(远端 nohup,日志落 ~/.srv-jobs/<id>.log):
  srv -d ./long-build.sh         起后台,立刻返回 job id
  srv jobs                       列本地 job 记录
  srv jobs --watch [-n 2s]       自刷新 TUI(q / Esc / Ctrl-C 退出)
  srv jobs notify on             job 完成时弹本地 OS 通知
  srv jobs notify webhook URL    job 完成时 POST JSON 到 URL
  srv jobs notify test           发一次测试通知
  srv logs <id> [-f]             cat(或 tail -f)远端日志
  srv kill <id>                  SIGTERM 远端进程并丢弃记录

Supervisor / 资源限制(对 srv run 和 srv -d 都生效):
  srv --restart-on-fail [N] <cmd>      非零退出自动重启(N 缺省 = 不限)
  srv --restart-delay 10s <cmd>        每次重启之间的延迟(默认 5s)
  srv --cpu-limit 50% <cmd>            通过 systemd-run 设 CPUQuota(可用时)
  srv --mem-limit 512M <cmd>           通过 systemd-run 设 MemoryMax(可用时)

会话(per-shell 隔离):
  srv sessions                   列所有 session 记录
  srv sessions show              当前 shell 的 session 记录
  srv sessions clear             删当前 session 记录
  srv sessions prune             清掉 PID 已死的 session

清理累积的缓存 —— 留活/留近、只删陈旧(整文件清空是另一个动词,如 srv stats --clear):
  srv prune <TAB>                jobs | sessions | mcp-log | mcp-stats | all
  srv prune jobs                 删本地账本里已完成的 job 记录
  srv prune jobs --remote        额外删服务器 ~/.srv-jobs/ 里已完成作业的
                                 *.log + *.exit(运行中的不动;需显式 opt-in)
  srv prune all [--remote]       jobs + sessions + mcp-log + mcp-stats

命令历史(默认只看当前 shell, --all 看全部):
  srv history                    最近 50 条远端命令(当前 shell)
  srv history -n 200             指定条数
  srv history --profile prod     按 profile 过滤
  srv history --grep RE          按命令正则过滤
  srv history --all              所有 shell 的命令
  srv history --json             原始 JSONL
  srv history clear              清空 ~/.srv/history.jsonl
  srv history path               打印历史文件路径

生命周期钩子(srv 命令的事件触发本地脚本):
  srv hooks                      列出已配置的钩子
  srv hooks events               列出可用事件名
  srv hooks set <event> <cmd>    替换 <event> 的命令列表
  srv hooks add <event> <cmd>    向 <event> 追加一条命令
  srv hooks rm <event> [<idx>]   删除全部(或某一条)命令
  srv hooks run <event>          手动触发 <event>(用于调试)
  # 事件: pre-cd post-cd pre-sync post-sync pre-run post-run
  #       pre-push post-push pre-pull post-pull
  # 钩子在本地 shell 里执行,通过 SRV_HOOK / SRV_PROFILE / SRV_HOST /
  # SRV_CWD / SRV_TARGET / SRV_LOCAL / SRV_EXIT_CODE 等环境变量描述事件

集成 / 工具:
  srv completion <bash|zsh|powershell> [--install]
                                         输出 shell 补全脚本(加 --install 直接写入对应 shell 的 rc 文件)
  srv project                            查看当前 .srv-project 自动 pin 状态
  srv group <list|show|set|remove>       管理命名 profile 组(配合 -G 使用)
  srv -G <group> <cmd>                   在组内所有 profile 上并行执行 cmd
  srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart] [--on-demand]
                                         保存命名隧道(--on-demand 延迟 SSH dial 到第一次连接)
  srv tunnel <up|down|list|show|remove> [name]
                                         管理保存的隧道(up/down 由 daemon 托管)
  srv sudo [--no-cache] [--cache-ttl <dur>] <cmd>
                                         远程 sudo 执行;本地无回显输入密码,daemon 默认缓存 5 分钟
  srv ui                                 一屏总览(profile / daemon / tunnel / job / session)
  srv tail [-n LINES] [--grep RE] <remote-path>...
                                         实时跟踪远端文件,SSH 断了自动重连
  srv watch [-n SECS] [--diff] <cmd>     周期性跑远端命令,原地刷新(SSH 上的 watch)
  srv journal [-u UNIT] [--since TIME] [-f] [-g RE] [-n LINES]
                                         远端 systemd 日志(一次性或持续跟踪)
  srv top [-n SECS]                      流式拉取远端 "top -b",断线自动重连
  srv mcp                                以 stdio MCP server 跑
  srv guard [on|off|status] [--global]   MCP 高危确认(默认开;'off' 仅当前 shell,'off --global' 含 MCP server)
  srv guard test "<cmd>"                 dry-run: 给出哪条规则会拦截 <cmd>
  srv guard [list|add|rm|allow|defaults] 管理拦截正则集合 + 允许列表
  srv mcp replay [list|show <i>|clear|path]
                                         每次 tools/call 完整参数+结果回放
  srv recipe [list|show|save|rm|run]     命名多步剧本,支持 $1..$9 和 ${KEY} 替换。
                                         save -- s1 ;; s2 用 ;; 切分步骤。
  srv color [on|off|use [name]|list|status]
                                         CLI 远端命令彩色,默认开启(所有平台)。
                                         srv color off 关掉当前 shell;预设放
                                         ~/.srv/init/*.sh 后 srv color use <name>;
                                         TTY 下省略 name 进 ↑↓ 选择器。
                                         MCP run 始终保持纯文本。
  srv daemon                             连接池前台运行(主要给调试)
  srv daemon status [--json]             看池里的 profile / uptime
  srv daemon stop                        停 daemon
  srv daemon restart                     重启后台 daemon
  srv daemon logs                        cat 自动 spawn 的 daemon 日志
  srv disconnect [profile]               关闭 daemon 里某个 profile 的池连接
                                         (--all 关全部); tunnel 不受影响

Profile 解析优先级(高 → 低):
  -P/--profile flag  >  session pin (` + "`" + `srv use` + "`" + `)  >  $SRV_PROFILE  >  全局默认

Session 检测:
  每个 shell 一个独立 session id(父 shell 的 PID,Windows 自动跳 shim)。
  $SRV_SESSION=<任意字符串> 可显式覆盖。

配置文件:~/.srv/config.json   会话:~/.srv/sessions.json
后台作业:~/.srv/jobs.json
`
