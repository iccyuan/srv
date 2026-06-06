package mcp

import (
	"fmt"
	"regexp"
	"srv/internal/config"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Pre-execution gates: high-risk command guard, sync-rejection
// patterns (sleep N, tail -f, nohup ..., etc), token-economy filters
// for unbounded sources (cat /file, dmesg, journalctl, find /), and
// the streaming-must-have-a-filter rule. All called from handlers;
// none touch SSH directly.

// riskyPattern flags a remote command as destructive enough to
// require confirm=true when the session guard is on. Each pattern is
// a regex matched against the full command string; `name` is the
// human-readable label surfaced to the model in the block reason.
//
// Because the guard now defaults to ON (see config.GuardActive),
// this list is kept narrow: irreversible destruction of data / a
// filesystem / a raw disk, PLUS the host power-control verbs
// (shutdown / reboot / halt / poweroff) -- an unintended reboot or
// halt of a prod box is high-impact enough to be worth a confirm
// even though the data survives. False-positives are recoverable
// (re-issue with confirm=true). Pure destruction *precursors* that
// don't themselves lose anything (chattr -i just clears the
// immutable bit; the rm still has to come after and would trip its
// own rule) stay OUT of the built-in set to keep default-on
// unobtrusive -- add them with `srv guard add <name> <regex>` if
// your environment wants them gated too.
//
// Coverage is cross-platform and not SQL-only: relational DROP/
// TRUNCATE incl. Cassandra KEYSPACE, MongoDB dropDatabase()/.drop(),
// Redis FLUSHALL/FLUSHDB, PostgreSQL `dropdb`; and macOS-native disk
// destroyers (newfs_*, diskutil erase*/partitionDisk, /dev/rdisk*)
// since macOS has no mkfs and uses /dev/rdiskN for raw access.
//
// Word boundaries (\b) keep us from matching `farm -rf` etc. Quoted
// content (echo "rm -rf /") is filtered out separately by riskyMatch
// via isInsideQuotes, since \b alone can't tell a real command
// position from a string-literal occurrence. The DB-client rules
// (`sql -e drop`, `mongo --eval drop`) work around this for the
// common `mysql -e "DROP DATABASE x"` shape by anchoring the match
// on the unquoted client binary instead of the quoted verb -- see
// their comment below. A verb in a quoted arg of a NON-DB command
// is still not gated (that's the intended echo "rm -rf" exemption).
type riskyPattern struct {
	name string
	re   *regexp.Regexp
}

var defaultRiskyPatterns = []riskyPattern{
	{"rm -rf", regexp.MustCompile(`(?i)\brm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF]\b|-[a-zA-Z]*[fF][a-zA-Z]*[rR]\b|--recursive\s+--force|--force\s+--recursive|-[rRfF]\s+--(?:recursive|force)\b|--(?:recursive|force)\s+-[rRfF]\b)`)},
	{"dd of=/...", regexp.MustCompile(`(?i)\bdd\s+(?:[^|;&\n]*\s)?(?:of=|if=/dev/(?:zero|random|urandom)\b)`)},
	{"mkfs", regexp.MustCompile(`(?i)\bmkfs(?:\.[a-z0-9]+)?\b`)},
	{"newfs", regexp.MustCompile(`(?i)\bnewfs_[a-z0-9]+\b`)},
	{"shutdown", regexp.MustCompile(`(?i)\bshutdown\b`)},
	{"reboot", regexp.MustCompile(`(?i)\breboot\b`)},
	{"halt", regexp.MustCompile(`(?i)\bhalt\b`)},
	{"poweroff", regexp.MustCompile(`(?i)\bpoweroff\b`)},
	{"drop database", regexp.MustCompile(`(?i)\bdrop\s+(?:database|table|schema|keyspace)\b`)},
	{"truncate table", regexp.MustCompile(`(?i)\btruncate\s+(?:table\b|-)`)},
	// NoSQL "wipe it all" verbs. dropDatabase() is mongo-specific
	// enough to match anywhere (covers db.dropDatabase() and
	// getSiblingDB(...).dropDatabase()); collection .drop() is
	// constrained to a literal db.<name>.drop() with empty parens so
	// pandas `df.drop(columns=...)` / lodash `_.drop(2)` don't trip.
	// flushall/flushdb is Redis' destructive flush. These bare forms
	// only fire at a command position (`db.dropDatabase()` typed into
	// a REPL, `redis-cli FLUSHALL` as bare args).
	{"mongo drop", regexp.MustCompile(`(?i)\bdropDatabase\s*\(|\bdb\.[a-z_]\w*\.drop\s*\(\s*\)`)},
	{"redis flush", regexp.MustCompile(`(?i)\bflush(?:all|db)\b`)},
	{"dropdb", regexp.MustCompile(`(?i)\bdropdb\b`)},
	// Quoted-payload DB destroyers. The bare rules above can't see a
	// verb hidden in a client's quoted -e/--eval/-c arg
	// (`mysql -e "DROP DATABASE x"`), because codePositions marks
	// quoted bytes literal. These instead anchor the match on the
	// *client binary* itself -- which IS at a code position -- and
	// reach forward into the quoted arg. Net effect:
	//   - `mysql -e "DROP DATABASE x"`            -> caught (match
	//     starts at unquoted `mysql`)
	//   - `echo "mysql -e \"DROP DATABASE x\""`   -> NOT caught: the
	//     `mysql` is itself inside echo's quotes, so codePositions
	//     suppresses it (no false-positive on echoed examples)
	// Both gaps use [^|;&\n] so the verb must live in the SAME simple
	// command as the client -- a later `&& echo "...drop database..."`
	// segment can't trip it. Bounded quantifiers keep RE2 linear.
	{"sql -e drop", regexp.MustCompile(`(?i)\b(?:mysql|mariadb|psql|cqlsh|clickhouse-client)\b[^|;&\n]{0,120}?(?:-e|--execute|-c|--command|-q|--query)['"=\s][^|;&\n]{0,200}?\b(?:drop\s+(?:database|table|schema|keyspace)|truncate\s+table)\b`)},
	{"mongo --eval drop", regexp.MustCompile(`(?i)\bmongo(?:sh)?\b[^|;&\n]{0,120}?--eval['"=\s][^|;&\n]{0,200}?(?:dropDatabase\s*\(|\.drop\s*\(\s*\)|\bdrop\s+(?:database|collection)\b)`)},
	{":>/", regexp.MustCompile(`:\s*>\s*/`)},
	// r?disk so macOS raw-disk nodes (/dev/rdisk0) match too, not
	// just /dev/disk0. dd of=/dev/rdisk0 is already caught by the dd
	// rule; this covers a bare `> /dev/rdisk0` redirect.
	{"> /dev/disk", regexp.MustCompile(`>\s*/dev/(?:sd|nvme|r?disk|hd)`)},
	// macOS has no mkfs; diskutil erase*/partitionDisk/zeroDisk/
	// secureErase and `apfs delete*` are the real disk destroyers.
	// diskutil list/info/mount/unmount are recoverable -> not matched.
	{"diskutil erase", regexp.MustCompile(`(?i)\bdiskutil\s+(?:erase\w*|reformat|partitiondisk|zerodisk|secureerase|apfs\s+(?:delete|erase)\w*)\b`)},
}

// activePatterns is the effective rule set after merging defaults with
// the user's GuardConfig. The result is cached behind a content-derived
// signature so edits to config.json still take effect without a restart
// (next call sees a different sig, rebuilds) while a stable config
// avoids paying regexp.Compile on every MCP tool invocation.
func activePatterns(cfg *config.Config) ([]riskyPattern, []*regexp.Regexp) {
	if cfg == nil || cfg.Guard == nil {
		return defaultRiskyPatterns, nil
	}
	gc := cfg.Guard
	sig := guardSig(gc)
	if e := guardCache.Load(); e != nil && e.sig == sig {
		return e.rules, e.allow
	}
	out := []riskyPattern{}
	if !gc.DisableDefaults {
		out = append(out, defaultRiskyPatterns...)
	}
	for _, r := range gc.Rules {
		if r.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.Pattern
		}
		out = append(out, riskyPattern{name: name, re: re})
	}
	var allow []*regexp.Regexp
	for _, p := range gc.Allow {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		allow = append(allow, re)
	}
	guardCache.Store(&guardCacheEntry{sig: sig, rules: out, allow: allow})
	return out, allow
}

