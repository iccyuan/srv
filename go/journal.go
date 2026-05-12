package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cmdJournal is a thin, opinionated wrapper around remote
// `journalctl`. The win over `srv run "journalctl ..."` is:
//   - `-f` mode uses streamWithReconnect so a flaky link doesn't kill
//     a long-running tail.
//   - Argument shape matches journalctl's familiar one (-u UNIT,
//     --since TIME, -p PRIORITY, -n LINES, -g REGEX, -f) so muscle
//     memory transfers.
//   - Synchronous (non -f) calls go through runRemoteCapture which
//     reuses the daemon connection pool.
//
// Linux-only by nature; on non-systemd remotes the remote command
// just errors and we forward that. We don't try to detect "no
// journalctl here" upfront because the user already has a profile
// pointing at the remote -- they know whether it has systemd.
func cmdJournal(args []string, cfg *Config, profileOverride string) error {
	jc, err := parseJournalArgs(args)
	if err != nil {
		return err
	}
	profName, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	remoteCmd := jc.toRemoteCommand()
	cwd := GetCwd(profName, profile)

	if !jc.follow {
		res, _ := runRemoteCapture(profile, cwd, remoteCmd)
		os.Stdout.WriteString(res.Stdout)
		if res.Stderr != "" {
			os.Stderr.WriteString(res.Stderr)
		}
		return exitCode(res.ExitCode)
	}

	var re *regexp.Regexp
	if jc.localGrep != "" {
		r, gerr := regexp.Compile(jc.localGrep)
		if gerr != nil {
			return exitErr(2, "bad regex %q: %v", jc.localGrep, gerr)
		}
		re = r
	}
	fmt.Fprintf(os.Stderr,
		"srv journal: following on %s   (Ctrl-C to stop, auto-reconnect on drop)\n",
		profName)
	onChunk := func(kind StreamChunkKind, line string) {
		if re != nil && !re.MatchString(line) {
			return
		}
		if kind == StreamStderr {
			fmt.Fprint(os.Stderr, line)
		} else {
			fmt.Fprint(os.Stdout, line)
		}
	}
	return streamWithReconnect(profile, remoteCmd, onChunk)
}

// journalCmd holds the parsed flags ready to be assembled into a
// remote `journalctl ...` invocation. Each field maps to one
// journalctl flag (or none, for localGrep).
type journalCmd struct {
	unit      string
	since     string
	priority  string
	lines     int
	grep      string // server-side `-g REGEX` (journalctl >= 237)
	follow    bool
	localGrep string // post-fetch client-side regex; overrides `grep`
}

func parseJournalArgs(args []string) (journalCmd, error) {
	jc := journalCmd{lines: 0}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unit":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.unit = v
			i++
		case strings.HasPrefix(a, "--unit="):
			jc.unit = a[len("--unit="):]
		case a == "--since":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.since = v
			i++
		case strings.HasPrefix(a, "--since="):
			jc.since = a[len("--since="):]
		case a == "-p" || a == "--priority":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.priority = v
			i++
		case a == "-n" || a == "--lines":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 0 {
				return jc, exitErr(2, "bad %s value %q", a, v)
			}
			jc.lines = n
			i++
		case a == "-g" || a == "--grep":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.grep = v
			i++
		case a == "--local-grep":
			v, err := needValue(args, i, a)
			if err != nil {
				return jc, err
			}
			jc.localGrep = v
			i++
		case a == "-f" || a == "--follow":
			jc.follow = true
		case a == "--":
			// No positional args expected; ignore the rest.
			return jc, nil
		default:
			return jc, exitErr(2, "unknown journal arg %q (try -u UNIT, --since DUR, -f, -n LINES, -g RE)", a)
		}
	}
	return jc, nil
}

func needValue(args []string, i int, flag string) (string, error) {
	if i+1 >= len(args) {
		return "", exitErr(2, "%s requires a value", flag)
	}
	return args[i+1], nil
}

// toRemoteCommand assembles the journalctl invocation. `--no-pager`
// keeps it quiet over a non-tty SSH session; `-o short-iso` is more
// greppable than the default colored timestamps.
func (j journalCmd) toRemoteCommand() string {
	parts := []string{"journalctl", "--no-pager", "-o", "short-iso"}
	if j.unit != "" {
		parts = append(parts, "-u", shQuote(j.unit))
	}
	if j.since != "" {
		parts = append(parts, "--since", shQuote(j.since))
	}
	if j.priority != "" {
		parts = append(parts, "-p", shQuote(j.priority))
	}
	if j.lines > 0 {
		parts = append(parts, "-n", strconv.Itoa(j.lines))
	}
	if j.grep != "" {
		parts = append(parts, "-g", shQuote(j.grep))
	}
	if j.follow {
		parts = append(parts, "-f")
	}
	return strings.Join(parts, " ")
}

// handleMCPJournal exposes journal to the MCP server. Bounded
// duration (same idea as `tail`): follow_seconds defaults to 30s,
// caps at 60s. Always non-follow if follow_seconds=0.
func handleMCPJournal(args map[string]any, cfg *Config, profileOverride string) toolResult {
	unit, _ := args["unit"].(string)
	since, _ := args["since"].(string)
	priority, _ := args["priority"].(string)
	lines := 100
	if v, ok := args["lines"].(float64); ok && v >= 0 {
		lines = int(v)
	}
	grep, _ := args["grep"].(string)
	follow := 0
	if v, ok := args["follow_seconds"].(float64); ok && v > 0 {
		follow = int(v)
	}
	if follow > 60 {
		follow = 60
	}

	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := GetCwd(profName, prof)

	jc := journalCmd{
		unit: unit, since: since, priority: priority, lines: lines, grep: grep,
		follow: follow > 0,
	}
	remoteCmd := jc.toRemoteCommand()

	if follow == 0 {
		res, _ := runRemoteCapture(prof, cwd, remoteCmd)
		text, truncatedBytes := buildMCPRunText(res, cwd)
		structured := map[string]any{
			"exit_code": res.ExitCode,
			"cwd":       cwd,
			"unit":      unit,
		}
		if truncatedBytes > 0 {
			structured["truncated_bytes"] = truncatedBytes
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           res.ExitCode != 0,
			StructuredContent: structured,
		}
	}

	// Follow mode: bounded tail-style streaming. Same shape as the
	// `tail` MCP tool -- dial direct, time-out via client close,
	// stream chunks via progress notifications.
	c, err := Dial(prof)
	if err != nil {
		return mcpTextErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	token := currentProgressTokenFn()
	timer := time.NewTimer(time.Duration(follow) * time.Second)
	defer timer.Stop()
	go func() {
		<-timer.C
		_ = c.Close()
	}()

	var buf strings.Builder
	var captured int
	var capped bool
	onChunk := func(_ StreamChunkKind, line string) {
		if captured+len(line) <= mcpRunTextMax {
			buf.WriteString(line)
			captured += len(line)
		} else {
			capped = true
		}
		mcpProgress(token, captured, line)
	}
	_, _, _, _ = c.RunStream(remoteCmd, cwd, onChunk)

	text := buf.String()
	if capped {
		text += fmt.Sprintf("\n[output cap %d bytes; further lines streamed via progress only]\n", mcpRunTextMax)
	}
	text += fmt.Sprintf("\n[followed journal on %s for %ds, %d bytes captured]", profName, follow, captured)
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"unit":           unit,
			"follow_seconds": follow,
			"bytes_captured": captured,
			"capped":         capped,
		},
	}
}
