package streams

import (
	"fmt"
	"os"
	"regexp"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strconv"
	"strings"
)

// Journal is a thin, opinionated wrapper around remote
// `journalctl`. The win over `srv run "journalctl ..."` is:
//   - `-f` mode uses StreamWithReconnect so a flaky link doesn't kill
//     a long-running tail.
//   - Argument shape matches journalctl's familiar one (-u UNIT,
//     --since TIME, -p PRIORITY, -n LINES, -g REGEX, -f) so muscle
//     memory transfers.
//   - Synchronous (non -f) calls go through remote.RunCapture which
//     reuses the daemon connection pool.
//
// Linux-only by nature; on non-systemd remotes the remote command
// just errors and we forward that. We don't try to detect "no
// journalctl here" upfront because the user already has a profile
// pointing at the remote -- they know whether it has systemd.
func Journal(args []string, cfg *config.Config, profileOverride string) error {
	jc, err := ParseJournalArgs(args)
	if err != nil {
		return err
	}
	profName, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	// Auto-dispatch when the user didn't explicitly set --prefer log.
	// A darwin remote gets the macOS path by default so `srv journal`
	// just works on a Mac profile without remembering the flag. The
	// CLI has no "explicitly false" form, so a true PreferLog means
	// the user passed --prefer log and we respect it; otherwise we
	// fall through to the auto-detect.
	if !jc.PreferLog && remote.GetPlatform(profile) == remote.PlatformDarwin {
		jc.PreferLog = true
	}
	remoteCmd := jc.ToRemoteCommand()
	cwd := config.GetCwd(profName, profile)

	if !jc.Follow {
		res, _ := remote.RunCapture(profile, cwd, remoteCmd)
		os.Stdout.WriteString(res.Stdout)
		if res.Stderr != "" {
			os.Stderr.WriteString(res.Stderr)
		}
		if hint := MissingJournalctlHint(res.ExitCode, res.Stderr, jc.PreferLog); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		return srvutil.Code(res.ExitCode)
	}

	var re *regexp.Regexp
	if jc.LocalGrep != "" {
		r, gerr := regexp.Compile(jc.LocalGrep)
		if gerr != nil {
			return srvutil.Errf(2, "bad regex %q: %v", jc.LocalGrep, gerr)
		}
		re = r
	}
	fmt.Fprintf(os.Stderr,
		"srv journal: following on %s   (Ctrl-C to stop, auto-reconnect on drop)\n",
		profName)
	onChunk := func(kind sshx.StreamChunkKind, line string) {
		if re != nil && !re.MatchString(line) {
			return
		}
		if kind == sshx.StreamStderr {
			fmt.Fprint(os.Stderr, line)
		} else {
			fmt.Fprint(os.Stdout, line)
		}
	}
	return StreamWithReconnectResumable(profile, &journalResumer{base: jc}, onChunk)
}

// journalResumer rebuilds the journalctl invocation across reconnects.
// On the first attempt it issues the user's command verbatim; on each
// reconnect it overrides --since with the timestamp parsed off the
// last seen stdout line. The first stdout chunk after the reconnect is
// matched against the cached lastLine and dropped when identical --
// journalctl --since=<ts> is inclusive of the boundary second, so the
// seam line is otherwise printed twice.
//
// We extract the timestamp from the `-o short-iso` prefix the journal
// command always carries (`2026-05-15T10:30:45+0800 host ...`). When
// no timestamp has been observed yet, the resumer falls back to the
// user's original --since (or no --since), so a reconnect that
// happens before the first line still works.
type journalResumer struct {
	base     JournalCmd
	sinceISO string // last observed ISO timestamp, "" until first line
	lastLine string // last stdout line, used for boundary dedupe
}

