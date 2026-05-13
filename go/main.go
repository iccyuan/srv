// srv -- run commands on a remote SSH server with persistent cwd.
//
// Go rewrite of the Python original (kept in ../src). Uses
// golang.org/x/crypto/ssh as a programmatic SSH client, sidestepping the
// system ssh.exe quirks the Python version had to work around.
package main

import (
	"fmt"
	"os"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/group"
	"srv/internal/hints"

	"srv/internal/i18n"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
// goreleaser sets it from the git tag on release builds.
var Version = "2.6.6"

func init() {
	// Let the i18n package read Config.Lang lazily without having to
	// import package main. Provider is invoked the first time T() is
	// called from a non-MCP context, so config.Load's disk read stays
	// off the hot startup path.
	i18n.SetConfigLangProvider(func() string {
		if cfg, _ := config.Load(); cfg != nil {
			return cfg.Lang
		}
		return ""
	})
}

// reservedSubcommands lives in commands.go now -- derived from the
// subcommand registry so it can never drift from the dispatcher. Adding
// a name there automatically excludes it from being interpreted as a
// remote command.

type globalOpts struct {
	profile string
	group   string
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
				fatal("%s", i18n.T("err.flag_requires_value", a))
			}
			opts.profile = args[i+1]
			i += 2
			continue
		case len(a) > 10 && a[:10] == "--profile=":
			opts.profile = a[10:]
			i++
			continue
		case a == "-G" || a == "--group":
			if i+1 >= len(args) {
				fatal("%s", i18n.T("err.flag_requires_value", a))
			}
			opts.group = args[i+1]
			i += 2
			continue
		case len(a) > 8 && a[:8] == "--group=":
			opts.group = a[8:]
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

// errExit / exitErr / exitCode / exitCodeOf moved to internal/clierr
// so feature subpackages can produce ExitError values translateExit
// recognises. The aliases below keep every package-main call site
// unchanged while the types live in the new shared package.
type errExit = clierr.ExitError

var (
	exitErr    = clierr.Errf
	exitCode   = clierr.Code
	exitCodeOf = clierr.CodeOf
)

// translateExit converts a cmd handler's error return into the int
// run() needs to pass to os.Exit. Empty-msg errExits (exitCode-style)
// emit no stderr line; non-errExit errors are printed verbatim and
// surface as exit 1.
func translateExit(err error) int {
	if err == nil {
		return 0
	}
	if ex, ok := err.(*errExit); ok {
		if ex.Msg != "" {
			fmt.Fprintln(os.Stderr, ex.Msg)
		}
		return ex.Code
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

// fatal is retained for the few CLI-only argument-parsing paths in
// main.go that run before any handler dispatch (parseGlobalFlags). It
// also panics under mcpMode so a stray future call can't silently kill
// the MCP server. New code should return errors via exitErr instead.
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if mcpMode {
		panic("fatal: " + msg)
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(i18n.T("help.full"))
		return 0
	}
	opts, rest := parseGlobalFlags(args)
	if len(rest) == 0 {
		fmt.Print(i18n.T("help.full"))
		return 0
	}

	sub := rest[0]
	cmd, known := lookupSub(sub)

	// -P and -G are mutually exclusive: a single profile pin makes no
	// sense when the caller has also asked for a group fan-out. Surface
	// at the parse point so the failure is at top-level, not buried in
	// one subcommand handler.
	if opts.profile != "" && opts.group != "" {
		fmt.Fprintln(os.Stderr, "error: -P and -G are mutually exclusive")
		return 2
	}

	// Build the uniform context. cfg is loaded only when at least one
	// path needs it: a known subcommand without noConfig, or the
	// remote-fallthrough (cmdRunWithHints / cmdDetach both need cfg).
	ctx := cmdCtx{
		args:            rest[1:],
		profileOverride: opts.profile,
		group:           opts.group,
		detach:          opts.detach,
		tty:             opts.tty,
		noHints:         opts.noHints,
	}
	needCfg := !known || !cmd.noConfig
	if needCfg {
		cfg, err := config.Load()
		if err != nil {
			fatal("%v", err)
		}
		if cfg == nil {
			cfg = config.New()
		}
		ctx.cfg = cfg
	}

	if known {
		return translateExit(cmd.handler(ctx))
	}

	// Default: treat as a remote command. Nudge the user if the first
	// token is suspiciously close to a known local subcommand -- the
	// run still proceeds (their command might be the right one).
	hints.EmitTypoPre(ctx.cfg, opts.noHints, sub)
	if opts.group != "" {
		return translateExit(group.RunCmd(rest, ctx.cfg, opts.group))
	}
	if opts.detach {
		return translateExit(cmdDetach(rest, ctx.cfg, opts.profile))
	}
	return translateExit(cmdRunWithHints(rest, ctx.cfg, opts))
}
