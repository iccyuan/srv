package main

import (
	"fmt"
	"srv/internal/check"
	"srv/internal/clierr"
	"srv/internal/completion"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/diff"
	"srv/internal/doctor"
	"srv/internal/editcmd"
	"srv/internal/group"
	"srv/internal/guard"
	"srv/internal/hints"
	"srv/internal/history"
	"srv/internal/hooks"
	"srv/internal/install"
	"srv/internal/jobcli"
	"srv/internal/launcher"
	"srv/internal/mcp"
	"srv/internal/mcpstats"
	"srv/internal/project"
	"srv/internal/recipe"
	"srv/internal/session"
	"srv/internal/streams"
	"srv/internal/sudo"
	"srv/internal/syncx"
	"srv/internal/theme"
	"srv/internal/tunnel"
	"srv/internal/ui"
	"time"

	"srv/internal/i18n"
)

// Subcommand registry. ONE source of truth: dispatch, the
// reservedSubcommands set used by the typo-hint engine, and -- once the
// completion DSL lands -- the shell completion specs all read from this
// slice. Adding a new subcommand means appending one entry here, no
// other file edits required (apart from the actual cmdXxx implementation
// and an optional one-line update to the prose help text).
//
// The handler shape is uniform (cmdHandler over cmdCtx) so the registry
// stays a flat data table rather than a series of irregular function
// signatures. Each entry's handler is a tiny adapter that pulls the
// fields it needs out of cmdCtx and calls the underlying cmd* impl --
// preserves the cmd* signatures we already have without forcing every
// command to take an opaque context object.

type cmdHandler func(ctx cmdCtx) error

// cmdCtx is the uniform input shape every handler receives. Holds the
// post-flag args and the resolved config (when loaded), plus the
// global-flag values that some handlers care about.
type cmdCtx struct {
	args            []string
	cfg             *config.Config
	profileOverride string
	// group is the -G / --group flag, populated when the user wants to
	// fan-out a command across a named profile group. Currently honored
	// only by `run` (and the implicit run path); other subcommands
	// ignore it.
	group   string
	detach  bool
	tty     bool
	noHints bool
	// runwrap-related: see globalOpts in main.go.
	restartOnFail int
	restartDelay  time.Duration
	cpuLimit      string
	memLimit      string
}

type subcommand struct {
	name     string   // primary name -- shown in help, drives dispatch
	aliases  []string // alternate names that hit the same handler (e.g. exec → run)
	handler  cmdHandler
	noConfig bool // skip config.Load before dispatch (help/version/install/completion/init load lazily)
	hidden   bool // internal helper -- excluded from help and from typo-hint candidates
}

