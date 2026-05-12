// Package srvutil holds tiny, stateless helpers used across srv that
// have no dependency on any srv-specific type. Moving them out of
// package main lets feature modules pull in just the helpers they
// need without dragging in the rest of main as a transitive dep.
package srvutil

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"sync"
)

// IntToStr / UintToStr are pure conveniences -- srv builds enough
// "fmt.Sprintf(\"%d\", n)"-style id strings that having a named helper
// shortened call sites measurably.
func IntToStr(i int) string     { return strconv.Itoa(i) }
func UintToStr(i uint32) string { return strconv.FormatUint(uint64(i), 10) }

// RandHex4 returns 4 random hex chars (2 bytes). Falls back to a plain
// fixed string on error so we still produce a non-empty id even when
// crypto/rand is unavailable (which only happens in deeply broken
// environments).
func RandHex4() string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

// StrPtr / BoolPtr are tiny helpers for JSON struct fields that need
// *string / *bool. The pointer-vs-value distinction in Go marshalling
// is the difference between "zero" and "absent" -- using these makes
// the intent obvious at call sites.
func StrPtr(s string) *string { return &s }
func BoolPtr(b bool) *bool    { return &b }

// AllDigits reports whether every rune in s is an ASCII digit. Used by
// callers that need to distinguish numeric-looking ids from names.
func AllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

// RegexMatch compiles `pattern` (caching the compiled form) and tests
// s. Used by the glob -> regex translator for sync includes/excludes;
// the cache pays for itself because the same patterns recur on every
// file the sync walker visits.
func RegexMatch(pattern, s string) bool {
	regexCacheMu.Lock()
	re, ok := regexCache[pattern]
	if !ok {
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			regexCache[pattern] = nil
			regexCacheMu.Unlock()
			return false
		}
		regexCache[pattern] = re
	}
	regexCacheMu.Unlock()
	if re == nil {
		return false
	}
	return re.MatchString(s)
}
