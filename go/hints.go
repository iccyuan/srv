package main

import (
	"fmt"
	"os"
	"strings"
)

// "Did you mean?" hint emitter.
//
// Two trigger points:
//
//  1. Pre-dispatch typo: when the first arg isn't a known subcommand
//     (so it would run on the remote), but is within edit distance 2
//     of one. Print a single-line stderr nudge and continue running on
//     the remote anyway -- the user's command might be the right one,
//     it just looks like a typo.
//
//  2. Post-failure: when a remote command exits 127 (bash convention
//     for "command not found"), check the same fuzzy-match table
//     against the first word of the original command. If anything
//     close turns up, suggest the local subcommand.
//
// Disable knobs (any of):
//   - SRV_HINTS=0 / SRV_HINTS=false (env, highest priority -- works
//     even before config loads)
//   - --no-hints global flag (per-call)
//   - cfg.Hints set to false in ~/.srv/config.json
//
// MCP path skips hints entirely -- stderr there is read by Claude Code
// as tool output, and we don't want to add hint text into a model's
// reasoning loop.

// hintCandidates returns public reserved subcommands considered for
// fuzzy matching. We exclude internal helpers (`_profiles`, `_ls`),
// the help / version aliases, and the dash flag aliases since those
// would create noisy false positives for short tokens.
func hintCandidates() []string {
	exclude := map[string]bool{
		"_profiles": true, "_ls": true,
		"--help": true, "-h": true, "--version": true,
	}
	out := make([]string, 0, len(reservedSubcommands))
	for k := range reservedSubcommands {
		if exclude[k] {
			continue
		}
		out = append(out, k)
	}
	return out
}

// hintsAllowed reports whether to fire hint output for this invocation.
// Honors SRV_HINTS env, --no-hints flag, and cfg.Hints in that order.
func hintsAllowed(cfg *Config, opts globalOpts) bool {
	if v := strings.ToLower(os.Getenv("SRV_HINTS")); v == "0" || v == "false" || v == "off" || v == "no" {
		return false
	}
	if opts.noHints {
		return false
	}
	if cfg != nil && !cfg.HintsEnabled() {
		return false
	}
	return true
}

// suggestSubcommand returns the closest public reserved subcommand to
// `s` if one is within the threshold, else "". Threshold rules:
//   - first letter must match (cheap false-positive guard)
//   - distance <= 1 for short tokens (< 5 chars)
//   - distance <= 2 for longer tokens (>= 5 chars)
//   - exact matches return "" (caller would already have routed there)
func suggestSubcommand(s string) string {
	if s == "" {
		return ""
	}
	if reservedSubcommands[s] {
		return ""
	}
	first := s[0]
	threshold := 1
	if len(s) >= 5 {
		threshold = 2
	}
	best := ""
	bestDist := threshold + 1
	for _, cand := range hintCandidates() {
		if cand == "" || cand[0] != first {
			continue
		}
		// Skip wildly different lengths early.
		diff := len(cand) - len(s)
		if diff < 0 {
			diff = -diff
		}
		if diff > threshold {
			continue
		}
		d := levenshtein(s, cand)
		if d < bestDist {
			bestDist = d
			best = cand
		}
	}
	if bestDist <= threshold {
		return best
	}
	return ""
}

// levenshtein computes the standard edit distance between a and b. Two
// row buffers; allocates O(min(len(a), len(b))) once.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			min := ins
			if del < min {
				min = del
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// emitTypoHintPre prints a one-line hint to stderr when the dispatcher
// is about to send `sub` to the remote and there's a near-match local
// subcommand. No-op when hints are disabled.
func emitTypoHintPre(cfg *Config, opts globalOpts, sub string) {
	if !hintsAllowed(cfg, opts) {
		return
	}
	if match := suggestSubcommand(sub); match != "" {
		fmt.Fprintln(os.Stderr, t("hint.typo_pre", sub, match))
	}
}

// cmdRunWithHints wraps cmdRun so post-failure hints fire when the
// remote command can't be found (exit 127). Used by both the default
// "fall through to remote" dispatch path and the explicit `srv run`
// subcommand.
func cmdRunWithHints(args []string, cfg *Config, opts globalOpts) error {
	err := cmdRun(args, cfg, opts.profile, opts.tty)
	rc := exitCodeOf(err)
	if rc == 127 && len(args) > 0 {
		emitTypoHintPostFailure(cfg, opts, strings.Join(args, " "), rc)
	}
	return err
}

// emitTypoHintPostFailure prints a hint after a remote command exited
// 127 ("command not found"). `cmd` is the remote command line (or just
// its first token, the actual exec name). We pull the first whitespace-
// delimited word out and check it against the candidate table.
func emitTypoHintPostFailure(cfg *Config, opts globalOpts, cmd string, exitCode int) {
	if exitCode != 127 {
		return
	}
	if !hintsAllowed(cfg, opts) {
		return
	}
	first := strings.TrimSpace(cmd)
	if i := strings.IndexAny(first, " \t"); i > 0 {
		first = first[:i]
	}
	// Strip a leading "./" / absolute path so heuristic still works for
	// tokens like "./pwd" or "/usr/local/bin/staus".
	if idx := strings.LastIndexAny(first, "/\\"); idx >= 0 {
		first = first[idx+1:]
	}
	if first == "" {
		return
	}
	if match := suggestSubcommand(first); match != "" {
		fmt.Fprintln(os.Stderr, t("hint.typo_post", first, match, match))
	}
}
