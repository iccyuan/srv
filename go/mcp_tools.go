package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"srv/internal/srvtty"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MCP tool registry. ONE source of truth: mcpToolDefs() (advertised to
// the client via tools/list) and mcpHandleTool() (dispatch on
// tools/call) both read from `mcpTools`. Adding a tool means appending
// a single entry below; the def-list and the dispatch switch can never
// drift. Same OCP pattern as the CLI subcommand registry in commands.go.
//
// Each tool's handler is a top-level named function (`handleMCPRun`
// etc.). The registry just pairs a toolDef with its handler, which
// keeps the file scannable (each handler is one function, not a 100-
// line closure buried inside a struct literal) and each handler unit-
// testable in isolation.

// mcpRunTextMax caps the combined stdout+stderr the `run` tool returns
// to the MCP client. Beyond this, output is truncated with a marker
// pointing the caller at remote-side filtering.
//
// Rationale: the MCP client keeps every tool result in its conversation
// history, so a single `cat /var/log/...` or `journalctl -n 100000`
// permanently inflates the client's memory by the full payload. 64 KiB is
// enough for typical command output while drawing a hard line against
// runaway dumps.
const (
	mcpRunTextMax            = 64 * 1024
	mcpWaitJobDefaultSeconds = 8
	mcpWaitJobMaxSeconds     = 15
)

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolResult struct {
	Content           []toolContent `json:"content"`
	IsError           bool          `json:"isError,omitempty"`
	StructuredContent any           `json:"structuredContent,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// riskyPattern flags a remote command as destructive enough to require
// confirm=true when the session guard is on. Each pattern is a regex
// matched against the full command string; `name` is the human-readable
// label surfaced to the model in the block reason.
//
// The list is intentionally short and conservative -- false-positives
// are recoverable (re-issue with confirm=true), false-negatives are not.
// We cover the canonical "oh no" set: recursive force delete, raw-disk
// writes, mkfs, system halt, SQL drops, explicit truncates.
//
// Word boundaries (\b) keep us from matching `farm -rf` etc. Quoted
// content (echo "rm -rf /") is filtered out separately by runRiskyMatch
// via isInsideQuotes, since \b alone can't tell a real command position
// from a string-literal occurrence.
type riskyPattern struct {
	name string
	re   *regexp.Regexp
}

var runRiskyPatterns = []riskyPattern{
	// rm with both -r/-R and -f/-F somewhere in the flag bundle, or the
	// long-flag equivalents. Captures -rf, -fr, -rF, -Rf, -fR, -RF, -FR,
	// `--recursive --force` (either order), and short+long mixes.
	{"rm -rf", regexp.MustCompile(`(?i)\brm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF]\b|-[a-zA-Z]*[fF][a-zA-Z]*[rR]\b|--recursive\s+--force|--force\s+--recursive|-[rRfF]\s+--(?:recursive|force)\b|--(?:recursive|force)\s+-[rRfF]\b)`)},
	{"dd of=/...", regexp.MustCompile(`(?i)\bdd\s+(?:[^|;&\n]*\s)?(?:of=|if=/dev/(?:zero|random|urandom)\b)`)},
	{"mkfs", regexp.MustCompile(`(?i)\bmkfs(?:\.[a-z0-9]+)?\b`)},
	{"shutdown", regexp.MustCompile(`(?i)\bshutdown\b`)},
	{"reboot", regexp.MustCompile(`(?i)\breboot\b`)},
	{"halt", regexp.MustCompile(`(?i)\bhalt\b`)},
	{"poweroff", regexp.MustCompile(`(?i)\bpoweroff\b`)},
	{"drop database", regexp.MustCompile(`(?i)\bdrop\s+(?:database|table|schema)\b`)},
	{"truncate table", regexp.MustCompile(`(?i)\btruncate\s+(?:table\b|-)`)},
	{":>/", regexp.MustCompile(`:\s*>\s*/`)},
	{"chattr -i", regexp.MustCompile(`(?i)\bchattr\s+-i\b`)},
	{"> /dev/disk", regexp.MustCompile(`>\s*/dev/(?:sd|nvme|disk|hd)`)},
}

// runRiskyMatch reports the name of the first risky pattern present in
// `command`, or "" if none. Matches inside single- or double-quoted
// strings (e.g. `echo "rm -rf foo"`) are skipped -- they're operands,
// not commands. Quote tracking is best-effort: it handles common shell
// quoting but doesn't try to mirror full POSIX rules.
func runRiskyMatch(command string) string {
	if command == "" {
		return ""
	}
	for _, p := range runRiskyPatterns {
		for _, loc := range p.re.FindAllStringIndex(command, -1) {
			if !isInsideQuotes(command, loc[0]) {
				return p.name
			}
		}
	}
	return ""
}

// isInsideQuotes reports whether byte offset `pos` in `s` falls inside
// a `"..."` or `'...'` quoted region. Tracks backslash escapes inside
// double quotes (POSIX rule); single quotes are literal. Heredocs and
// $'...' are not modeled -- treating their contents as "real command"
// favors safety (catch the risky token) over precision.
func isInsideQuotes(s string, pos int) bool {
	inDouble, inSingle, escape := false, false, false
	for i := 0; i < pos && i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inDouble {
			escape = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		}
	}
	return inDouble || inSingle
}

// mcpTextErr wraps a plain string as an isError tool result. Used by
// every handler for pre-flight validation failures (missing args,
// profile not found, etc.).
func mcpTextErr(s string) toolResult {
	return toolResult{
		IsError: true,
		Content: []toolContent{{Type: "text", Text: s}},
	}
}

// runLongWaitSleep matches `sleep N` or `sleep N.X` where N > 5. Doesn't
// catch `sleep 5m` / `sleep 1h` (rare in AI-generated commands; would
// match if we tried, with false-positive risk on `sleep ${VAR}`).
var runLongWaitSleep = regexp.MustCompile(`\bsleep\s+(\d+(?:\.\d+)?)\b`)

// runForeverPatterns are commands that don't terminate on their own.
// `tail -f`, `watch`, and `journalctl -f` are the canonical sins.
//
// `[^;&|\n]*?` allows arbitrary intervening flags (`journalctl -u nginx -f`,
// `tail -n 100 -f log`) while still stopping at shell separators -- so
// `echo hi; journalctl -u svc -f` triggers, but a quoted argument cannot
// hop the separator boundary. Note: case-sensitive `-f` -- we do not
// flag `tail -F` (retry-on-truncate), which has legitimate non-blocking
// uses in scripted contexts.
var runForeverPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\btail\b[^;&|\n]*?\s(?:-f|--follow)\b`),
	regexp.MustCompile(`\bwatch\s+`),
	regexp.MustCompile(`\bjournalctl\b[^;&|\n]*?\s(?:-f|--follow)\b`),
}

// runRejectSync inspects a command planned for synchronous execution and
// returns a non-empty hint if it would block the MCP turn for too long.
// AI clients reach for sleep+poll loops by reflex, but those tie up the
// MCP per-tool timeout and produce the "tools no longer available" red
// dot. We catch the patterns here and route the model toward
// background=true / wait_job. Empty return = command is fine to run sync.
func runRejectSync(cmd string) string {
	if m := runLongWaitSleep.FindStringSubmatch(cmd); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil && n > 5 {
			return fmt.Sprintf("contains `sleep %s` which would block %ss synchronously", m[1], m[1])
		}
	}
	for _, re := range runForeverPatterns {
		if loc := re.FindStringIndex(cmd); loc != nil {
			return fmt.Sprintf("contains a never-terminating pattern (%s)", cmd[loc[0]:loc[1]])
		}
	}
	return ""
}

// runRejectMessage builds the educational error returned when sync run
// hits a long-blocking pattern. Tells the model exactly what to swap to.
func runRejectMessage(cmd, why string) string {
	return fmt.Sprintf(
		"rejected: %s. Synchronous `run` is bound by the MCP per-tool timeout (default 60s); long blocks tank the connection.\n\nUse the background pattern instead:\n  run { command: %q, background: true }   -> returns job_id immediately\n  wait_job { id: <returned id> }           -> short polls (default 8s, cap 15s)\n\nFor commands that legitimately need their full output streamed back synchronously, restructure them to finish in <60s (e.g. cap with `head`/`timeout 30`).",
		why, cmd,
	)
}

