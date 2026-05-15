package streams

import (
	"fmt"
	"os"
	"regexp"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/srvtty"
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
		return clierr.Errf(1, "%v", err)
	}
	remoteCmd := jc.ToRemoteCommand()
	cwd := config.GetCwd(profName, profile)

	if !jc.Follow {
		res, _ := remote.RunCapture(profile, cwd, remoteCmd)
		os.Stdout.WriteString(res.Stdout)
		if res.Stderr != "" {
			os.Stderr.WriteString(res.Stderr)
		}
		return clierr.Code(res.ExitCode)
	}

	var re *regexp.Regexp
	if jc.LocalGrep != "" {
		r, gerr := regexp.Compile(jc.LocalGrep)
		if gerr != nil {
			return clierr.Errf(2, "bad regex %q: %v", jc.LocalGrep, gerr)
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
type JournalCmd struct {
	Unit      string
	Since     string
	Priority  string
	Lines     int
	Grep      string // server-side `-g REGEX` (journalctl >= 237)
	Follow    bool
	LocalGrep string // post-fetch client-side regex; overrides `grep`
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
				return jc, clierr.Errf(2, "bad %s value %q", a, v)
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
		case a == "-h" || a == "--help":
			return jc, clierr.Errf(0, `usage: srv journal [-u UNIT] [--since TIME] [-p PRI] [-g RE] [-n LINES] [-f]
  systemd journal viewer (one-shot or live-follow on the remote)

see also:
  srv tail [-n N] [--grep RE] <path>            any remote file
  srv logs <id> [-f]                            output of a detached srv job`)
		case a == "--":
			// No positional args expected; ignore the rest.
			return jc, nil
		default:
			return jc, clierr.Errf(2, "unknown journal arg %q (try -u UNIT, --since DUR, -f, -n LINES, -g RE; --help for more)", a)
		}
	}
	return jc, nil
}

func needValue(args []string, i int, flag string) (string, error) {
	if i+1 >= len(args) {
		return "", clierr.Errf(2, "%s requires a value", flag)
	}
	return args[i+1], nil
}

// ToRemoteCommand assembles the journalctl invocation. `--no-pager`
// keeps it quiet over a non-tty SSH session; `-o short-iso` is more
// greppable than the default colored timestamps.
func (j JournalCmd) ToRemoteCommand() string {
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
