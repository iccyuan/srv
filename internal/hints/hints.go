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
//
// candidatesSlice is the immutable slice view used by Suggest's hot
// loop -- precomputed at SetCandidates time so we don't rebuild the
// snapshot on every typo check. Replaced wholesale (never mutated in
// place), so a reader that captured an old reference stays consistent.
var (
	candidatesMu    sync.RWMutex
	candidatesSet   map[string]bool
	candidatesSlice []string
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
// the last call wins. Builds both the lookup map and the immutable
// iteration slice so Suggest's hot path never has to materialise one.
func SetCandidates(names []string) {
	set := make(map[string]bool, len(names))
	slice := make([]string, 0, len(names))
	for _, n := range names {
		if candidatesExcluded[n] {
			continue
		}
		if !set[n] {
			slice = append(slice, n)
		}
		set[n] = true
	}
	candidatesMu.Lock()
	candidatesSet = set
	candidatesSlice = slice
	candidatesMu.Unlock()
}

// candidateNames returns the precomputed iteration slice. The slice is
// never mutated in place -- SetCandidates swaps in a new one -- so the
// returned reference is safe to range over without holding the lock.
func candidateNames() []string {
	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	return candidatesSlice
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
	// Two DP row buffers, allocated once and reused across every
	// candidate. Size: the inner dimension of levenshtein is
	// min(len(a), len(b)) + 1; since the length-delta filter below
	// rejects any candidate with |len(cand) - len(s)| > threshold,
	// min <= len(s), so len(s)+1 always suffices.
	bufLen := len(s) + 1
	prev := make([]int, bufLen)
	curr := make([]int, bufLen)
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
		d := levenshteinInto(s, cand, prev, curr)
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
// Thin wrapper around levenshteinInto that allocates fresh buffers --
// kept for tests and any external single-shot caller. Hot paths
// (Suggest) call levenshteinInto with reused buffers instead.
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
	return levenshteinDP(a, b, prev, curr)
}

// levenshteinInto is the same edit-distance function as levenshtein
// but takes caller-owned row buffers so a sweep across N candidates
// allocates 0 times instead of 2N. Buffers must each have capacity
// >= min(len(a), len(b)) + 1; Suggest enforces this by sizing them to
// len(s)+1 (and pre-filtering candidates by length delta).
func levenshteinInto(a, b string, prev, curr []int) int {
	if a == b {
		return 0
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}
	return levenshteinDP(a, b, prev[:len(b)+1], curr[:len(b)+1])
}

// levenshteinDP runs the classic two-row DP. Pre-condition: prev and
// curr have length exactly len(b)+1, and len(a) >= len(b) > 0.
func levenshteinDP(a, b string, prev, curr []int) int {
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
			curr[j] = min(ins, del, sub)
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