// Token-economy gates for MCP `run` / `run_stream`. The 64 KiB result
// cap stops the model from drowning in output, but it doesn't stop
// the WASTED tokens that get paid when the model asks for an
// unbounded source and we serve them the wrong 64 KiB slice. Forcing
// an explicit slicing decision usually returns more relevant content
// AND saves tokens; the model can read the rejection and pick a
// `head -n N` / `tail -n N` / `grep` / dedicated MCP tool path.

var (
	// catWithArg matches `cat <something>` at a command-position. We
	// don't reject `cat` with no arg (it's just `stdin -> stdout`).
	reBareCat = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*cat\s+\S`)
	// dmesg / journalctl / find anywhere as a verb at command position.
	reBareDmesg      = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*dmesg\b`)
	reBareJournalctl = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*journalctl\b`)
	reBareFind       = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*find\s+/`)

	// Pipe into a limiter -- if we see this anywhere, downstream is
	// bounded and we let the call through. Conservative list of
	// commands that genuinely bound output.
	reDownstreamLimiter = regexp.MustCompile(`(?i)\|\s*(head|tail|grep|awk|sed|wc|cut|jq|sort|uniq|column|less|more|fold|tr|xxd|od)\b`)
	// head -n N / head -c N anywhere in the pipeline counts as a
	// bound, even without piping (e.g. `head -n 100 file`).
	reHeadBounded = regexp.MustCompile(`(?i)\bhead\s+-[cnN][= ]?\s*\d`)
	// tail with -n N (default 10 lines, but explicit is explicit).
	reTailBounded = regexp.MustCompile(`(?i)\btail\s+-n[= ]?\s*\d`)
	// journalctl-flag detector. -u / --unit / --since / --until /
	// -S / -U / -n / -g / --grep / -p / --priority / -k / -f all
	// constrain output. Anchored to the `journalctl` token so a
	// random `-u` elsewhere doesn't satisfy it.
	reJournalctlFiltered = regexp.MustCompile(`(?i)\bjournalctl\b[^|;&\n]*?(\s-u\b|\s--unit\b|\s--since\b|\s--until\b|\s-S\b|\s-U\b|\s-n\b|\s-g\b|\s--grep\b|\s-p\b|\s--priority\b|\s-k\b|\s-f\b)`)
	// find-flag detector. Any of the narrowing predicates counts.
	reFindFiltered = regexp.MustCompile(`(?i)\bfind\b[^|;&\n]*?\s-(maxdepth|name|iname|type|newer|mtime|mmin|size|path|prune|regex|wholename)\b`)
)

// runRejectUnfiltered checks `cmd` (already quote-stripped) for
// "dumps everything" patterns. Returns (label, message) when the
// command would likely produce unbounded output; ("", "") to proceed.
//
// Patterns checked, in order:
//   - `cat <file>`           (no native limit; demand slicing)
//   - `dmesg`                (kernel ring buffer; demand filter pipe)
//   - `journalctl ...`       (without -u / --since / -p / -g / -n / -k / -f)
//   - `find /path ...`       (without -maxdepth / -name / -type / ...)
//
// Each check is short-circuited by a "downstream limiter": a pipe
// into head / tail / grep / wc / ... is enough to call the output
// bounded, since the model has made an explicit slicing decision.
func runRejectUnfiltered(cmd string) (string, string) {
	stripped := stripShellQuotedContent(cmd)
	// Trust the model when there's any downstream limiter or an
	// explicit head/tail -n N anywhere -- it has chosen a slice.
	if reDownstreamLimiter.MatchString(stripped) ||
		reHeadBounded.MatchString(stripped) ||
		reTailBounded.MatchString(stripped) {
		return "", ""
	}

	if reBareCat.MatchString(stripped) {
		return "cat", "`cat <file>` returns the whole file with no native limit. Pick a slice:\n" +
			"  run { command: \"head -n 100 <file>\" }\n" +
			"  run { command: \"tail -n 100 <file>\" }\n" +
			"  run { command: \"grep PATTERN <file>\" }\n" +
			"  tail { path: \"<file>\", lines: 100 }   (dedicated MCP tool, no `cat` needed)"
	}
	if reBareDmesg.MatchString(stripped) {
		return "dmesg", "`dmesg` dumps the entire kernel ring buffer (often hundreds of KB). Add a downstream slicer:\n" +
			"  run { command: \"dmesg | tail -n 100\" }\n" +
			"  run { command: \"dmesg | grep -i error\" }"
	}
	if reBareJournalctl.MatchString(stripped) && !reJournalctlFiltered.MatchString(stripped) {
		return "journalctl", "`journalctl` with no filter returns the whole journal. Use the dedicated MCP tool:\n" +
			"  journal { unit: \"nginx.service\", lines: 100 }\n" +
			"or add a filter to the run call:\n" +
			"  run { command: \"journalctl -u nginx -n 100\" }\n" +
			"  run { command: \"journalctl --since '10 min ago' -p err\" }"
	}
	if reBareFind.MatchString(stripped) && !reFindFiltered.MatchString(stripped) {
		return "find", "`find <path>` with no narrowing flags can traverse arbitrarily large trees. Add one of: -maxdepth N, -name PATTERN, -type f/d, -newer FILE, -mtime N.\n" +
			"  run { command: \"find /var/log -maxdepth 2 -name '*.log'\" }"
	}
	return "", ""
}

// stripShellQuotedContent removes the contents (but not the
// delimiters) of double / single-quoted strings in `s`. Used to keep
// the unbounded-pattern matchers from false-positiving on `echo "cat
// foo"` or `grep "journalctl" log`. Best-effort: backslash escapes
// inside double quotes are honored, single-quoted runs are literal,
// $'...' and heredocs are not modeled.
func stripShellQuotedContent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inDouble, inSingle, escape := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inDouble {
			escape = true
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			b.WriteByte(c)
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			b.WriteByte(c)
			continue
		}
		if inDouble || inSingle {
			// Drop quoted byte but keep a placeholder space so word
			// boundaries on either side of the quoted region still
			// work (e.g. `echo "rm -rf"` shouldn't make `echo` and
			// `-rf` look adjacent).
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// runRejectUnfilteredMessage formats the rejection into the standard
// MCP shape: a clear text explanation + structured metadata the
// client can branch on.
func runRejectUnfilteredMessage(label, body string) toolResult {
	r := mcpTextErr("rejected: " + body)
	r.StructuredContent = map[string]any{
		"rejected_reason": "unbounded_output",
		"pattern":         label,
	}
	return r
}

func mcpDetachedResult(rec *JobRecord) toolResult {
	info := map[string]any{
		"job_id":    rec.ID,
		"status":    "running",
		"profile":   rec.Profile,
		"pid":       rec.Pid,
		"log":       rec.Log,
		"cwd":       rec.Cwd,
		"started":   rec.Started,
		"next_tool": "wait_job",
	}
	text := fmt.Sprintf(
		"started job %s pid=%d profile=%s\npoll with wait_job id=%s max_wait_seconds=%d",
		rec.ID, rec.Pid, rec.Profile, rec.ID, mcpWaitJobDefaultSeconds,
	)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: info,
	}
}

// mcpToolHandler is the uniform handler signature. The dispatcher
// extracts profileOverride from args once and passes it explicitly so
// each handler doesn't repeat the extraction.
type mcpToolHandler func(args map[string]any, cfg *Config, profileOverride string) toolResult

type mcpTool struct {
	def     toolDef
	handler mcpToolHandler
}

// strSchema builds a string-type JSON schema fragment. Empty desc maps
// to a bare {"type": "string"} -- shaving "description":"" off every
// passthrough field keeps the tools/list payload compact.
func strSchema(desc string) map[string]any {
	if desc == "" {
		return map[string]any{"type": "string"}
	}
	return map[string]any{"type": "string", "description": desc}
}

