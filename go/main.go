// srv -- run commands on a remote SSH server with persistent cwd.
//
// Go rewrite of the Python original (kept in ../src). Uses
// golang.org/x/crypto/ssh as a programmatic SSH client, sidestepping the
// system ssh.exe quirks the Python version had to work around.
package main

import (
	"fmt"
	"os"

	"srv/internal/i18n"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
// goreleaser sets it from the git tag on release builds.
var Version = "2.6.6"

func init() {
	// Let the i18n package read Config.Lang lazily without having to
	// import package main. Provider is invoked the first time T() is
	// called from a non-MCP context, so LoadConfig's disk read stays
	// off the hot startup path.
	i18n.SetConfigLangProvider(func() string {
		if cfg, _ := LoadConfig(); cfg != nil {
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

// errExit is the error type cmd handlers return to signal a non-zero
// exit code with an optional stderr message. main.go's run() translates
// it back into an exit code via translateExit. Replaces the old global
// fatal() / os.Exit pattern: cmd code now propagates rather than
// terminating, so the same handler can be safely reused under the MCP
// path (where os.Exit would have killed the whole server).
type errExit struct {
	code int
	msg  string
}

func (e *errExit) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.msg
}

// exitErr builds an errExit with a printf-formatted message. Use code 1
// for ordinary failures, 2 for usage / argument errors (POSIX convention).
func exitErr(code int, format string, args ...any) error {
	return &errExit{code: code, msg: fmt.Sprintf(format, args...)}
}

// exitCode wraps a bare numeric exit code into an error. Useful when a
// non-cmd helper (runRemoteStream, etc.) already returned the right
// code and we just want to propagate it without an extra message.
func exitCode(code int) error {
	if code == 0 {
		return nil
	}
	return &errExit{code: code}
}

// exitCodeOf is the inverse of exitCode -- pulls the numeric code out
// of an error. nil → 0, errExit → its code, anything else → 1. Used by
// cmdRunWithHints to decide whether a remote command exited 127 (the
// "did you mean a local subcommand?" hint trigger).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ex, ok := err.(*errExit); ok {
		return ex.code
	}
	return 1
}

// translateExit converts a cmd handler's error return into the int
// run() needs to pass to os.Exit. Empty-msg errExits (exitCode-style)
// emit no stderr line; non-errExit errors are printed verbatim and
// surface as exit 1.
func translateExit(err error) int {
	if err == nil {
		return 0
	}
	if ex, ok := err.(*errExit); ok {
		if ex.msg != "" {
			fmt.Fprintln(os.Stderr, ex.msg)
		}
		return ex.code
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
		cfg, err := LoadConfig()
		if err != nil {
			fatal("%v", err)
		}
		if cfg == nil {
			cfg = newConfig()
		}
		ctx.cfg = cfg
	}

	if known {
		return translateExit(cmd.handler(ctx))
	}

	// Default: treat as a remote command. Nudge the user if the first
	// token is suspiciously close to a known local subcommand -- the
	// run still proceeds (their command might be the right one).
	emitTypoHintPre(ctx.cfg, opts, sub)
	if opts.group != "" {
		return translateExit(cmdRunGroup(rest, ctx.cfg, opts.group))
	}
	if opts.detach {
		return translateExit(cmdDetach(rest, ctx.cfg, opts.profile))
	}
	return translateExit(cmdRunWithHints(rest, ctx.cfg, opts))
}