// journalISOTimestampPattern matches the leading "2026-05-15T10:30:45+0800"
// timestamp short-iso always prints. Anchored at the start so a
// timestamp appearing in the middle of a payload won't fool us.
var journalISOTimestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{4}`)

func (j *journalResumer) Cmd() string {
	cmd := j.base
	if j.sinceISO != "" {
		// Override --since regardless of whether the user originally
		// supplied one; we want to resume from where we stopped, not
		// from the user's original window-start.
		cmd.Since = j.sinceISO
		// Also force --lines=0 so the resume doesn't re-emit the
		// original -n N backlog all over again.
		cmd.Lines = 0
	}
	return cmd.ToRemoteCommand()
}

func (j *journalResumer) Observe(kind sshx.StreamChunkKind, line string) {
	if kind != sshx.StreamStdout {
		return
	}
	if m := journalISOTimestampPattern.FindString(line); m != "" {
		j.sinceISO = m
	}
	j.lastLine = line
}

func (j *journalResumer) Suppress(kind sshx.StreamChunkKind, line string) bool {
	if kind != sshx.StreamStdout {
		return false
	}
	return line == j.lastLine
}

// JournalCmd holds the parsed flags ready to be assembled into a
// remote `journalctl ...` invocation. Each field maps to one
// journalctl flag (or none, for localGrep).
//
// When PreferLog is true, ToRemoteCommand emits a macOS `log show` /
// `log stream` invocation instead -- the same flag set is translated
// to NSPredicate predicates and the `log` subcommand. See
// ToMacOSLogCommand for the mapping. This is opt-in (`--prefer log`
// on the CLI, `prefer_log: true` in the MCP arg) and exists so the
// same journal call shape works against macOS remotes where
// journalctl doesn't exist.
type JournalCmd struct {
	Unit      string
	Since     string
	Priority  string
	Lines     int
	Grep      string // server-side `-g REGEX` (journalctl >= 237)
	Follow    bool
	LocalGrep string // post-fetch client-side regex; overrides `grep`
	PreferLog bool   // dispatch to macOS `log` instead of journalctl
}

func ParseJournalArgs(args []string) (JournalCmd, error) {
	jc := JournalCmd{Lines: 0}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unit":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.Unit = v
			i++
		case strings.HasPrefix(a, "--unit="):
			jc.Unit = a[len("--unit="):]
		case a == "--since":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.Since = v
			i++
		case strings.HasPrefix(a, "--since="):
			jc.Since = a[len("--since="):]
		case a == "-p" || a == "--priority":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.Priority = v
			i++
		case a == "-n" || a == "--lines":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 0 {
				return jc, srvutil.Errf(2, "bad %s value %q", a, v)
			}
			jc.Lines = n
			i++
		case a == "-g" || a == "--grep":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.Grep = v
			i++
		case a == "--local-grep":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.LocalGrep = v
			i++
		case a == "-f" || a == "--follow":
			jc.Follow = true
		case a == "--prefer":
			// Opt-in dispatch to a non-systemd implementation. Only
			// `log` (macOS unified logging) is recognized today; any
			// other value errors loudly rather than silently falling
			// back to journalctl so a typo doesn't masquerade as a
			// successful command.
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			switch v {
			case "log":
				jc.PreferLog = true
			default:
				return jc, srvutil.Errf(2, "unknown --prefer value %q (only \"log\" is supported)", v)
			}
			i++
		case strings.HasPrefix(a, "--prefer="):
			v := a[len("--prefer="):]
			switch v {
			case "log":
				jc.PreferLog = true
			default:
				return jc, srvutil.Errf(2, "unknown --prefer value %q (only \"log\" is supported)", v)
			}
		case a == "-h" || a == "--help":
			return jc, srvutil.Errf(0, `usage: srv journal [-u UNIT] [--since TIME] [-p PRI] [-g RE] [-n LINES] [-f] [--prefer log]
  systemd journal viewer (one-shot or live-follow on the remote)

  --prefer log   dispatch to macOS unified logging (`+"`log show` / `log stream`"+`)
                 instead of journalctl. Use on macOS remotes where journalctl
                 doesn't exist. --since accepts duration like "10m" / "1h".