// boolSchema with default value.
func boolSchema(def bool, desc string) map[string]any {
	out := map[string]any{"type": "boolean", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// intSchema with default + description.
func intSchema(def int, desc string) map[string]any {
	out := map[string]any{"type": "integer", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// resolveMCPProfile is the per-handler wrapper around ResolveProfile.
// Returns (name, profile, nil) on success or ("", nil, errResult) when
// resolution fails -- callers `return *errResult` to short-circuit.
// This replaces the 12-call repetition of ResolveProfile + mcpTextErr
// across the registry.
func resolveMCPProfile(cfg *Config, override string) (string, *Profile, *toolResult) {
	name, prof, err := ResolveProfile(cfg, override)
	if err != nil {
		r := mcpTextErr(err.Error())
		return "", nil, &r
	}
	return name, prof, nil
}

// guardCheckRisky returns a blocking toolResult when the session guard
// is on AND `cmd` matches a high-risk pattern AND the caller didn't
// pass confirm=true. Returns nil to mean "allowed". Used by `run`,
// `detach`, and any other tool that ferries a raw shell command to the
// remote.
func guardCheckRisky(tool, cmd string, confirm bool) *toolResult {
	if !GuardOn() || confirm {
		return nil
	}
	pat := runRiskyMatch(cmd)
	if pat == "" {
		return nil
	}
	r := guardBlocked(tool, fmt.Sprintf("command contains a high-risk pattern %q", pat))
	return &r
}

// emptyFilterRegex matches grep patterns that filter nothing in
// practice -- a `.*` / `.` / `.+` / `[\s\S]*` "filter" is a bypass
// dressed up as a regex. Anchored matches only after whitespace trim;
// real-world filters always have at least one literal character, so
// we won't false-positive a meaningful pattern.
var emptyFilterRegex = regexp.MustCompile(`^(\.[\*\+\?]?|\[.*?\][\*\+\?]?)$`)

// isMeaningfulFilter reports whether `s` is a grep / unit / since
// value that actually constrains output. Empty, whitespace-only, and
// "matches everything" patterns count as no filter.
func isMeaningfulFilter(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if emptyFilterRegex.MatchString(s) {
		return false
	}
	return true
}

// requireStreamFilter is the gate that follow-mode MCP streaming
// tools call before kicking off a stream. Rule: any `follow_seconds
// > 0` requires at least one meaningful output filter. Earlier
// versions exempted "short" follows (≤5s), but a chatty log can flood
// the per-call progress channel even in five seconds, and the
// exemption left no incentive for the model to ever pass a filter.
// Strict rule, no exemption.
//
//	toolName     for the error message
//	follow       follow_seconds the caller passed (0 = one-shot, no gate)
//	filters      list of caller-supplied filter strings (grep, unit,
//	             since, priority, ...); any meaningful entry counts.
//	hint         single-line example the model can copy-paste to fix.
//
// Returns nil to proceed, or a populated toolResult that the caller
// should `return *r`.
func requireStreamFilter(toolName string, follow int, filters []string, hint string) *toolResult {
	if follow <= 0 {
		return nil // one-shot, not streaming
	}
	for _, f := range filters {
		if isMeaningfulFilter(f) {
			return nil
		}
	}
	msg := fmt.Sprintf(
		"streaming `%s` (follow_seconds=%d) requires at least one output filter to keep token cost bounded. The cap on the final tool-result text does NOT cap the progress-notification stream, so even a short unfiltered follow can flood the conversation. Add a constraint and retry; or omit follow_seconds for a one-shot fetch.\n\nExample: %s",
		toolName, follow, hint,
	)
	r := mcpTextErr(msg)
	r.StructuredContent = map[string]any{
		"rejected_reason": "unbounded_streaming",
		"follow_seconds":  follow,
	}
	return &r
}

// clampLines bounds the user-supplied `lines` value to `max` and
// signals via the second return whether clamping happened (callers
// can surface it in the response so the model knows).
func clampLines(asked, max int) (int, bool) {
	if asked > max {
		return max, true
	}
	return asked, false
}

// -----------------------------------------------------------------------------
// Named tool handlers. Each function below is the handler for one MCP tool
// and is referenced from the `mcpTools` registry at the bottom of the file.
// Signatures match mcpToolHandler so they're interchangeable with the
// dispatch loop.
// -----------------------------------------------------------------------------

func handleMCPRun(args map[string]any, cfg *Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return mcpTextErr("error: command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run", cmd, confirm); blocked != nil {
		return *blocked
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	background, _ := args["background"].(bool)
	if background {
		rec, err := spawnDetached(profName, prof, cmd)
		if err != nil {
			return mcpTextErr(err.Error())
		}
		return mcpDetachedResult(rec)
	}
	// Hard-reject sync calls that would block the MCP turn for too
	// long. Description tells the model what to do instead; this
	// catches the case where it ignored that and went with the
	// reflex sleep+poll pattern anyway.
	if why := runRejectSync(cmd); why != "" {
		return mcpTextErr(runRejectMessage(cmd, why))
	}
	// Token-economy gate: reject `cat <file>` / `dmesg` / unfiltered
	// `journalctl` / unfiltered `find /` and friends -- they have no
	// native upper bound, so even the 64 KiB result cap pays tokens for
	// the wrong slice. Model is told exactly what slicer to add.
	if label, msg := runRejectUnfiltered(cmd); label != "" {
		return runRejectUnfilteredMessage(label, msg)
	}
	cwd := GetCwd(profName, prof)
	res, _ := runRemoteCapture(prof, cwd, cmd)
	text, truncatedBytes := buildMCPRunText(res, cwd)
	structured := map[string]any{
		"exit_code": res.ExitCode,
		"cwd":       cwd,
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

// handleMCPRunStream is the streaming variant of `run`. While the
// remote command executes, every line of stdout/stderr is pushed to
// the client as a `notifications/progress` notification tagged with
// the caller's progressToken. The final `tools/call` response still
// carries the full captured output (capped at mcpRunTextMax) and the
// exit code -- progress notifications are informational, not the
// authoritative output, so a client that ignores them still gets the
// same shape as `run`.
//
// Why this exists: the synchronous `run` tool is bound by the MCP
// per-tool timeout (Claude Code default 60s) -- a 30s build sits
// silent until completion and risks the "tools no longer available"
// red dot if it slips past the bound. Streaming keeps progress
// flowing so the client doesn't time out, and lets the model see
// partial output before the command finishes.
func handleMCPRunStream(args map[string]any, cfg *Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return mcpTextErr("error: command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run_stream", cmd, confirm); blocked != nil {
		return *blocked
	}
	// Same token-economy gate as plain `run` -- streaming makes the
	// unbounded-source problem worse, not better, since progress
	// notifications add their own token cost on top of the final
	// result.
	if label, msg := runRejectUnfiltered(cmd); label != "" {
		return runRejectUnfilteredMessage(label, msg)
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := GetCwd(profName, prof)
	token := currentProgressTokenFn()

	// Direct dial -- the daemon's stream_run op exists but is wired for
	// CLI consumption (writes to stdout); routing through it would
	// require yet another adapter. Cold handshake hits the same ~2.7s
	// cost as any non-pooled tool, which is fine for an explicitly-
	// streaming call (the streaming masks the dial cost).
	c, err := Dial(prof)
	if err != nil {
		return mcpTextErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	// progress counter: byte-based, monotonic. Some MCP clients use
	// progress to drive a UI bar; bytes is meaningful enough without
	// knowing the unbounded total.
	var emitted int
	onChunk := func(_ StreamChunkKind, line string) {
		emitted += len(line)
		mcpProgress(token, emitted, line)
	}

	exitCode, stdout, stderr, runErr := c.RunStream(cmd, cwd, onChunk)
	if runErr != nil {
		return mcpTextErr(fmt.Sprintf("stream run: %v", runErr))
	}

	res := &RunCaptureResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
		Cwd:      cwd,
	}
	text, truncatedBytes := buildMCPRunText(res, cwd)
	structured := map[string]any{
		"exit_code":     exitCode,
		"cwd":           cwd,
		"bytes_emitted": emitted,
	}
	if truncatedBytes > 0 {
		structured["truncated_bytes"] = truncatedBytes
	}
	if token != nil {
		structured["streamed"] = true
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           exitCode != 0,
		StructuredContent: structured,
	}
}

// handleMCPTail follows a remote file for a bounded duration and
// streams new lines to the client via `notifications/progress`. The
// final tools/call response returns the full accumulated output
// (capped at mcpRunTextMax) plus structured metadata.
//
// Why not just use `run` with `tail -F`: synchronous `run` rejects
// long-blocking patterns including `tail -f` for the MCP timeout
// reason. `tail` here is the explicit, bounded-time version: stream
// for up to follow_seconds (max 60s), then return -- the model gets
// real-time progress mid-call AND a deterministic upper bound on the
// turn duration.
func handleMCPTail(args map[string]any, cfg *Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return mcpTextErr("path is required")
	}
	lines := 50
	if v, ok := args["lines"].(float64); ok && v > 0 {
		lines = int(v)
	}
	// Hard cap on backfill -- a request for `lines: 100000` would
	// produce hundreds of KB of one-shot output regardless of follow
	// mode. Clamp transparently and tell the model.
	var linesClamped bool
	lines, linesClamped = clampLines(lines, 1000)

	// Default is one-shot (no follow). A no-arg `tail {path}` call
	// fetches the last `lines` of the file and returns. Models that
	// want a live follow opt in via follow_seconds AND a grep filter
	// -- the token-economy gate (below) enforces the pairing.
	follow := 0
	if v, ok := args["follow_seconds"].(float64); ok && v > 0 {
		follow = int(v)
	}
	// Hard cap on follow_seconds. The MCP per-tool timeout (Claude Code
	// default 60s) is the binding constraint; progress notifications
	// reset it but we still want a deterministic ceiling.
	if follow > 60 {
		follow = 60
	}

	grep, _ := args["grep"].(string)

	// Token-economy gate: a long follow on a chatty log emits megabytes
	// of progress notifications regardless of the final-result cap. We
	// require `grep` whenever the caller asks for more than a brief
	// spot-check window. The CLI doesn't have this constraint -- only
	// MCP, since only there does volume translate directly to tokens.
	if r := requireStreamFilter("tail", follow,
		[]string{grep},
		`{ path: "/var/log/app.log", follow_seconds: 30, grep: "ERROR|WARN" }`,
	); r != nil {
		return *r
	}

	var re *regexp.Regexp
	if grep != "" {
		r, err := regexp.Compile(grep)
		if err != nil {
			return mcpTextErr(fmt.Sprintf("bad regex %q: %v", grep, err))
		}
		re = r
	}

	_, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}

	c, err := Dial(prof)
	if err != nil {
		return mcpTextErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	remoteCmd := fmt.Sprintf("tail -F -n %d %s", lines, srvtty.ShQuotePath(path))
	token := currentProgressTokenFn()

	// Bound the call: close the SSH client after follow_seconds so
	// RunStream returns. The model sees a stream that politely ends
	// instead of one that runs until the MCP timeout hits.
	timer := time.NewTimer(time.Duration(follow) * time.Second)
	defer timer.Stop()
	stopOnce := sync.Once{}
	stop := func() { stopOnce.Do(func() { _ = c.Close() }) }
	go func() {
		<-timer.C
		stop()
	}()

	var capturedBytes int
	var capped bool
	var buf strings.Builder
	onChunk := func(_ StreamChunkKind, line string) {
		if re != nil && !re.MatchString(line) {
			return
		}
		// Accumulate up to the run-text cap; further chunks still
		// stream via progress (model sees them in real time) but the
		// final result text gets a truncation marker.
		if capturedBytes+len(line) <= mcpRunTextMax {
			buf.WriteString(line)
			capturedBytes += len(line)
		} else {
			capped = true
		}
		mcpProgress(token, capturedBytes, line)
	}

	_, _, _, runErr := c.RunStream(remoteCmd, "", onChunk)
	// runErr is expected when we close the client to end the follow;
	// that's the normal exit path. Surface only as info, never as
	// IsError.

	text := buf.String()
	if capped {
		text += fmt.Sprintf("\n[output cap %d bytes; further lines streamed via progress only]\n", mcpRunTextMax)
	}
	text += fmt.Sprintf("\n[followed %s for %ds, %d bytes captured]", path, follow, capturedBytes)
	structured := map[string]any{
		"path":           path,
		"follow_seconds": follow,
		"bytes_captured": capturedBytes,
		"capped":         capped,
		"lines_clamped":  linesClamped,
		"end_reason":     "timer",
	}
	if runErr != nil && !errors.Is(runErr, os.ErrClosed) {
		structured["transport_error"] = runErr.Error()
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}

func handleMCPCd(args map[string]any, cfg *Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	newCwd, err := changeRemoteCwd(profName, prof, path)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: newCwd}},
		StructuredContent: map[string]any{"cwd": newCwd, "profile": profName},
	}
}

func handleMCPPwd(args map[string]any, cfg *Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := GetCwd(profName, prof)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: cwd}},
		StructuredContent: map[string]any{"cwd": cwd, "profile": profName},
	}
}

func handleMCPUse(args map[string]any, cfg *Config, profileOverride string) toolResult {
	clear, _ := args["clear"].(bool)
	if clear {
		sid, _ := SetSessionProfile("")
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: unpinned", sid)}},
			StructuredContent: map[string]any{"session": sid, "profile": nil},
		}
	}
	target, _ := args["profile"].(string)
	if target == "" {
		sid, rec := TouchSession()
		info := map[string]any{
			"session": sid,
			"pinned":  nil,
			"default": cfg.DefaultProfile,
		}
		if rec.Profile != nil {
			info["pinned"] = *rec.Profile
		}
		b, _ := json.MarshalIndent(info, "", "  ")
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: string(b)}},
			StructuredContent: info,
		}
	}
	if _, ok := cfg.Profiles[target]; !ok {
		return mcpTextErr(fmt.Sprintf("profile %q not found", target))
	}
	sid, _ := SetSessionProfile(target)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: fmt.Sprintf("session %s: pinned to %q", sid, target)}},
		StructuredContent: map[string]any{"session": sid, "profile": target},
	}
}