// subcommands is the full registry. Order is preserved for
// `srv help`-style enumeration once we drive help text from this too;
// for now the prose help in main.go is still the user-facing source.
var subcommands = []subcommand{
	// Help / version / first-run -- need no config.
	{name: "help", aliases: []string{"--help", "-h"}, noConfig: true, handler: func(c cmdCtx) error {
		fmt.Print(i18n.T("help.full"))
		return nil
	}},
	{name: "version", aliases: []string{"--version"}, noConfig: true, handler: func(c cmdCtx) error {
		fmt.Printf("srv %s\n", Version)
		return nil
	}},
	{name: "completion", noConfig: true, handler: func(c cmdCtx) error { return completion.Cmd(c.args, userVisibleSubcommands()) }},
	{name: "install", noConfig: true, handler: func(c cmdCtx) error {
		snap := install.Snap{Version: Version}
		if cfg, _ := config.Load(); cfg != nil {
			snap.ProfileCount = len(cfg.Profiles)
			snap.ProfileDefault = cfg.DefaultProfile
		}
		return install.Cmd(c.args, snap)
	}},
	{name: "init", noConfig: true, handler: func(c cmdCtx) error {
		// init creates config; load empty if missing.
		cfg, _ := config.Load()
		if cfg == nil {
			cfg = config.New()
		}
		return cmdInit(cfg)
	}},

	// Profile / cwd / status.
	{name: "config", handler: func(c cmdCtx) error { return cmdConfig(c.args, c.cfg) }},
	{name: "use", handler: func(c cmdCtx) error { return cmdUse(c.args, c.cfg) }},
	{name: "cd", handler: func(c cmdCtx) error {
		p := ""
		if len(c.args) > 0 {
			p = c.args[0]
		}
		return cmdCd(p, c.cfg, c.profileOverride)
	}},
	{name: "pwd", handler: func(c cmdCtx) error { return cmdPwd(c.cfg, c.profileOverride) }},
	{name: "status", handler: func(c cmdCtx) error { return cmdStatus(c.cfg, c.profileOverride) }},
	{name: "check", handler: func(c cmdCtx) error { return check.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "doctor", handler: func(c cmdCtx) error { return doctor.Cmd(c.args, c.cfg, c.profileOverride, Version) }},
	{name: "shell", handler: func(c cmdCtx) error { return cmdShell(c.cfg, c.profileOverride) }},
	{name: "env", handler: func(c cmdCtx) error { return cmdEnv(c.args, c.cfg, c.profileOverride) }},

	// Transfer / view.
	{name: "push", handler: func(c cmdCtx) error { return cmdPush(c.args, c.cfg, c.profileOverride) }},
	{name: "pull", handler: func(c cmdCtx) error { return cmdPull(c.args, c.cfg, c.profileOverride) }},
	{name: "sync", handler: func(c cmdCtx) error { return syncx.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "edit", handler: func(c cmdCtx) error { return editcmd.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "open", handler: func(c cmdCtx) error { return launcher.Open(c.args, c.cfg, c.profileOverride) }},
	{name: "code", handler: func(c cmdCtx) error { return launcher.Code(c.args, c.cfg, c.profileOverride) }},
	{name: "diff", handler: func(c cmdCtx) error { return diff.Cmd(c.args, c.cfg, c.profileOverride) }},

	// Tunnel / jobs / sessions.
	{name: "tunnel", handler: func(c cmdCtx) error { return tunnel.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "jobs", handler: func(c cmdCtx) error { return jobcli.CmdJobs(c.args, c.cfg, c.profileOverride) }},
	{name: "logs", handler: func(c cmdCtx) error { return jobcli.CmdLogs(c.args, c.cfg, c.profileOverride) }},
	{name: "kill", handler: func(c cmdCtx) error { return jobcli.CmdKill(c.args, c.cfg, c.profileOverride) }},
	{name: "sessions", handler: func(c cmdCtx) error { return cmdSessions(c.args) }},
	{name: "hooks", handler: func(c cmdCtx) error { return hooks.Cmd(c.args, c.cfg) }},
	{name: "history", handler: func(c cmdCtx) error { return history.Cmd(c.args, session.ID()) }},
	{name: "recipe", handler: func(c cmdCtx) error { return recipe.Cmd(c.args, c.cfg, c.profileOverride) }},

	// Integrations / settings.
	{name: "mcp", handler: func(c cmdCtx) error {
		// `srv mcp` with no args is the stdio MCP server entry that
		// Claude Code launches; subcommands are inspection helpers
		// that run locally and exit. `serve` is accepted as an
		// explicit alias for the no-args form so scripts that want
		// to be unambiguous can write it.
		if len(c.args) == 0 || c.args[0] == "serve" {
			mcpMode = true
			return mcp.Run(c.cfg, Version)
		}
		switch c.args[0] {
		case "stats":
			return mcpstats.Cmd(c.args[1:])
		case "replay":
			return mcp.ReplayCmd(c.args[1:])
		}
		return clierr.Errf(2, "unknown mcp subcommand %q (try: serve, stats, replay)", c.args[0])
	}},
	{name: "guard", handler: func(c cmdCtx) error { return guard.Cmd(c.args) }},
	{name: "color", handler: func(c cmdCtx) error { return theme.Cmd(c.args) }},
	{name: "daemon", handler: func(c cmdCtx) error { return daemon.Cmd(c.args) }},
	{name: "disconnect", handler: func(c cmdCtx) error { return daemon.DisconnectCmd(c.args, c.cfg, c.profileOverride) }},
	{name: "project", noConfig: true, handler: func(c cmdCtx) error { return project.Cmd(c.args) }},
	{name: "group", handler: func(c cmdCtx) error { return group.Cmd(c.args, c.cfg) }},
	{name: "sudo", handler: func(c cmdCtx) error { return sudo.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "ui", handler: func(c cmdCtx) error { return ui.Cmd(c.args, c.cfg, c.profileOverride) }},
	{name: "tail", handler: func(c cmdCtx) error { return streams.Tail(c.args, c.cfg, c.profileOverride) }},
	{name: "watch", handler: func(c cmdCtx) error { return streams.Watch(c.args, c.cfg, c.profileOverride) }},
	{name: "journal", handler: func(c cmdCtx) error { return streams.Journal(c.args, c.cfg, c.profileOverride) }},
	{name: "top", handler: func(c cmdCtx) error { return streams.Top(c.args, c.cfg, c.profileOverride) }},

	// run/exec: -d global flag swaps in cmdDetach; -G swaps in fan-out;
	// otherwise wrap with the typo-hint emitter.
	{name: "run", aliases: []string{"exec"}, handler: func(c cmdCtx) error {
		if c.group != "" {
			return group.RunCmd(c.args, c.cfg, c.group)
		}
		if c.detach {
			return jobcli.CmdDetach(c.args, c.cfg, c.profileOverride, jobcli.RunOpts{
				RestartOnFail: c.restartOnFail,
				RestartDelay:  c.restartDelay,
				CPULimit:      c.cpuLimit,
				MemLimit:      c.memLimit,
			})
		}
		return cmdRunWithHints(c.args, c.cfg, globalOpts{
			profile:       c.profileOverride,
			tty:           c.tty,
			detach:        c.detach,
			noHints:       c.noHints,
			restartOnFail: c.restartOnFail,
			restartDelay:  c.restartDelay,
			cpuLimit:      c.cpuLimit,
			memLimit:      c.memLimit,
		})
	}},

	// Internal helpers for shell completion. Hidden so they don't show
	// up in `srv help` and aren't candidates for typo-hint matching.
	{name: "_profiles", hidden: true, handler: func(c cmdCtx) error {
		for n := range c.cfg.Profiles {
			fmt.Println(n)
		}
		return nil
	}},
	{name: "_ls", hidden: true, handler: func(c cmdCtx) error { return completion.LsCmd(c.args, c.cfg, c.profileOverride) }},
	{name: "_remote_path", hidden: true, handler: func(c cmdCtx) error { return completion.RemotePathCmd(c.args, c.cfg, c.profileOverride) }},
}

// subcommandMap and reservedSubcommands are populated by init() rather
// than var initializers because the registry's closures transitively
// reference cmdRunWithHints → suggestSubcommand → reservedSubcommands,
// which Go's compiler flags as an initialization cycle when the maps
// are themselves var initializers. init() runs after all package vars
// are constructed, so by then `subcommands` is fully laid down and we
// can derive both lookup tables in one pass.
var (
	subcommandMap         map[string]*subcommand
	reservedSubcommands   map[string]bool
	visibleSubcommandList []string
)

func init() {
	subcommandMap = make(map[string]*subcommand, len(subcommands)*2)
	reservedSubcommands = make(map[string]bool, len(subcommands)*2)
	visibleSubcommandList = make([]string, 0, len(subcommands))
	for i := range subcommands {
		s := &subcommands[i]
		subcommandMap[s.name] = s
		reservedSubcommands[s.name] = true
		for _, a := range s.aliases {
			subcommandMap[a] = s
			reservedSubcommands[a] = true
		}
		if !s.hidden {
			visibleSubcommandList = append(visibleSubcommandList, s.name)
			// Include aliases so e.g. `srv exe<TAB>` still completes to
			// `exec`, but skip the dash-flag ones (`--help`/`-h`/
			// `--version`) -- they're not natural completion targets, the
			// user knows to type those out.
			for _, a := range s.aliases {
				if len(a) > 0 && a[0] != '-' {
					visibleSubcommandList = append(visibleSubcommandList, a)
				}
			}
		}
	}
	// Seed the hint engine's fuzzy-match set from the reserved-name
	// table. Hidden helpers (_profiles / _ls) and dash-flag aliases are
	// filtered out inside internal/hints.
	names := make([]string, 0, len(reservedSubcommands))
	for n := range reservedSubcommands {
		names = append(names, n)
	}
	hints.SetCandidates(names)
}

func lookupSub(name string) (*subcommand, bool) {
	s, ok := subcommandMap[name]
	return s, ok
}

// userVisibleSubcommands returns the primary names of every non-hidden
// subcommand, in registration order. Used by the completion templates
// (bash/zsh/PS) so their `subs` lists derive from the registry rather
// than being copy-pasted into three shell scripts that historically
// drifted apart whenever a new command was added.
//
// Reads from a cache populated by init() instead of iterating
// `subcommands` directly: doing the latter would form an
// initialization cycle (subcommands' closure for `completion` calls
// cmdCompletion, which calls back here).
func userVisibleSubcommands() []string {
	return visibleSubcommandList
}
