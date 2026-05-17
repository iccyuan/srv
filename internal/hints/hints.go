// Package hints emits "did you mean?" nudges when the dispatcher sees
// a probable typo. Two trigger points:
//
//  1. Pre-dispatch: a user typed an unknown subcommand that's within
//     edit distance 2 of a known one. We still run the command on the
//     remote (the user's command might be the right one), but print a
//     one-line stderr nudge first.
//
//  2. Post-failure: a remote command exited 127 ("command not found").
//     Same fuzzy match against the first word of the original command.
//
// Disable knobs (any of):
//   - SRV_HINTS=0 / false / off / no (env, highest priority)
//   - --no-hints CLI flag (passed in via the noHints bool)
//   - cfg.Hints = false in ~/.srv/config.json
//
// MCP path skips hints entirely -- stderr there is read by the model
// as tool output, and a stray "did you mean status?" lands in the
// reasoning loop.
package hints

import (
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/i18n"
	"strings"
	"sync"
)

// candidates is the universe of names Suggest will fuzzy-match
// against. Populated once at startup by SetCandidates(); reads are
// race-free because the dispatch loop is strictly serial.
var (
	candidatesMu  sync.RWMutex
	candidatesSet map[string]bool
)

// candidatesExcluded names are filtered from fuzzy matching even if
// passed to SetCandidates. Two categories:
//   - internal helpers (_profiles / _ls) that completion uses but
//     users don't type;
//   - dash-flag aliases that would create noisy false positives for
//     short tokens like "-r" → "--help".
var candidatesExcluded = map[string]bool{
	"_profiles": true, "_ls": true,
	"--help": true, "-h": true, "--version": true,
}

// SetCandidates installs the list of names that Suggest considers.
// Main calls this once during init from the reserved-subcommand
// registry. Safe to call again later (e.g. a future plugin system);
// the last call wins.
func SetCandidates(names []string) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if candidatesExcluded[n] {
			continue
		}
		set[n] = true
	}
	candidatesMu.Lock()
	candidatesSet = set
	candidatesMu.Unlock()
}

// candidateNames returns the current candidates as a slice; thread-
// safe snapshot since SetCandidates replaces the map atomically.
func candidateNames() []string {
	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	out := make([]string, 0, len(candidatesSet))
	for n := range candidatesSet {
		out = append(out, n)
	}
	return out
}

// isCandidate reports whether `name` is in the active candidate set.
// Used by Suggest to short-circuit exact matches (the dispatcher
// would already have routed there).
func isCandidate(name string) bool {
	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	return candidatesSet[name]
}

// Allowed reports whether hint output should fire for this
// invocation. Honors SRV_HINTS env, the --no-hints flag, and
// cfg.Hints in that order.
func Allowed(cfg *config.Config, noHints bool) bool {
	if v := strings.ToLower(os.Getenv("SRV_HINTS")); v == "0" || v == "false" || v == "off" || v == "no" {
		return false
	}
	if noHints {
		return false
	}
	if cfg != nil && !cfg.HintsEnabled() {
		return false
	}
	return true
}

// Suggest returns the closest known subcommand to `s`, or "" when
// nothing is close enough. Threshold rules:
//   - first letter must match (cheap false-positive guard)
//   - distance <= 1 for short tokens (< 5 chars)
//   - distance <= 2 for longer tokens (>= 5 chars)
//   - exact matches return "" (caller would already have routed there)
func Suggest(s string) string {
	if s == "" || isCandidate(s) {
		return ""
	}
	first := s[0]
	threshold := 1
	if len(s) >= 5 {
		threshold = 2
	}
	best := ""
	bestDist := threshold + 1
	for _, cand := range candidateNames() {
		if cand == "" || cand[0] != first {
			continue
		}
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

// levenshtein computes the standard edit distance between a and b.
// Two row buffers; allocates O(min(len(a), len(b))) once.
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

// EmitTypoPre prints a one-line hint to stderr when the dispatcher
// is about to send `sub` to the remote and there's a near-match
// local subcommand. No-op when hints are disabled.
func EmitTypoPre(cfg *config.Config, noHints bool, sub string) {
	if !Allowed(cfg, noHints) {
		return
	}
	if match := Suggest(sub); match != "" {
		fmt.Fprintln(os.Stderr, i18n.T("hint.typo_pre", sub, match))
	}
}

// EmitTypoPostFailure prints a hint after a remote command exited
// 127 ("command not found"). Pulls the first whitespace-delimited
// word out of `cmd` (after stripping any leading path), then runs it
// through Suggest.
func EmitTypoPostFailure(cfg *config.Config, noHints bool, cmd string, exitCode int) {
	if exitCode != 127 {
		return
	}
	if !Allowed(cfg, noHints) {
		return
	}
	first := strings.TrimSpace(cmd)
	if i := strings.IndexAny(first, " \t"); i > 0 {
		first = first[:i]
	}
	if idx := strings.LastIndexAny(first, "/\\"); idx >= 0 {
		first = first[idx+1:]
	}
	if first == "" {
		return
	}
	if match := Suggest(first); match != "" {
		fmt.Fprintln(os.Stderr, i18n.T("hint.typo_post", first, match, match))
	}
}