func handleMCPStatus(args map[string]any, cfg *Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	sid, rec := TouchSession()
	var pinned any
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	multiplex := prof.Multiplex == nil || *prof.Multiplex
	return mcpJSONResult(map[string]any{
		"profile":       profName,
		"pinned":        pinned,
		"host":          prof.Host,
		"user":          prof.User,
		"port":          prof.GetPort(),
		"identity_file": prof.IdentityFile,
		"cwd":           GetCwd(profName, prof),
		"session":       sid,
		"multiplex":     multiplex,
		"compression":   prof.GetCompression(),
		"guard":         GuardOn(),
	})
}

func handleMCPListProfiles(args map[string]any, cfg *Config, profileOverride string) toolResult {
	sid, rec := TouchSession()
	var pinned any
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	return mcpJSONResult(map[string]any{
		"default":  cfg.DefaultProfile,
		"pinned":   pinned,
		"session":  sid,
		"profiles": names,
	})
}

func handleMCPCheck(args map[string]any, cfg *Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	res := runCheck(prof)
	var advice []string
	if !res.OK {
		advice = checkAdvice(res.Diagnosis, prof, profName)
	}
	info := map[string]any{
		"profile":   profName,
		"host":      prof.Host,
		"user":      prof.User,
		"port":      prof.GetPort(),
		"ok":        res.OK,
		"diagnosis": res.Diagnosis,
		"exit_code": res.ExitCode,
	}
	var text string
	if res.OK {
		target := prof.Host
		if prof.User != "" {
			target = prof.User + "@" + prof.Host
		}
		text = fmt.Sprintf("OK -- %s: %s key auth works.", profName, target)
	} else {
		text = fmt.Sprintf("FAIL (%s): %s\n\n%s", res.Diagnosis,
			strings.TrimSpace(res.Stderr), strings.Join(advice, "\n"))
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           !res.OK,
		StructuredContent: info,
	}
}

func handleMCPDoctor(args map[string]any, cfg *Config, profileOverride string) toolResult {
	checks, ok := doctorChecks(cfg, profileOverride)
	res := mcpJSONResult(map[string]any{"ok": ok, "checks": checks})
	res.IsError = !ok
	return res
}

func handleMCPDaemonStatus(args map[string]any, cfg *Config, profileOverride string) toolResult {
	conn := daemonDial(time.Second)
	if conn == nil {
		return mcpJSONResult(map[string]any{"running": false})
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, 2*time.Second)
	if err != nil || resp == nil {
		return mcpTextErr(fmt.Sprintf("daemon status failed: %v", err))
	}
	return mcpJSONResult(map[string]any{
		"running":         true,
		"uptime_sec":      resp.Uptime,
		"profiles_pooled": resp.Profiles,
		"protocol":        resp.V,
	})
}