// guardCacheEntry holds a compiled rule set keyed by the signature of
// the GuardConfig that produced it.
type guardCacheEntry struct {
	sig   string
	rules []riskyPattern
	allow []*regexp.Regexp
}

// guardCache is a single-slot lock-free cache. In practice users almost
// never edit guard rules, so this stays warm for the lifetime of the
// MCP server process. Races between two misses are benign (one of the
// two compilations wins; both produce identical output).
var guardCache atomic.Pointer[guardCacheEntry]

// guardSig builds a fingerprint of the inputs activePatterns reads.
// NUL is a safe separator because regex / rule-name strings are
// already validated UTF-8 with no embedded NULs. Cost is one
// allocation proportional to total rule-string length -- orders of
// magnitude cheaper than even one regexp.Compile call.
func guardSig(gc *config.GuardConfig) string {
	var b strings.Builder
	approx := 4 + len(gc.Rules)*16 + len(gc.Allow)*16
	for _, r := range gc.Rules {
		approx += len(r.Name) + len(r.Pattern)
	}
	for _, p := range gc.Allow {
		approx += len(p)
	}
	b.Grow(approx)
	if gc.DisableDefaults {
		b.WriteByte('D')
	}
	b.WriteByte('|')
	for _, r := range gc.Rules {
		b.WriteString(r.Name)
		b.WriteByte(0)
		b.WriteString(r.Pattern)
		b.WriteByte(0)
	}
	b.WriteByte('|')
	for _, p := range gc.Allow {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return b.String()
}

// guardConfigForTests overrides the active config so unit tests in
// this package don't have to write to ~/.srv/config.json. Set via
// SetGuardConfigForTests; reset to nil when done.
var (
	guardConfigForTestsMu sync.RWMutex
	guardConfigForTests   *config.Config
)

// SetGuardConfigForTests pins a config to use as the source of guard
// rules. Test-only seam; production code path reads via config.Load
// inside guardCheckRisky.
func SetGuardConfigForTests(c *config.Config) {
	guardConfigForTestsMu.Lock()
	guardConfigForTests = c
	guardConfigForTestsMu.Unlock()
}

func loadGuardConfig() *config.Config {
	guardConfigForTestsMu.RLock()
	tc := guardConfigForTests
	guardConfigForTestsMu.RUnlock()
	if tc != nil {
		return tc
	}
	c, err := config.Load()
	if err != nil || c == nil {
		return &config.Config{}
	}
	return c
}

// riskyMatch reports the name of the first risky pattern present in
// `command`, or "" if none. A match counts only when its first byte
// is in "code position" -- i.e. the shell would execute that byte
// as a command word, not consume it as a literal string fragment.
// See codePositions for the exact classification.
//
// The fancy classification is mostly to catch the false-negative
// shape `echo "$(rm -rf foo)"`: a naive "is this inside quotes?"
// check would say yes and let it through, but real shells execute
// $(...) contents regardless of surrounding double quotes.
func riskyMatch(command string) string {
	return riskyMatchWithRules(command, defaultRiskyPatterns, nil)
}

// riskyMatchWithRules is the inner form taking explicit rule set +
// allow list so the CLI's `srv guard test "..."` can preview matches
// against the user's *configured* set rather than the defaults.
//
// If any allow regex matches the command, the deny match is suppressed
// and "" returned.
func riskyMatchWithRules(command string, rules []riskyPattern, allow []*regexp.Regexp) string {
	if command == "" {
		return ""
	}
	code := codePositions(command)
	hit := ""
	for _, p := range rules {
		for _, loc := range p.re.FindAllStringIndex(command, -1) {
			if loc[0] < len(code) && code[loc[0]] {
				hit = p.name
				break
			}
		}
		if hit != "" {
			break
		}
	}
	if hit == "" {
		return ""
	}
	for _, a := range allow {
		if a.MatchString(command) {
			return ""
		}
	}
	return hit
}

// RiskyMatchPublic exposes riskyMatchWithRules to the guard CLI so
// `srv guard test "..."` reuses
// the same engine the live MCP server uses, including the allow-list
// short-circuit. Always reads the current config.
func RiskyMatchPublic(command string) string {
	cfg := loadGuardConfig()
	rules, allow := activePatterns(cfg)
	return riskyMatchWithRules(command, rules, allow)
}

// codePositions returns a per-byte classifier: out[i] is true when
// shell would execute byte i as command-position content, false
// when it's literal-string content.
//
// Classification rules (best-effort POSIX subset):
//
//	'...'        single-quoted, every byte literal
//	"..."        double-quoted, bytes literal EXCEPT $(...) / `...`
//	             nested inside (those expand)
//	$(...)       command substitution; contents are code, regardless
//	             of any surrounding quote
//	`...`        backtick command substitution; same as $()
//	\<char>      inside double quotes, escapes treat next byte as
//	             literal (skipped past in classifier state)
//
// Heredocs, $'...' (ANSI-C quoting), and $((...)) arithmetic
// expansion are not modeled. We err on the side of "code position"
// for unrecognized shapes so risky patterns inside exotic syntax
// trip the guard rather than slipping through.
func codePositions(s string) []bool {
	out := make([]bool, len(s))
	inSingle, inDouble, escape := false, false, false
	// cmdSubDepth counts unclosed $(...) and `...` regions. >0 forces
	// "code position" regardless of surrounding quotes.
	cmdSubDepth := 0
	for i := 0; i < len(s); i++ {
		// Decide what THIS byte is, then advance state for next iter.
		inLiteral := false
		if cmdSubDepth == 0 {
			inLiteral = inSingle || inDouble
		}
		out[i] = !inLiteral

		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inDouble && cmdSubDepth == 0 {
			escape = true
			continue
		}

		// $( opens a command-substitution scope. Allowed inside
		// double quotes (real shells expand it there) but not
		// inside single quotes (those are literal).
		if c == '$' && i+1 < len(s) && s[i+1] == '(' && !inSingle {
			cmdSubDepth++
			i++ // step past '(' so the next loop iteration sees what
			// followed it.
			continue
		}
		if cmdSubDepth > 0 && c == ')' {
			cmdSubDepth--
			continue
		}

		// Backtick toggles: open or close a command-substitution
		// scope. Single quotes still suppress (literal everywhere).
		if c == '`' && !inSingle {
			if cmdSubDepth > 0 {
				cmdSubDepth--
			} else {
				cmdSubDepth++
			}
			continue
		}

		// Quote toggles only meaningful at the top level (cmd-sub
		// scopes have their own quote bookkeeping in real shells;
		// we lump them as "code" and accept the imprecision).
		if cmdSubDepth == 0 {
			if c == '"' && !inSingle {
				inDouble = !inDouble
			} else if c == '\'' && !inDouble {
				inSingle = !inSingle
			}
		}
	}
	return out
}

// isInsideQuotes is the legacy classifier kept only for the unit
// test that locks down its byte-position semantics; production code
// goes through codePositions.
func isInsideQuotes(s string, pos int) bool {
	if pos < 0 || pos >= len(s) {
		return false
	}
	return !codePositions(s)[pos]
}

// guardCheckRisky returns a blocking toolResult when the session
// guard is on AND `cmd` matches a high-risk pattern AND the caller
// didn't pass confirm=true. Returns nil to mean "allowed". Used by
// `run`, `detach`, `run_group`, and any other tool
// that ferries a raw shell command to the remote.
func guardCheckRisky(tool, cmd string, confirm bool) *toolResult {
	if confirm {
		return nil
	}
	cfg := loadGuardConfig()
	// cfg.GuardActive() (not session.GuardOn()) so `srv guard off
	// --global` disables the gate for THIS process -- the MCP server's
	// ppid-derived session never matches the user's shell, so the
	// per-session form alone could never turn it off here.
	if !cfg.GuardActive() {
		return nil
	}
	rules, allow := activePatterns(cfg)
	pat := riskyMatchWithRules(cmd, rules, allow)
	if pat == "" {
		return nil
	}
	reason := fmt.Sprintf("command contains a high-risk pattern %q", pat)
	// Put a human in the loop before hard-denying: if the client can be
	// asked (advertised the elicitation capability), surface an
	// Allow/Deny prompt and honour the human's answer. asked=false means
	// the client couldn't be reached -- fall back to the original
	// hard-deny so a risky command is never let through just because the
	// round-trip was unavailable.
	allowByHuman, asked := elicitConfirm(fmt.Sprintf(
		"srv guard: %s wants to run a high-risk command.\n\n  %s\n\nReason: %s\n\nAllow it to run?",
		tool, cmd, reason))
	if asked {
		if allowByHuman {
			return nil
		}
		r := guardDenied(tool, reason)
		return &r
	}
	r := guardBlocked(tool, reason)
	return &r
}

// longWaitSleep matches `sleep N` / `sleep N.X` (N>5 checked by the
// caller) ONLY when `sleep` sits at a command position -- start of
// line, after a shell separator (`; & | ( ) { }` / newline), or after
// a `do`/`then` keyword. The old `\bsleep\s+\d` matched the substring
// anywhere, so `pgrep -af 'sleep 30'`, `grep "sleep 60" log` and
// `echo sleep 90` were wrongly rejected as blocking. Trade-off: a
// genuinely-blocking `sleep` buried in quotes with no loop keyword
// (`bash -c 'sleep 30'`) is no longer caught -- the 60s MCP per-tool
// timeout stays the backstop, and under-rejecting beats blocking
// legitimate fast inspection commands.
var longWaitSleep = regexp.MustCompile(`(?:^|[;&|(){}\n]|\bdo\b|\bthen\b)\s*sleep\s+(\d+(?:\.\d+)?)\b`)

// foreverPatterns are commands that don't terminate on their own.
// `tail -f`, `watch`, and `journalctl -f` are the canonical sins.
//
// `[^;&|\n]*?` allows arbitrary intervening flags (`journalctl -u
// nginx -f`, `tail -n 100 -f log`) while still stopping at shell
// separators -- so `echo hi; journalctl -u svc -f` triggers, but a
// quoted argument cannot hop the separator boundary. Note: case-
// sensitive `-f` -- we do not flag `tail -F` (retry-on-truncate),
// which has legitimate non-blocking uses in scripted contexts.
var foreverPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\btail\b[^;&|\n]*?\s(?:-f|--follow)\b`),
	regexp.MustCompile(`\bwatch\s+`),
	regexp.MustCompile(`\bjournalctl\b[^;&|\n]*?\s(?:-f|--follow)\b`),
}

// reSyncNohup matches `nohup ...` at a command position -- start of
// line, after a shell separator (`; & | ( ) { }` / newline), or after
// a `do`/`then` keyword. Same anchor shape as longWaitSleep so token
// references (`cat /tmp/nohup.out`, `grep nohup`, `echo nohup`) don't
// false-positive.
//
// Rationale: `nohup` signals "I want to detach this from the session"
// -- almost always paired with a long-running service. Running it via
// synchronous `run` still captures the service's startup banner /
// config dump / listen-port lines into MCP tokens for no benefit
// (4-5 KiB per service start is typical; the standard background path
// returns ~150 B regardless of how loud the service is). Route the
// model to `background: true` + wait_job + tail_log, which gives the
// same detach semantics with bounded token spend.
var reSyncNohup = regexp.MustCompile(`(?:^|[;&|(){}\n]|\bdo\b|\bthen\b)\s*nohup\s+\S`)

// rejectSync inspects a command planned for synchronous execution
// and returns a non-empty hint if it would block the MCP turn for
// too long. AI clients reach for sleep+poll loops by reflex, but
// those tie up the MCP per-tool timeout and produce the "tools no
// longer available" red dot. We catch the patterns here and route
// the model toward background=true / wait_job. Empty return =
// command is fine to run sync.
func rejectSync(cmd string) string {
	// Match against a cleaned view: heredoc bodies removed and quoted
	// content blanked. This stops a command that merely *writes* a
	// script containing `sleep 90` / `tail -f` / `nohup ...` (via a
	// heredoc, or a quoted argument) from being rejected as if it ran
	// those -- only patterns at a real command position survive.
	clean := stripShellQuotedContent(stripHeredocs(cmd))
	if m := longWaitSleep.FindStringSubmatch(clean); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil && n > 5 {
			return fmt.Sprintf("contains `sleep %s` which would block %ss synchronously", m[1], m[1])
		}
	}
	for _, re := range foreverPatterns {
		if loc := re.FindStringIndex(clean); loc != nil {
			return fmt.Sprintf("contains a never-terminating pattern (%s)", clean[loc[0]:loc[1]])
		}
	}
	if reSyncNohup.MatchString(clean) {
		return "uses `nohup` (intent to detach)"
	}
	return ""
}

// rejectMessage builds the educational error returned when sync run
// hits a long-blocking pattern. Tells the model exactly what to swap
// to. nohup gets a tailored message (the cost isn't the 60s timeout,
// it's the startup-banner tokens captured into the result).
func rejectMessage(cmd, why string) string {
	// Decide the nohup-tailored message on the cleaned view (matching
	// rejectSync), but keep the original cmd for the suggested call.
	if reSyncNohup.MatchString(stripShellQuotedContent(stripHeredocs(cmd))) {
		stripped := stripNohup(cmd)
		return fmt.Sprintf(
			"rejected: %s. `nohup` signals you want a detached process, but synchronous `run` still captures the service's startup output (banners, listen ports, config dumps) into MCP tokens for no benefit -- typical service starts spend 4-5 KiB this way.\n\nUse the background pattern instead (drop the `nohup` prefix; srv detaches the job from the MCP session already):\n  run { command: %q, background: true }   -> returns job_id immediately (~150 B)\n  wait_job { id: <returned id> }           -> confirm it stayed up (default 8s, cap 15s)\n  tail_log { id: <returned id>, lines: N } -> read startup lines only if needed",
			why, stripped,
		)
	}
	return fmt.Sprintf(
		"rejected: %s. Synchronous `run` is bound by the MCP per-tool timeout (default 60s); long blocks tank the connection.\n\nUse the background pattern instead:\n  run { command: %q, background: true }   -> returns job_id immediately\n  wait_job { id: <returned id> }           -> short polls (default 8s, cap 15s)\n\nFor commands that legitimately need their full output streamed back synchronously, restructure them to finish in <60s (e.g. cap with `head`/`timeout 30`).",
		why, cmd,
	)
}

// stripNohup rewrites the command suggestion to drop a leading `nohup`
// (and any trailing `&` background marker) so the model copies a
// background-pattern call that doesn't fight srv's own detach. Only
// the first occurrence is stripped -- subsequent `nohup` inside the
// command (e.g. inside a script the user is invoking) is left intact.
//
// Best-effort cosmetic: a quoted-string `nohup` would also be stripped
// if it happened to sit at command position, but that case is so
// unusual it's not worth the parsing complexity.
func stripNohup(cmd string) string {
	out := reSyncNohupStrip.ReplaceAllString(cmd, "$1$2")
	// Trim a single trailing ` &` (with any preceding whitespace) --
	// background=true takes over the detach role and the model
	// shouldn't leave the literal `&` in the new command shape.
	out = reTrailingBackground.ReplaceAllString(out, "")
	return out
}

// reSyncNohupStrip mirrors reSyncNohup but captures the parts AROUND
// `nohup ` so a replacement can drop the keyword while keeping the
// leading separator (group 1) and the following first non-space char
// (group 2).
var reSyncNohupStrip = regexp.MustCompile(`((?:^|[;&|(){}\n]|\bdo\b|\bthen\b)\s*)nohup\s+(\S)`)

// reTrailingBackground matches a final ` &` (optionally followed by
// trailing whitespace). Used to strip the background marker after
// nohup is removed -- the new shape relies on background=true, not
// shell `&`.
var reTrailingBackground = regexp.MustCompile(`\s*&\s*$`)

// Token-economy gates for MCP `run`. The ResultByteMax (64 KiB) cap
// stops the model from drowning in output, but it doesn't stop the
// WASTED tokens that get paid when the model asks for an unbounded
// source and we serve them the wrong slice. Forcing an explicit
// slicing decision usually returns more relevant content AND saves
// tokens; the model can read the rejection and pick a `head -n N`
// / `tail -n N` / `grep` / dedicated MCP tool path.
var (
	// reBareCat matches `cat <file>` at a command-position. We don't
	// reject `cat` with no arg (it's just `stdin -> stdout`), nor a
	// write/heredoc form: `cat > f`, `cat >> f`, `cat <<EOF` redirect
	// (often inline heredoc content) INTO a file and read nothing
	// unbounded. So after any flags the first real token must not be a
	// redirect / heredoc operator or shell separator (`>`/`<`/`|`/`&`/`;`).
	// `cat < file` (input redirect) is exempted too -- the lone `<` is
	// indistinguishable from `<<` here and the case is rare.
	reBareCat        = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*cat\s+(?:-\S+\s+)*[^\s>|&;<]`)
	reBareDmesg      = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*dmesg\b`)
	reBareJournalctl = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*journalctl\b`)
	reBareFind       = regexp.MustCompile(`(?i)(?:^|[;&|\n])\s*find\s+/`)

	// reDownstreamLimiter -- a pipe into one of these counts as a
	// bound; downstream is conservative-limited and we let the call
	// through.
	reDownstreamLimiter  = regexp.MustCompile(`(?i)\|\s*(head|tail|grep|awk|sed|wc|cut|jq|sort|uniq|column|less|more|fold|tr|xxd|od)\b`)
	reHeadBounded        = regexp.MustCompile(`(?i)\bhead\s+-[cnN][= ]?\s*\d`)
	reTailBounded        = regexp.MustCompile(`(?i)\btail\s+-n[= ]?\s*\d`)
	reJournalctlFiltered = regexp.MustCompile(`(?i)\bjournalctl\b[^|;&\n]*?(\s-u\b|\s--unit\b|\s--since\b|\s--until\b|\s-S\b|\s-U\b|\s-n\b|\s-g\b|\s--grep\b|\s-p\b|\s--priority\b|\s-k\b|\s-f\b)`)
	reFindFiltered       = regexp.MustCompile(`(?i)\bfind\b[^|;&\n]*?\s-(maxdepth|name|iname|type|newer|mtime|mmin|size|path|prune|regex|wholename)\b`)
)

