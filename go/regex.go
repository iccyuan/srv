package main

import (
	"regexp"
	"sync"
)

var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

// regexMatch compiles `pattern` (caching the compiled form) and tests s.
// Used by the glob -> regex translator for sync includes/excludes.
func regexMatch(pattern, s string) bool {
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