func handleMCPEnv(args map[string]any, cfg *Config, profileOverride string) toolResult {
	action, _ := args["action"].(string)
	if action == "" {
		action = "list"
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	key, _ := args["key"].(string)
	value, _ := args["value"].(string)
	switch action {
	case "list":
	case "set":
		if key == "" {
			return mcpTextErr("key is required")
		}
		if prof.Env == nil {
			prof.Env = map[string]string{}
		}
		prof.Env[key] = value
		if err := SaveConfig(cfg); err != nil {
			return mcpTextErr(err.Error())
		}
	case "unset":
		if key == "" {
			return mcpTextErr("key is required")
		}
		delete(prof.Env, key)
		if err := SaveConfig(cfg); err != nil {
			return mcpTextErr(err.Error())
		}
	case "clear":
		prof.Env = nil
		if err := SaveConfig(cfg); err != nil {
			return mcpTextErr(err.Error())
		}
	default:
		return mcpTextErr("unknown env action")
	}
	return mcpJSONResult(map[string]any{"profile": profName, "env": prof.Env})
}

func handleMCPDiff(args map[string]any, cfg *Config, profileOverride string) toolResult {
	local, _ := args["local"].(string)
	if local == "" {
		return mcpTextErr("local is required")
	}
	remote, _ := args["remote"].(string)
	text, rc, err := diffLocalRemote(cfg, profileOverride, local, remote)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: map[string]any{"exit_code": rc, "local": local, "remote": remote},
	}
}

func handleMCPPush(args map[string]any, cfg *Config, profileOverride string) toolResult {
	local, _ := args["local"].(string)
	if local == "" {
		return mcpTextErr("local is required")
	}
	if _, err := os.Stat(local); err != nil {
		return mcpTextErr(fmt.Sprintf("local path missing: %q", local))
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := GetCwd(profName, prof)
	remote, _ := args["remote"].(string)
	if remote == "" {
		remote = filepath.Base(local)
	}
	abs := resolveRemotePath(remote, cwd)
	st, _ := os.Stat(local)
	recursive := false
	if rb, ok := args["recursive"].(bool); ok {
		recursive = rb
	}
	if st != nil && st.IsDir() {
		recursive = true
	}
	start := time.Now()
	rc, finalRemote, perr := pushPath(prof, local, abs, recursive)
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		bytes = sumLocalSize(local)
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("uploaded %s -> %s [exit 0]%s", local, finalRemote, fmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("upload FAILED %s -> %s [exit %d]", local, finalRemote, rc)
		if perr != nil {
			text += ": " + perr.Error()
		}
	}
	structured := map[string]any{
		"exit_code":        rc,
		"remote":           finalRemote,
		"local":            local,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handleMCPPull(args map[string]any, cfg *Config, profileOverride string) toolResult {
	remote, _ := args["remote"].(string)
	if remote == "" {
		return mcpTextErr("remote is required")
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := GetCwd(profName, prof)
	local, _ := args["local"].(string)
	if local == "" {
		local = "."
	}
	abs := resolveRemotePath(remote, cwd)
	recursive := false
	if rb, ok := args["recursive"].(bool); ok {
		recursive = rb
	}
	start := time.Now()
	rc, finalLocal, perr := pullPath(prof, abs, local, recursive)
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		bytes = sumLocalSize(finalLocal)
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("downloaded %s -> %s [exit 0]%s", abs, finalLocal, fmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("download FAILED %s -> %s [exit %d]", abs, finalLocal, rc)
		if perr != nil {
			text += ": " + perr.Error()
		}
	}
	structured := map[string]any{
		"exit_code":        rc,
		"remote":           abs,
		"local":            finalLocal,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handleMCPSync(args map[string]any, cfg *Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	o := &syncOpts{gitScope: "all"}
	if v, ok := args["remote_root"].(string); ok {
		o.remoteRoot = v
	}
	if v, ok := args["mode"].(string); ok {
		o.mode = v
	}
	if v, ok := args["git_scope"].(string); ok {
		o.gitScope = v
	}
	if v, ok := args["since"].(string); ok {
		o.since = v
	}
	if v, ok := args["root"].(string); ok {
		o.root = v
	}
	if v, ok := args["dry_run"].(bool); ok {
		o.dryRun = v
	}
	if v, ok := args["delete"].(bool); ok {
		o.delete = v
	}
	if v, ok := args["yes"].(bool); ok {
		o.yes = v
	}
	if o.delete && !o.dryRun && GuardOn() {
		confirm, _ := args["confirm"].(bool)
		if !confirm {
			return guardBlocked("sync",
				"delete=true would remove remote files")
		}
	}
	if v, ok := args["delete_limit"].(float64); ok {
		o.deleteLimit = int(v)
	}
	if v, ok := args["include"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.include = append(o.include, s)
			}
		}
	}
	if v, ok := args["exclude"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.exclude = append(o.exclude, s)
			}
		}
	}
	if v, ok := args["files"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				o.files = append(o.files, s)
			}
		}
	}
	localRoot := o.root
	if localRoot == "" {
		localRoot = findGitRoot(mustCwd())
		if localRoot == "" {
			localRoot = mustCwd()
		}
	}
	if o.mode == "" {
		if findGitRoot(localRoot) != "" {
			o.mode = "git"
		} else if len(o.include) > 0 {
			o.mode = "glob"
		} else if o.since != "" {
			o.mode = "mtime"
		} else if len(o.files) > 0 {
			o.mode = "list"
		} else {
			return mcpTextErr("no mode resolved (not a git repo and no include/since/files)")
		}
	}
	cwd := GetCwd(profName, prof)
	remoteRoot := cwd
	if o.remoteRoot != "" {
		remoteRoot = resolveRemotePath(o.remoteRoot, cwd)
	} else if prof.SyncRoot != "" {
		remoteRoot = resolveRemotePath(prof.SyncRoot, cwd)
	}
	allExcludes := append([]string{}, o.exclude...)
	allExcludes = append(allExcludes, prof.SyncExclude...)
	allExcludes = append(allExcludes, defaultSyncExcludes...)
	files, err := collectSyncFiles(o, localRoot, allExcludes)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	deletes, err := collectSyncDeletes(o, localRoot, allExcludes)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	limit := o.deleteLimit
	if limit == 0 {
		limit = 20
	}
	if len(deletes) > limit && !o.dryRun && !o.yes {
		return mcpTextErr(fmt.Sprintf("sync delete would remove %d files (limit %d). Re-run dry_run=true, yes=true, or set delete_limit.", len(deletes), limit))
	}
	if len(files) == 0 && len(deletes) == 0 {
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: "(nothing to sync)"}},
			StructuredContent: map[string]any{"files": []string{}, "deletes": deletes, "remote_root": remoteRoot, "exit_code": 0},
		}
	}
	if o.dryRun {
		text := fmt.Sprintf("would sync %d files to %s\n", len(files), remoteRoot)
		lim := len(files)
		if lim > 200 {
			lim = 200
		}
		text += strings.Join(files[:lim], "\n")
		if len(files) > 200 {
			text += fmt.Sprintf("\n... (%d more)", len(files)-200)
		}
		if len(deletes) > 0 {
			text += "\nwould delete:\n" + strings.Join(deletes, "\n")
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			StructuredContent: map[string]any{
				"files_count":   len(files),
				"deletes_count": len(deletes),
				"remote_root":   remoteRoot,
				"dry_run":       true,
			},
		}
	}
	rc := 0
	var terr error
	start := time.Now()
	if len(files) > 0 {
		rc, terr = tarUploadStream(prof, localRoot, files, remoteRoot)
	}
	if rc == 0 && len(deletes) > 0 {
		rc, terr = deleteRemoteFiles(prof, remoteRoot, deletes)
	}
	duration := time.Since(start)
	var bytes int64
	if rc == 0 {
		for _, f := range files {
			if st, err := os.Stat(filepath.Join(localRoot, f)); err == nil {
				bytes += st.Size()
			}
		}
	}
	var text string
	if rc == 0 {
		text = fmt.Sprintf("synced %d files to %s [exit 0]%s", len(files), remoteRoot, fmtRate(bytes, duration))
	} else {
		text = fmt.Sprintf("sync FAILED to %s [exit %d]; %d files were NOT transferred -- verify with `run \"ls -la %s\"` before assuming",
			remoteRoot, rc, len(files), remoteRoot)
	}
	if terr != nil {
		text += ": " + terr.Error()
	}
	structured := map[string]any{
		"files_count":      len(files),
		"deletes_count":    len(deletes),
		"remote_root":      remoteRoot,
		"exit_code":        rc,
		"duration_seconds": duration.Seconds(),
	}
	if rc == 0 {
		structured["bytes_transferred"] = bytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           rc != 0,
		StructuredContent: structured,
	}
}