see also:
  srv tail [-n N] [--grep RE] <path>            any remote file
  srv logs <id> [-f]                            output of a detached srv job`)
		case a == "--":
			// No positional args expected; ignore the rest.
			return jc, nil
		default:
			return jc, srvutil.Errf(2, "unknown journal arg %q (try -u UNIT, --since DUR, -f, -n LINES, -g RE; --help for more)", a)
		}
	}
	return jc, nil
}

func needValue(args []string, i int, flag string) (string, error) {
	if i+1 >= len(args) {
		return "", srvutil.Errf(2, "%s requires a value", flag)
	}
	return args[i+1], nil
}

// ToRemoteCommand assembles the journalctl invocation. `--no-pager`
// keeps it quiet over a non-tty SSH session; `-o short-iso` is more
// greppable than the default colored timestamps.
//
// When PreferLog is set the call delegates to ToMacOSLogCommand and
// the result is a macOS `log show` / `log stream` invocation instead.
func (j JournalCmd) ToRemoteCommand() string {
	if j.PreferLog {
		return j.ToMacOSLogCommand()
	}
	parts := []string{"journalctl", "--no-pager", "-o", "short-iso"}
	if j.Unit != "" {
		parts = append(parts, "-u", srvtty.ShQuote(j.Unit))
	}
	if j.Since != "" {
		parts = append(parts, "--since", srvtty.ShQuote(j.Since))
	}
	if j.Priority != "" {
		parts = append(parts, "-p", srvtty.ShQuote(j.Priority))
	}
	if j.Lines > 0 {
		parts = append(parts, "-n", strconv.Itoa(j.Lines))
	}
	if j.Grep != "" {
		parts = append(parts, "-g", srvtty.ShQuote(j.Grep))
	}
	if j.Follow {
		parts = append(parts, "-f")
	}
	return strings.Join(parts, " ")
}

// ToMacOSLogCommand translates the JournalCmd shape into a macOS
// unified-logging invocation:
//
//   - Follow=false -> `log show ...` (one-shot)
//   - Follow=true  -> `log stream ...` (live; --last/--start don't apply)
//
// Mapping rules (all best-effort, documented in code so the gaps are
// visible rather than hidden behind a misleading interface):
//
//   - Unit       -> NSPredicate `(subsystem == "U" || process == "U")`
//   - Since      -> `log show --last <U>` (durations like "10m" / "1h");
//     in stream mode --since is ignored because `log
//     stream` is "from now" by definition.
//   - Priority   -> "err"/"error"/"fault" map to messageType predicate;
//     "info"/"debug" become `--info` / `--debug` flags;
//     other values pass through to predicate verbatim.
//   - Grep       -> predicate `eventMessage MATCHES "RE"` (NSPredicate
//     regex). For `log stream` this is server-side; for
//     `log show` ditto.
//   - Lines      -> appended as `| /usr/bin/tail -n N`. macOS `log` has
//     no native -n; piping post-hoc is the simplest sound
//     option and keeps the tool tree shallow.
//   - Follow     -> dispatch to `log stream` subcommand.
//
// We pin the output style to `syslog` for two reasons: (1) it's the
// most journalctl-ish line shape (compact, one-line-per-event) so
// downstream eyeballs / grep tooling don't need a per-platform branch;
// (2) it deliberately does NOT start lines with an ISO `T`-separated
// timestamp so the journalResumer's ISO regex naturally won't match,
// keeping the reconnect path a clean restart instead of a malformed
// --start argument injection on macOS.
func (j JournalCmd) ToMacOSLogCommand() string {
	var subcmd string
	if j.Follow {
		subcmd = "stream"
	} else {
		subcmd = "show"
	}
	// Use absolute path: macOS default shell (zsh) has `log` as a
	// builtin that prints recent login history and balks on subcmds
	// with `too many arguments`. The system `log` we actually want
	// lives at /usr/bin/log -- spelling the full path bypasses the
	// builtin and is also robust against a user's $PATH that puts
	// some other `log` ahead of it.
	parts := []string{"/usr/bin/log", subcmd, "--style", "syslog"}

	var preds []string
	if j.Unit != "" {
		// NSPredicate string literals use double quotes; the unit may
		// itself contain quotes / shell metacharacters, so wrap the
		// whole predicate fragment with srvtty.ShQuote at the end.
		q := nsPredicateQuote(j.Unit)
		preds = append(preds, fmt.Sprintf(`(subsystem == %s OR process == %s)`, q, q))
	}
	if j.Grep != "" {
		preds = append(preds, fmt.Sprintf(`eventMessage MATCHES %s`, nsPredicateQuote(j.Grep)))
	}
	// Priority mapping: keep the "show me errors and worse" intuition
	// from journalctl's err/warning/info/debug hierarchy.
	switch strings.ToLower(j.Priority) {
	case "":
		// no constraint
	case "err", "error", "crit", "alert", "emerg", "fault":
		preds = append(preds, `(messageType == "error" OR messageType == "fault")`)
	case "info":
		parts = append(parts, "--info")
	case "debug":
		parts = append(parts, "--info", "--debug")
	case "warning", "warn", "notice":
		// macOS unified logging has no "warning" category; the
		// closest match is "default at info+" -- include --info so
		// non-default messages aren't silently dropped.
		parts = append(parts, "--info")
	default:
		// Unknown priority: pass through as a literal predicate. If
		// it isn't a valid NSPredicate, `log` will print a parse
		// error on stderr -- preferable to silently dropping a
		// user-supplied filter.
		preds = append(preds, fmt.Sprintf(`messageType == %s`, nsPredicateQuote(j.Priority)))
	}
	if len(preds) > 0 {
		parts = append(parts, "--predicate", srvtty.ShQuote(strings.Join(preds, " AND ")))
	}
	// `log show` honors --last for relative durations like "10m";
	// `log stream` has no time window (it's strictly future events).
	if !j.Follow && j.Since != "" {
		parts = append(parts, "--last", srvtty.ShQuote(j.Since))
	}

	cmd := strings.Join(parts, " ")
	// macOS `log` has no `-n N`; pipe through tail when the caller
	// asked for a line cap. Only meaningful for one-shot reads --
	// `log stream`'s output is unbounded and the caller controls
	// duration via follow_seconds upstream.
	if j.Lines > 0 && !j.Follow {
		cmd += " | /usr/bin/tail -n " + strconv.Itoa(j.Lines)
	}
	return cmd
}

