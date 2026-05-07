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
var Version = "2.6.1"

const helpText = `srv - run commands on a remote SSH server with persistent cwd.

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
  srv doctor                     local config / daemon / SSH readiness report
  srv shell                      interactive remote shell (cwd-positioned)
  srv tunnel 8080                forward localhost:8080 -> remote 127.0.0.1:8080
  srv tunnel 8080:db:5432        forward localhost:8080 -> db:5432 from remote
  srv edit /etc/foo.conf         pull, open in $EDITOR, push back if changed
  srv open logs/app.log          pull remote file to temp and open locally
  srv code /opt/app              open VS Code Remote SSH for a remote folder
  srv diff local.py remote.py    compare local file with remote file
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

// reservedSubcommands are names that won't be interpreted as remote commands.
// Any first-arg outside this set is run on the remote.
var reservedSubcommands = map[string]bool{
	"init": true, "config": true, "use": true, "cd": true, "pwd": true,
	"status": true, "check": true, "shell": true, "run": true, "exec": true,
	"push": true, "pull": true, "sync": true, "tunnel": true, "edit": true,
	"open": true, "code": true, "diff": true, "doctor": true,
	"env": true, "completion": true, "mcp": true, "daemon": true,
	"_profiles": true, "_ls": true,
	"jobs": true, "logs": true, "kill": true, "sessions": true,
	"help": true, "--help": true, "-h": true,
	"version": true, "--version": true,
}

type globalOpts struct {
	profile string
	tty     bool
	detach  bool
}

func parseGlobalFlags(args []string) (globalOpts, []string) {
	var opts globalOpts
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-P" || a == "--profile":
			if i+1 >= len(args) {
				fatal("error: %s requires a value.", a)
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
		fmt.Print(helpText)
		return 0
	}
	opts, rest := parseGlobalFlags(args)
	if len(rest) == 0 {
		fmt.Print(helpText)
		return 0
	}

	sub := rest[0]

	// Help / version are pure local, no config needed.
	switch sub {
	case "help", "--help", "-h":
		fmt.Print(helpText)
		return 0
	case "version", "--version":
		fmt.Printf("srv %s\n", Version)
		return 0
	case "completion":
		return cmdCompletion(rest[1:])
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
		return cmdCheck(cfg, opts.profile)
	case "doctor":
		return cmdDoctor(cfg, opts.profile)
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
		return cmdRun(rest[1:], cfg, opts.profile, opts.tty)
	}

	// Default: treat as remote command.
	if opts.detach {
		return cmdDetach(rest, cfg, opts.profile)
	}
	return cmdRun(rest, cfg, opts.profile, opts.tty)
}