func handleMCPSyncDeleteDryRun(args map[string]any, cfg *Config, profileOverride string) toolResult {
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	root, _ := args["root"].(string)
	if root == "" {
		root = findGitRoot(mustCwd())
	}
	if root == "" {
		return mcpTextErr("not in a git repo")
	}
	root, _ = filepath.Abs(root)
	o := &syncOpts{mode: "git", gitScope: "all", delete: true, dryRun: true}
	allExcludes := append([]string{}, prof.SyncExclude...)
	allExcludes = append(allExcludes, defaultSyncExcludes...)
	deletes, err := collectSyncDeletes(o, root, allExcludes)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	cwd := GetCwd(profName, prof)
	remoteRoot := cwd
	if v, _ := args["remote_root"].(string); v != "" {
		remoteRoot = resolveRemotePath(v, cwd)
	} else if prof.SyncRoot != "" {
		remoteRoot = resolveRemotePath(prof.SyncRoot, cwd)
	}
	text := fmt.Sprintf("would delete %d files from %s\n%s", len(deletes), remoteRoot, strings.Join(deletes, "\n"))
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"deletes_count": len(deletes),
			"remote_root":   remoteRoot,
			"dry_run":       true,
		},
	}
}

func handleMCPDetach(args map[string]any, cfg *Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return mcpTextErr("command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("detach", cmd, confirm); blocked != nil {
		return *blocked
	}
	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	rec, err := spawnDetached(profName, prof, cmd)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	return mcpDetachedResult(rec)
}

// handleMCPRunGroup fans `command` out across every profile in a
// named group and returns one structured result per member.
//
// Why separate from `run`: mixing the fan-out shape into the `run`
// tool's schema would force callers to handle both "single result"
// and "array of results" depending on whether `group` was set. Keeping
// it separate gives both tools narrow, predictable response shapes.
func handleMCPRunGroup(args map[string]any, cfg *Config, profileOverride string) toolResult {
	groupName, _ := args["group"].(string)
	if groupName == "" {
		return mcpTextErr("group is required")
	}
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return mcpTextErr("command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run_group", cmd, confirm); blocked != nil {
		return *blocked
	}
	// Long-blocking sync rejection applies fan-out too: a `sleep 60`
	// across 10 hosts wedges every connection in parallel for the same
	// 60s and still busts the MCP per-tool timeout.
	if why := runRejectSync(cmd); why != "" {
		return mcpTextErr(runRejectMessage(cmd, why))
	}
	results, err := runGroup(cfg, groupName, cmd)
	if err != nil {
		return mcpTextErr(err.Error())
	}

	// Build a compact text section per profile -- enough for the model
	// to read at a glance, with the full payload in structured content.
	var sb strings.Builder
	maxExit, failed := 0, 0
	for _, r := range results {
		fmt.Fprintf(&sb, "=== %s [exit %d, %.1fs]", r.Profile, r.ExitCode, r.Duration)
		if r.Error != "" {
			fmt.Fprintf(&sb, " ERROR: %s", r.Error)
		}
		sb.WriteString(" ===\n")
		if r.Stdout != "" {
			sb.WriteString(r.Stdout)
			if !strings.HasSuffix(r.Stdout, "\n") {
				sb.WriteByte('\n')
			}
		}
		if r.Stderr != "" {
			sb.WriteString("--- stderr ---\n")
			sb.WriteString(r.Stderr)
			if !strings.HasSuffix(r.Stderr, "\n") {
				sb.WriteByte('\n')
			}
		}
		if r.ExitCode != 0 || r.Error != "" {
			failed++
			if r.ExitCode > maxExit {
				maxExit = r.ExitCode
			} else if r.ExitCode < 0 && maxExit == 0 {
				maxExit = 255
			}
		}
	}
	fmt.Fprintf(&sb, "\n%d profile(s), %d succeeded, %d failed.\n", len(results), len(results)-failed, failed)

	return toolResult{
		Content: []toolContent{{Type: "text", Text: sb.String()}},
		IsError: failed > 0,
		StructuredContent: map[string]any{
			"group":     groupName,
			"results":   results,
			"succeeded": len(results) - failed,
			"failed":    failed,
		},
	}
}

func handleMCPListJobs(args map[string]any, cfg *Config, profileOverride string) toolResult {
	jobs := loadJobsFile().Jobs
	if profileOverride != "" {
		out := jobs[:0]
		for _, j := range jobs {
			if j.Profile == profileOverride {
				out = append(out, j)
			}
		}
		jobs = out
	}
	return mcpJSONResult(map[string]any{"jobs": jobs})
}

func handleMCPTailLog(args map[string]any, cfg *Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	lines := 200
	if v, ok := args["lines"].(float64); ok {
		lines = int(v)
	}
	jobs := loadJobsFile()
	j := findJob(jobs, jid)
	if j == nil {
		return mcpTextErr(fmt.Sprintf("no such job %q", jid))
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	res, _ := runRemoteCapture(prof, "", fmt.Sprintf("tail -n %d %s", lines, j.Log))
	text := res.Stdout
	if text == "" {
		text = res.Stderr
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           res.ExitCode != 0,
		StructuredContent: map[string]any{"job_id": j.ID, "exit_code": res.ExitCode},
	}
}

func handleMCPWaitJob(args map[string]any, cfg *Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	maxWait := mcpWaitJobDefaultSeconds
	if v, ok := args["max_wait_seconds"].(float64); ok && v > 0 {
		maxWait = int(v)
	}
	if maxWait > mcpWaitJobMaxSeconds {
		maxWait = mcpWaitJobMaxSeconds
	}
	tailLines := 50
	if v, ok := args["tail_lines"].(float64); ok && v > 0 {
		tailLines = int(v)
	}
	jobs := loadJobsFile()
	j := findJob(jobs, jid)
	if j == nil {
		return mcpTextErr(fmt.Sprintf("no such job %q", jid))
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	// One remote round-trip drives the whole wait loop. Bash spins
	// for up to maxWait seconds, checking each second for either
	// the .exit marker (job finished, capture exit code) or the
	// PID being gone without an .exit (got killed externally).
	// Either resolution prints `STATUS=...` on the first line plus
	// the log tail; if maxWait elapses the same shape is returned
	// with STATUS=running so the model can loop.
	exitFile := fmt.Sprintf("~/.srv-jobs/%s.exit", j.ID)
	script := fmt.Sprintf(`for i in $(seq 1 %d); do
  if [ -f %s ]; then
    code=$(cat %s)
    printf 'STATUS=completed EXIT=%%s\n' "$code"
    tail -n %d %s
    exit 0
  fi
  if ! kill -0 %d 2>/dev/null; then
    echo STATUS=killed
    tail -n %d %s
    exit 0
  fi
  sleep 1
done
echo STATUS=running
tail -n %d %s
`, maxWait, exitFile, exitFile, tailLines, j.Log, j.Pid, tailLines, j.Log, tailLines, j.Log)
	start := time.Now()
	res, _ := runRemoteCapture(prof, "", script)
	waited := time.Since(start).Seconds()

	lines := strings.SplitN(res.Stdout, "\n", 2)
	statusLine := ""
	body := ""
	if len(lines) > 0 {
		statusLine = lines[0]
	}
	if len(lines) > 1 {
		body = lines[1]
	}
	status := "unknown"
	exitCode := -1
	if strings.HasPrefix(statusLine, "STATUS=completed") {
		status = "completed"
		if i := strings.Index(statusLine, "EXIT="); i >= 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(statusLine[i+5:])); err == nil {
				exitCode = n
			}
		}
		// Job finished -- prune from local registry so list_jobs
		// doesn't keep advertising it. The .log / .exit files on
		// the remote stay; users can still tail historical logs
		// manually if they want.
		out := jobs.Jobs[:0]
		for _, x := range jobs.Jobs {
			if x.ID != j.ID {
				out = append(out, x)
			}
		}
		jobs.Jobs = out
		_ = saveJobsFile(jobs)
	} else if strings.HasPrefix(statusLine, "STATUS=killed") {
		status = "killed"
	} else if strings.HasPrefix(statusLine, "STATUS=running") {
		status = "running"
	}

	var hint string
	switch status {
	case "completed":
		hint = fmt.Sprintf("[%s exit=%d after %.1fs]", status, exitCode, waited)
	case "running":
		hint = fmt.Sprintf("[%s after %.1fs -- call wait_job again to keep waiting, or kill_job to stop]", status, waited)
	default:
		hint = fmt.Sprintf("[%s after %.1fs]", status, waited)
	}
	text := hint
	if body != "" {
		text += "\n" + body
	}
	structured := map[string]any{
		"job_id":         j.ID,
		"status":         status,
		"waited_seconds": waited,
	}
	if status == "completed" {
		structured["exit_code"] = exitCode
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           status == "killed" || (status == "completed" && exitCode != 0),
		StructuredContent: structured,
	}
}