// missingJournalctlPattern matches both common shells' "command not
// found" wording around the journalctl binary. bash uses
// `journalctl: command not found`; zsh (default on modern macOS)
// uses `command not found: journalctl`. We accept either ordering on
// the same line.
var missingJournalctlPattern = regexp.MustCompile(`(?i)(journalctl: command not found|command not found:?\s*journalctl)`)

// MissingJournalctlHint returns a one-line hint string when a journal
// invocation failed because the remote doesn't have `journalctl`
// (most commonly: it's a macOS host). Returns "" when the error
// signature doesn't match -- callers print only on a non-empty
// return. `preferLog` is consulted so we don't double up the hint
// when the user is already on the macOS path (a different error
// shape -- e.g. `log` missing -- shouldn't get the journalctl hint).
func MissingJournalctlHint(exitCode int, stderr string, preferLog bool) string {
	if exitCode == 0 || preferLog {
		return ""
	}
	if !missingJournalctlPattern.MatchString(stderr) {
		return ""
	}
	return "srv journal: this remote has no journalctl. " +
		"For macOS hosts, retry with `srv journal --prefer log [...]` " +
		"(MCP: pass `prefer_log: true`)."
}

// nsPredicateQuote wraps `s` in NSPredicate-style double quotes,
// escaping any embedded double quote or backslash. The output is
// suitable for embedding inside a shell-quoted `--predicate '...'`
// argument; the outer shell quoting is applied by srvtty.ShQuote in
// the caller. Kept private because the only legitimate caller is
// ToMacOSLogCommand.
func nsPredicateQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
