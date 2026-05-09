// srv -- run commands on a remote SSH server with persistent cwd.
//
// Go rewrite of the Python original (kept in ../src). Uses
// golang.org/x/crypto/ssh as a programmatic SSH client, sidestepping the
// system ssh.exe quirks the Python version had to work around.
package main

import (
	"fmt"
	"os"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
// goreleaser sets it from the git tag on release builds.
var Version = "2.6.5"

const helpEN = `srv - run commands on a remote SSH server with persistent cwd.

Quick start:
  srv init                       configure a profile interactively
  srv config list                show profiles
  srv use                        interactive picker (TTY): pin a profile to this shell
  srv use <profile>              pin a profile for this shell (quick switch)
  srv use --clear                unpin (fall back to default)
  srv config default             interactive picker: set the global default profile
  srv config default <profile>   set the global default profile (persists)
  srv cd /opt                    set persistent remote cwd (per session+profile)
  srv pwd                        show current remote cwd
  srv ls -la                     run on remote in current cwd
  srv "ps aux | grep redis"      pipes/redirects: quote at local shell
  srv -t htop                    interactive (TTY) command
  srv -P dev rsync ...           override profile for a single call
  srv check                      probe connectivity; diagnose key/host/port issues
  srv check --rtt [--count N]    measure SSH-level RTT + packet loss
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

Detached jobs (background on remote, log to ~/.srv-jobs/<id>.log):
  srv -d ./long-build.sh         kick off, return immediately, print job id
  srv jobs                       list local job records
  srv logs <id> [-f]             cat (or tail -f) the remote log
  srv kill <id>                  SIGTERM the remote process and forget it

Sessions (per-shell isolation):
  srv sessions                   list session records
  srv sessions show              show this shell's session record
  srv sessions clear             drop this shell's session record
  srv sessions prune             remove records whose pid is dead

Integrations:
  srv completion <bash|zsh|powershell>   emit shell completion script
  srv mcp                                run as a stdio MCP server
  srv guard [on|off|status]              MCP confirmation guard for high-risk ops (default off)
  srv color [on|off|use <name>|list|status]
                                         CLI run colour, on by default (any platform).
                                         srv color off to disable per-shell. drop *.sh
                                         into ~/.srv/init/ for custom presets, then
                                         srv color use <name>. MCP runs stay plain text.
  srv daemon                             keep ssh sessions warm (foreground)
  srv daemon status                      show running daemon's pool
  srv daemon status --json               machine-readable daemon status
  srv daemon stop                        stop the running daemon
  srv daemon restart                     restart background daemon
  srv daemon logs                        print auto-spawn daemon log
  srv daemon prune-cache                 drop the remote-completion (_ls) cache

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
  srv cd /opt                    设持久远端 cwd(per session+profile)
  srv pwd                        显示当前远端 cwd
  srv ls -la                     在远端当前 cwd 跑 ls -la
  srv "ps aux | grep redis"      含管道:本地引号,远端 shell 解析
  srv -t htop                    分配 TTY(vim / htop / sudo 输密码)
  srv -P dev rsync ...           单次命令切 profile
  srv check                      连通性诊断,9 类失败模式 + 修复建议
  srv check --rtt [--count N]    SSH 级 RTT + 丢包率
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

后台作业(远端 nohup,日志落 ~/.srv-jobs/<id>.log):
  srv -d ./long-build.sh         起后台,立刻返回 job id
  srv jobs                       列本地 job 记录
  srv logs <id> [-f]             cat(或 tail -f)远端日志
  srv kill <id>                  SIGTERM 远端进程并丢弃记录

会话(per-shell 隔离):
  srv sessions                   列所有 session 记录
  srv sessions show              当前 shell 的 session 记录
  srv sessions clear             删当前 session 记录
  srv sessions prune             清掉 PID 已死的 session

集成 / 工具:
  srv completion <bash|zsh|powershell>   输出 shell 补全脚本
  srv mcp                                以 stdio MCP server 跑
  srv guard [on|off|status]              MCP 高危操作确认开关(默认关闭,可针对当前 shell 开启)
  srv color [on|off|use <name>|list|status]
                                         CLI 远端命令彩色,默认开启(所有平台)。
                                         srv color off 关掉当前 shell;预设放
                                         ~/.srv/init/*.sh 后 srv color use <name>。
                                         MCP run 始终保持纯文本。
  srv daemon                             连接池前台运行(主要给调试)
  srv daemon status [--json]             看池里的 profile / uptime
  srv daemon stop                        停 daemon
  srv daemon restart                     重启后台 daemon
  srv daemon logs                        cat 自动 spawn 的 daemon 日志
  srv daemon prune-cache                 清远端补全 (_ls) 缓存

Profile 解析优先级(高 → 低):
  -P/--profile flag  >  session pin (` + "`" + `srv use` + "`" + `)  >  $SRV_PROFILE  >  全局默认

Session 检测:
  每个 shell 一个独立 session id(父 shell 的 PID,Windows 自动跳 shim)。
  $SRV_SESSION=<任意字符串> 可显式覆盖。

配置文件:~/.srv/config.json   会话:~/.srv/sessions.json
后台作业:~/.srv/jobs.json
`

// reservedSubcommands are names that won't be interpreted as remote commands.
// Any first-arg outside this set is run on the remote.
var reservedSubcommands = map[string]bool{
	"init": true, "config": true, "use": true, "cd": true, "pwd": true,
	"status": true, "check": true, "shell": true, "run": true, "exec": true,
	"push": true, "pull": true, "sync": true, "tunnel": true, "edit": true,
	"open": true, "code": true, "diff": true, "doctor": true,
	"env": true, "install": true, "completion": true, "mcp": true, "daemon": true, "guard": true, "color": true,
	"_profiles": true, "_ls": true,
	"jobs": true, "logs": true, "kill": true, "sessions": true,
	"help": true, "--help": true, "-h": true,
	"version": true, "--version": true,
}

type globalOpts struct {
	profile string
	tty     bool
	detach  bool
	noHints bool
}

func parseGlobalFlags(args []string) (globalOpts, []string) {
	var opts globalOpts
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-P" || a == "--profile":
			if i+1 >= len(args) {
				fatal(t("err.flag_requires_value", a))
			}
			opts.profile = args[i+1]
			i += 2
			continue
		case len(a) > 10 && a[:10] == "--profile=":
			opts.profile = a[10:]
			i++
			continue
		case a == "-t" || a == "--tty":
			opts.tty = true
			i++
			continue
		case a == "-d" || a == "--detach":
			opts.detach = true
			i++
			continue
		case a == "--no-hints":
			opts.noHints = true
			i++
			continue
		}
		break
	}
	return opts, args[i:]
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(t("help.full"))
		return 0
	}
	opts, rest := parseGlobalFlags(args)
	if len(rest) == 0 {
		fmt.Print(t("help.full"))
		return 0
	}

	sub := rest[0]

	// Help / version are pure local, no config needed.
	switch sub {
	case "help", "--help", "-h":
		fmt.Print(t("help.full"))
		return 0
	case "version", "--version":
		fmt.Printf("srv %s\n", Version)
		return 0
	case "completion":
		return cmdCompletion(rest[1:])
	case "install":
		// install opens a localhost web UI to wire srv up: PATH, MCP
		// registration, and a first-profile prompt. No config needed.
		return cmdInstall(rest[1:])
	case "init":
		// init creates config; load empty if missing.
		cfg, _ := LoadConfig()
		if cfg == nil {
			cfg = newConfig()
		}
		return cmdInit(cfg)
	}

	cfg, err := LoadConfig()
	if err != nil {
		fatal("%v", err)
	}
	if cfg == nil {
		cfg = newConfig()
	}

	switch sub {
	case "config":
		return cmdConfig(rest[1:], cfg)
	case "use":
		return cmdUse(rest[1:], cfg)
	case "cd":
		path := ""
		if len(rest) > 1 {
			path = rest[1]
		}
		return cmdCd(path, cfg, opts.profile)
	case "pwd":
		return cmdPwd(cfg, opts.profile)
	case "status":
		return cmdStatus(cfg, opts.profile)
	case "check":
		return cmdCheck(rest[1:], cfg, opts.profile)
	case "doctor":
		return cmdDoctor(rest[1:], cfg, opts.profile)
	case "shell":
		return cmdShell(cfg, opts.profile)
	case "push":
		return cmdPush(rest[1:], cfg, opts.profile)
	case "pull":
		return cmdPull(rest[1:], cfg, opts.profile)
	case "sync":
		return cmdSync(rest[1:], cfg, opts.profile)
	case "tunnel":
		return cmdTunnel(rest[1:], cfg, opts.profile)
	case "edit":
		return cmdEdit(rest[1:], cfg, opts.profile)
	case "open":
		return cmdOpen(rest[1:], cfg, opts.profile)
	case "code":
		return cmdCode(rest[1:], cfg, opts.profile)
	case "diff":
		return cmdDiff(rest[1:], cfg, opts.profile)
	case "env":
		return cmdEnv(rest[1:], cfg, opts.profile)
	case "jobs":
		return cmdJobs(cfg, opts.profile)
	case "logs":
		return cmdLogs(rest[1:], cfg, opts.profile)
	case "kill":
		return cmdKill(rest[1:], cfg, opts.profile)
	case "sessions":
		return cmdSessions(rest[1:])
	case "mcp":
		return cmdMcp(cfg)
	case "guard":
		return cmdGuard(rest[1:])
	case "color":
		return cmdColor(rest[1:])
	case "daemon":
		return cmdDaemon(rest[1:])
	case "_profiles":
		for n := range cfg.Profiles {
			fmt.Println(n)
		}
		return 0
	case "_ls":
		return cmdInternalLs(rest[1:], cfg, opts.profile)
	case "run", "exec":
		if opts.detach {
			return cmdDetach(rest[1:], cfg, opts.profile)
		}
		return cmdRunWithHints(rest[1:], cfg, opts)
	}

	// Default: treat as remote command. First, nudge the user if the
	// first token is suspiciously close to a known local subcommand --
	// the run still proceeds (their command might be the right one).
	emitTypoHintPre(cfg, opts, sub)
	if opts.detach {
		return cmdDetach(rest, cfg, opts.profile)
	}
	return cmdRunWithHints(rest, cfg, opts)
}