func handleMCPListDir(args map[string]any, cfg *Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	dirsOnly, _ := args["dirs_only"].(bool)
	limit := 500
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	entries, err := listRemoteEntries(path, cfg, profileOverride)
	if err != nil {
		return mcpTextErr(err.Error())
	}
	if dirsOnly {
		kept := entries[:0]
		for _, e := range entries {
			if strings.HasSuffix(e, "/") {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	truncated := 0
	if len(entries) > limit {
		truncated = len(entries) - limit
		entries = entries[:limit]
	}
	text := strings.Join(entries, "\n")
	if text != "" {
		text += "\n"
	}
	structured := map[string]any{
		"entries": entries,
		"count":   len(entries),
	}
	if truncated > 0 {
		structured["truncated_count"] = truncated
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}

func handleMCPKillJob(args map[string]any, cfg *Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	sig, _ := args["signal"].(string)
	if sig == "" {
		sig = "TERM"
	}
	jobs := loadJobsFile()
	j := findJob(jobs, jid)
	if j == nil {
		return mcpTextErr(fmt.Sprintf("no such job %q", jid))
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return mcpTextErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid'", sig, j.Pid)
	res, _ := runRemoteCapture(prof, "", cmd)
	out := jobs.Jobs[:0]
	for _, x := range jobs.Jobs {
		if x.ID != j.ID {
			out = append(out, x)
		}
	}
	jobs.Jobs = out
	_ = saveJobsFile(jobs)
	text := strings.TrimSpace(res.Stdout)
	if text == "" {
		text = strings.TrimSpace(res.Stderr)
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           res.ExitCode != 0,
		StructuredContent: map[string]any{"job_id": j.ID, "signal": sig, "exit_code": res.ExitCode},
	}
}

// -----------------------------------------------------------------------------
// Registry. Each entry pairs a toolDef (advertised to the client via
// tools/list) with its handler (called from tools/call). Order here is
// only a presentation choice; mcpToolMap below makes lookup O(1).
// -----------------------------------------------------------------------------

var mcpTools = []mcpTool{
	{
		def: toolDef{
			Name:        "run",
			Description: "Run a remote shell command. Synchronous by default (blocks until completion).\n\nREJECTED in synchronous mode (use background=true instead):\n  - `sleep N` where N > 5\n  - `tail -f`, `watch`, `journalctl -f` and similar never-terminating patterns\n\nREJECTED as unbounded-output (token economy -- add a slicer):\n  - `cat <file>`           -> use `head -n N <file>` or `tail -n N <file>` or the `tail` MCP tool\n  - `dmesg`                -> pipe into `tail -n N` or `grep PATTERN`\n  - `journalctl` w/o flags -> use the `journal` MCP tool or add -u/--since/-p/-g/-n\n  - `find /` w/o flags     -> add -maxdepth N / -name PATTERN / -type / etc.\nDownstream limiters (`| head`, `| tail`, `| grep`, `| wc`, etc.) satisfy the gate.\n\nFor anything expected to take more than ~10s (builds, installs, tests, big greps, sleep+poll loops), set background=true. The command starts as a detached job and returns a job_id immediately; then poll with wait_job in short (<=15s) chunks. Synchronous mode is bound by the client's per-tool timeout (Claude Code default 60s).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    strSchema("Remote shell command."),
					"profile":    strSchema(""),
					"background": boolSchema(false, "Start as a detached job and return immediately. Required for long commands and for any sleep/wait/follow pattern."),
					"confirm":    boolSchema(false, "Required when guard is on AND command hits a high-risk pattern (rm -rf, dd, mkfs, drop ...)."),
				},
				"required": []string{"command"},
			},
		},
		handler: handleMCPRun,
	},
	{
		def: toolDef{
			Name:        "journal",
			Description: "Read or follow systemd journal on the remote. Mirrors journalctl's flag shape: `unit` (-u), `since`, `priority` (-p), `lines` (-n), `grep` (-g, server-side). Pass `follow_seconds` > 0 to stream new lines via `notifications/progress` for that many seconds (cap 60); leave 0 for a one-shot read. Use this in place of `run \"journalctl ...\"` so the bounded-follow case has a real tool surface instead of getting rejected as a long-blocking pattern.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES at least one of unit / since / priority / grep -- progress notifications during follow are unbounded by the result-text cap.\n  - `lines` is clamped to 2000.\n  - follow_seconds capped at 60s.\n\nSibling tools (pick by source):\n  - `tail`      -> any remote file by path\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"unit":           strSchema("Service unit name (passed to -u)."),
					"since":          strSchema("Relative or absolute time (e.g. \"10 min ago\")."),
					"priority":       strSchema("Priority filter (-p), e.g. err / warning / info."),
					"lines":          intSchema(100, "Number of recent lines to fetch (-n)."),
					"grep":           strSchema("Server-side regex filter (-g)."),
					"follow_seconds": intSchema(0, "Follow for N seconds via progress notifications; 0 = one-shot. Capped at 60."),
					"profile":        strSchema(""),
				},
			},
		},
		handler: handleMCPJournal,
	},
	{
		def: toolDef{
			Name:        "tail",
			Description: "Read the last N lines of a remote file. With follow_seconds > 0, also streams new lines via `notifications/progress` for that duration. Use the one-shot form for log spot-checks; use the follow form when you actually need to watch a log change mid-deploy.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES a `grep` regex. Even short follows can flood progress notifications; the 64 KiB final-result cap does NOT cap the progress stream.\n  - `lines` is clamped to 1000.\n  - follow_seconds capped at 60s.\n\nFor one-shot reads (default), no grep is required -- the `lines` cap is the bound.\n\nSibling tools (pick by source):\n  - `journal`   -> systemd unit logs (use this for any service log on a systemd host; never `tail /var/log/journal/...`)\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":           strSchema("Remote file to follow."),
					"lines":          intSchema(50, "Lines to fetch (initial backfill, or full slice when not following). Clamped at 1000."),
					"follow_seconds": intSchema(0, "Follow for N seconds via progress notifications. 0 (default) = one-shot. ANY non-zero value REQUIRES a `grep` filter -- progress notifications are unbounded by the result-text cap. Capped at 60s."),
					"grep":           strSchema("Regex filter applied per line. Mandatory whenever follow_seconds > 0."),
					"profile":        strSchema(""),
				},
				"required": []string{"path"},
			},
		},
		handler: handleMCPTail,
	},
	{
		def: toolDef{
			Name:        "run_stream",
			Description: "Streaming variant of `run`. Output is pushed to the client as `notifications/progress` messages while the command runs, then the final tool result delivers the full captured output. Use this for medium-length commands (~20-90s builds, tests, deploys) where synchronous `run` would risk the per-tool timeout: progress keeps the call alive, the model sees partial output mid-flight, and the final result still arrives as the authoritative payload.\n\nThe client must pass `_meta.progressToken` on tools/call for the notifications to be delivered -- without it, this falls back to a synchronous shape identical to `run`.\n\nSame token-economy gate as `run`: `cat <file>` / bare `dmesg` / unfiltered `journalctl` / `find /` without flags are rejected. Streaming makes unbounded output WORSE (progress notifications add token cost on top of the final result), so the gate applies more aggressively here too.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": strSchema("Remote shell command."),
					"profile": strSchema(""),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"command"},
			},
		},
		handler: handleMCPRunStream,
	},
	{
		def: toolDef{
			Name:        "cd",
			Description: "Set remote cwd.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    strSchema("Path."),
					"profile": strSchema(""),
				},
				"required": []string{"path"},
			},
		},
		handler: handleMCPCd,
	},
	{
		def: toolDef{
			Name:        "pwd",
			Description: "Get remote cwd.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleMCPPwd,
	},
	{
		def: toolDef{
			Name:        "use",
			Description: "Pin or clear profile.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"profile": strSchema(""),
					"clear":   boolSchema(false, ""),
				},
			},
		},
		handler: handleMCPUse,
	},
	{
		def: toolDef{
			Name:        "status",
			Description: "Show active profile.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleMCPStatus,
	},
	{
		def: toolDef{
			Name:        "list_profiles",
			Description: "List profiles.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: handleMCPListProfiles,
	},
	{
		def: toolDef{
			Name:        "check",
			Description: "Probe SSH connectivity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleMCPCheck,
	},
	{
		def: toolDef{
			Name:        "doctor",
			Description: "Run local diagnostics.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleMCPDoctor,
	},
	{
		def: toolDef{
			Name:        "daemon_status",
			Description: "Show daemon status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: handleMCPDaemonStatus,
	},
	{
		def: toolDef{
			Name:        "env",
			Description: "Manage remote env.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": []string{"list", "set", "unset", "clear"}, "default": "list"},
					"key":     strSchema("Env var name."),
					"value":   strSchema("Env var value."),
					"profile": strSchema(""),
				},
			},
		},
		handler: handleMCPEnv,
	},
	{
		def: toolDef{
			Name:        "diff",
			Description: "Diff local vs remote file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema("Local file."),
					"remote":  strSchema("Remote file."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: handleMCPDiff,
	},
	{
		def: toolDef{
			Name:        "push",
			Description: "Upload file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema(""),
					"remote":  strSchema("Remote path."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: handleMCPPush,
	},
	{
		def: toolDef{
			Name:        "pull",
			Description: "Download file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote":    strSchema(""),
					"local":     strSchema("Local path."),
					"recursive": boolSchema(false, ""),
					"profile":   strSchema(""),
				},
				"required": []string{"remote"},
			},
		},
		handler: handleMCPPull,
	},
	{
		def: toolDef{
			Name:        "sync",
			Description: "Sync local changes to remote.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote_root":  strSchema("Remote root."),
					"mode":         map[string]any{"type": "string", "enum": []string{"git", "mtime", "glob", "list"}},
					"git_scope":    map[string]any{"type": "string", "enum": []string{"all", "staged", "modified", "untracked"}, "default": "all"},
					"since":        strSchema("Duration, e.g. 2h."),
					"include":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"exclude":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"root":         strSchema(""),
					"dry_run":      boolSchema(false, ""),
					"delete":       boolSchema(false, ""),
					"yes":          boolSchema(false, ""),
					"delete_limit": intSchema(20, "Max deletes without yes=true."),
					"profile":      strSchema(""),
					"confirm":      boolSchema(false, "Required when guard is on AND delete=true (non-dry-run)."),
				},
			},
		},
		handler: handleMCPSync,
	},
	{
		def: toolDef{
			Name:        "sync_delete_dry_run",
			Description: "Preview sync deletes.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"root": strSchema("Local root."), "remote_root": strSchema("Remote root."), "profile": strSchema("")},
			},
		},
		handler: handleMCPSyncDeleteDryRun,
	},
	{
		def: toolDef{
			Name:        "run_group",
			Description: "Run the same remote command across every profile in a named group, in parallel. Returns one result per member with exit code, stdout/stderr, and duration. Use this when you'd otherwise have to loop `run` over N hosts (deploys, restarts, status checks). Synchronous: subject to the same 60s MCP per-tool cap as `run`, so keep the command short or run it via `detach` per-profile and then poll.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"group":   strSchema("Group name as defined in config.groups."),
					"command": strSchema("Remote shell command to run on every member."),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"group", "command"},
			},
		},
		handler: handleMCPRunGroup,
	},
	{
		def: toolDef{
			Name:        "detach",
			Description: "Start a remote command in the background and return its job_id immediately (sub-second). Pair with `wait_job` to block on completion in bounded chunks -- the recommended pattern for any command expected to take more than ~30s. The wrapper writes the user command's exit code to ~/.srv-jobs/<id>.exit when it finishes, which `wait_job` polls without keeping an SSH session open the whole time.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": strSchema(""),
					"profile": strSchema(""),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"command"},
			},
		},
		handler: handleMCPDetach,
	},
	{
		def: toolDef{
			Name:        "list_jobs",
			Description: "List detached jobs.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleMCPListJobs,
	},
	{
		def: toolDef{
			Name:        "tail_log",
			Description: "Read the last N lines of a detached job's log file (by job_id). Resolves the id to ~/.srv-jobs/<id>.log on the remote and runs `tail -n LINES` there. One-shot only -- use `wait_job` for the polling pattern that pairs with `detach` / `run background=true`.\n\nSibling tools (pick by source):\n  - `tail`     -> any remote file by path (with optional follow + grep)\n  - `journal`  -> systemd unit logs",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    strSchema(""),
					"lines": intSchema(200, ""),
				},
				"required": []string{"id"},
			},
		},
		handler: handleMCPTailLog,
	},
	{
		def: toolDef{
			Name:        "wait_job",
			Description: "Poll a detached job for completion, returning exit code + log tail when done. Designed to pair with `detach` or `run background=true`: long commands run in the background, and the model loops wait_job until status=completed. Defaults to short 8s polls and caps each call at 15s so Claude Code stays responsive. status=running means \"call wait_job again\"; status=completed means it's done and the local job record has been cleaned up.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":               strSchema("Job id from detach."),
					"max_wait_seconds": intSchema(mcpWaitJobDefaultSeconds, "Upper bound on this call's blocking time. Capped at 15 to keep the MCP UI responsive."),
					"tail_lines":       intSchema(50, "Lines of log to include in the response."),
				},
				"required": []string{"id"},
			},
		},
		handler: handleMCPWaitJob,
	},
	{
		def: toolDef{
			Name:        "list_dir",
			Description: "List remote directory entries (subset of `ls -1Ap`). Use this instead of `run \"ls ...\"` for path discovery -- response is structured, ANSI-clean, and hits the warm daemon cache (sub-100ms on repeat). Pass an empty path for the active cwd. Dirs carry trailing '/'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      strSchema("Remote path prefix. Empty = current cwd. Trailing '/' = list that directory; no trailing '/' = match entries whose name starts with the basename. E.g., '/etc/' lists /etc; '/etc/host' returns host*, hostname, hosts, hosts.allow."),
					"dirs_only": boolSchema(false, "Filter to directories only (entries ending in '/')."),
					"limit":     intSchema(500, "Maximum entries returned. Anything beyond gets dropped; truncated_count surfaces the cut so you know to query a deeper prefix."),
					"profile":   strSchema(""),
				},
			},
		},
		handler: handleMCPListDir,
	},
	{
		def: toolDef{
			Name:        "kill_job",
			Description: "Signal detached job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     strSchema(""),
					"signal": map[string]any{"type": "string", "default": "TERM"},
				},
				"required": []string{"id"},
			},
		},
		handler: handleMCPKillJob,
	},
}

