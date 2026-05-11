package main

import "fmt"

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
	cfg             *Config
	profileOverride string
	// group is the -G / --group flag, populated when the user wants to
	// fan-out a command across a named profile group. Currently honored
	// only by `run` (and the implicit run path); other subcommands
	// ignore it.
	group   string
	detach  bool
	tty     bool
	noHints bool
}

type subcommand struct {
	name     string   // primary name -- shown in help, drives dispatch
	aliases  []string // alternate names that hit the same handler (e.g. exec → run)
	handler  cmdHandler
	noConfig bool // skip LoadConfig before dispatch (help/version/install/completion/init load lazily)
	hidden   bool // internal helper -- excluded from help and from typo-hint candidates
}

// subcommands is the full registry. Order is preserved for
// `srv help`-style enumeration once we drive help text from this too;
// for now the prose help in main.go is still the user-facing source.
var subcommands = []subcommand{
	// Help / version / first-run -- need no config.
	{name: "help", aliases: []string{"--help", "-h"}, noConfig: true, handler: func(c cmdCtx) error {
		fmt.Print(t("help.full"))
		return nil
	}},
	{name: "version", aliases: []string{"--version"}, noConfig: true, handler: func(c cmdCtx) error {
		fmt.Printf("srv %s\n", Version)
		return nil
	}},
	{name: "completion", noConfig: true, handler: func(c cmdCtx) error { return cmdCompletion(c.args) }},
	{name: "install", noConfig: true, handler: func(c cmdCtx) error { return cmdInstall(c.args) }},
	{name: "init", noConfig: true, handler: func(c cmdCtx) error {
		// init creates config; load empty if missing.
		cfg, _ := LoadConfig()
		if cfg == nil {
			cfg = newConfig()
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
	{name: "check", handler: func(c cmdCtx) error { return cmdCheck(c.args, c.cfg, c.profileOverride) }},
	{name: "doctor", handler: func(c cmdCtx) error { return cmdDoctor(c.args, c.cfg, c.profileOverride) }},
	{name: "shell", handler: func(c cmdCtx) error { return cmdShell(c.cfg, c.profileOverride) }},
	{name: "env", handler: func(c cmdCtx) error { return cmdEnv(c.args, c.cfg, c.profileOverride) }},

	// Transfer / view.
	{name: "push", handler: func(c cmdCtx) error { return cmdPush(c.args, c.cfg, c.profileOverride) }},
	{name: "pull", handler: func(c cmdCtx) error { return cmdPull(c.args, c.cfg, c.profileOverride) }},
	{name: "sync", handler: func(c cmdCtx) error { return cmdSync(c.args, c.cfg, c.profileOverride) }},
	{name: "edit", handler: func(c cmdCtx) error { return cmdEdit(c.args, c.cfg, c.profileOverride) }},
	{name: "open", handler: func(c cmdCtx) error { return cmdOpen(c.args, c.cfg, c.profileOverride) }},
	{name: "code", handler: func(c cmdCtx) error { return cmdCode(c.args, c.cfg, c.profileOverride) }},
	{name: "diff", handler: func(c cmdCtx) error { return cmdDiff(c.args, c.cfg, c.profileOverride) }},

	// Tunnel / jobs / sessions.
	{name: "tunnel", handler: func(c cmdCtx) error { return cmdTunnel(c.args, c.cfg, c.profileOverride) }},
	{name: "jobs", handler: func(c cmdCtx) error { return cmdJobs(c.cfg, c.profileOverride) }},
	{name: "logs", handler: func(c cmdCtx) error { return cmdLogs(c.args, c.cfg, c.profileOverride) }},
	{name: "kill", handler: func(c cmdCtx) error { return cmdKill(c.args, c.cfg, c.profileOverride) }},
	{name: "sessions", handler: func(c cmdCtx) error { return cmdSessions(c.args) }},

	// Integrations / settings.
	{name: "mcp", handler: func(c cmdCtx) error { return cmdMcp(c.cfg) }},
	{name: "guard", handler: func(c cmdCtx) error { return cmdGuard(c.args) }},
	{name: "color", handler: func(c cmdCtx) error { return cmdColor(c.args) }},
	{name: "daemon", handler: func(c cmdCtx) error { return cmdDaemon(c.args) }},
	{name: "project", noConfig: true, handler: func(c cmdCtx) error { return cmdProject(c.args) }},
	{name: "group", handler: func(c cmdCtx) error { return cmdGroup(c.args, c.cfg) }},
	{name: "sudo", handler: func(c cmdCtx) error {
		return cmdSudo(c.args, c.cfg, globalOpts{profile: c.profileOverride})
	}},

	// run/exec: -d global flag swaps in cmdDetach; -G swaps in fan-out;
	// otherwise wrap with the typo-hint emitter.
	{name: "run", aliases: []string{"exec"}, handler: func(c cmdCtx) error {
		if c.group != "" {
			return cmdRunGroup(c.args, c.cfg, c.group)
		}
		if c.detach {
			return cmdDetach(c.args, c.cfg, c.profileOverride)
		}
		return cmdRunWithHints(c.args, c.cfg, globalOpts{
			profile: c.profileOverride,
			tty:     c.tty,
			detach:  c.detach,
			noHints: c.noHints,
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
	{name: "_ls", hidden: true, handler: func(c cmdCtx) error { return cmdInternalLs(c.args, c.cfg, c.profileOverride) }},
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