// rejectUnfiltered checks `cmd` (already quote-stripped) for
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
func rejectUnfiltered(cmd string) (string, string) {
	// Strip heredoc bodies first (so a script written via `tee <<'EOF'
	// ... cat foo ... EOF` isn't scanned as if it ran `cat foo`), then
	// quoted content (so `echo "cat foo"` doesn't trip either).
	stripped := stripShellQuotedContent(stripHeredocs(cmd))
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
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// reHeredocOp matches a heredoc operator (`<<` or `<<-`) and its
// delimiter word, allowing optional quotes (`<<'EOF'`, `<<"EOF"`) and
// optional whitespace (`<< EOF`). Group 1 is the bare delimiter name.
// Here-strings (`<<<`) don't match: the third `<` isn't a quote or a
// delimiter letter.
var reHeredocOp = regexp.MustCompile(`<<-?\s*["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)

// heredocDelims returns the delimiter words of every heredoc operator on
// `line` that sits at a code position. A `<<` buried in a quoted string
// (`echo "a << b"`) is not a heredoc and is skipped, so we don't strip
// lines that aren't actually a heredoc body.
func heredocDelims(line string) []string {
	if !strings.Contains(line, "<<") {
		return nil
	}
	locs := reHeredocOp.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		return nil
	}
	code := codePositions(line)
	var out []string
	for _, m := range locs {
		if m[0] < len(code) && code[m[0]] {
			out = append(out, line[m[2]:m[3]])
		}
	}
	return out
}

// stripHeredocs removes heredoc *body* lines from a (possibly
// multi-line) shell command, keeping the opening line -- which holds the
// real command, e.g. `tee file <<'EOF'` -- and the closing delimiter
// line, while dropping everything in between. Without this, the gates
// would scan inline file content (a script written via `tee <<'EOF' ...
// EOF` whose body legitimately contains `tail -f`, `cat foo`, `sleep
// 90`, ...) as if it were commands the shell is about to execute, and
// reject the write.
//
// Best-effort: handles `<<`, `<<-`, quoted/bare delimiters, and multiple
// heredocs opened on one line (consumed FIFO like bash). An unterminated
// heredoc consumes to end-of-input. Delimiter matching trims surrounding
// whitespace, which also covers the leading-tab stripping of `<<-`.
func stripHeredocs(s string) string {
	if !strings.Contains(s, "<<") {
		return s
	}
	lines := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s))
	first := true
	emit := func(l string) {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		b.WriteString(l)
	}
	i := 0
	for i < len(lines) {
		line := lines[i]
		emit(line)
		i++
		delims := heredocDelims(line)
		for di := 0; di < len(delims) && i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == delims[di] {
				emit(lines[i]) // keep the closing delimiter line
				di++
			}
			// else: a body line -> dropped (not emitted).
		}
	}
	return b.String()
}

// rejectUnfilteredMessage formats the rejection into the standard
// MCP shape: a clear text explanation + structured metadata the
// client can branch on.
func rejectUnfilteredMessage(label, body string) toolResult {
	r := textErr("rejected: " + body)
	r.StructuredContent = map[string]any{
		"rejected_reason": "unbounded_output",
		"pattern":         label,
	}
	return r
}

// emptyFilterRegex matches grep patterns that filter nothing in
// practice -- a `.*` / `.` / `.+` / `[\s\S]*` "filter" is a bypass
// dressed up as a regex.
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
// tools call before kicking off a stream. Rule: any follow_seconds
// > 0 requires at least one meaningful output filter. Earlier
// versions exempted "short" follows (≤5s), but a chatty log can
// flood the per-call progress channel even in five seconds, and the
// exemption left no incentive for the model to ever pass a filter.
//
// Returns nil to proceed, or a populated toolResult that the caller
// should `return *r`.
func requireStreamFilter(toolName string, follow int, filters []string, hint string) *toolResult {
	if follow <= 0 {
		return nil
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
	r := textErr(msg)
	r.StructuredContent = map[string]any{
		"rejected_reason": "unbounded_streaming",
		"follow_seconds":  follow,
	}
	return &r
}