// mcpToolMap is built once from the registry so dispatch is O(1).
var mcpToolMap map[string]*mcpTool

func init() {
	mcpToolMap = make(map[string]*mcpTool, len(mcpTools))
	for i := range mcpTools {
		t := &mcpTools[i]
		mcpToolMap[t.def.Name] = t
	}
}

// mcpToolDefs returns the slice of toolDef advertised on tools/list.
// Derived from the registry so the def-list and the dispatcher cannot
// drift -- both come from the same source.
func mcpToolDefs() []toolDef {
	defs := make([]toolDef, 0, len(mcpTools))
	for i := range mcpTools {
		defs = append(defs, mcpTools[i].def)
	}
	return defs
}

// mcpHandleTool dispatches a tools/call request through the registry.
// Unknown names return a textual error -- spec doesn't require a more
// structured "tool not found" form for that case.
func mcpHandleTool(name string, args map[string]any, cfg *Config) toolResult {
	profileOverride, _ := args["profile"].(string)
	if t, ok := mcpToolMap[name]; ok {
		return t.handler(args, cfg, profileOverride)
	}
	return mcpTextErr(fmt.Sprintf("unknown tool %q", name))
}

// buildMCPRunText assembles the textual payload returned by the `run`
// tool, capping the combined stdout+stderr at mcpRunTextMax. Returns
// (text, truncatedBytes); truncatedBytes is 0 when the output fit.
func buildMCPRunText(res *RunCaptureResult, cwd string) (string, int) {
	text := res.Stdout
	if res.Stderr != "" {
		if text != "" {
			text += "\n--- stderr ---\n"
		}
		text += res.Stderr
	}
	truncated := 0
	if len(text) > mcpRunTextMax {
		truncated = len(text) - mcpRunTextMax
		text = text[:mcpRunTextMax] + fmt.Sprintf(
			"\n\n... [%d bytes truncated; pipe through head/tail/grep on the remote to slice the output] ...",
			truncated,
		)
	}
	text += fmt.Sprintf("\n[exit %d cwd %s]", res.ExitCode, cwd)
	return text, truncated
}
