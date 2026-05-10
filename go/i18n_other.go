//go:build !windows

package main

// platformLang has no work to do off Windows: Linux/macOS users get
// LC_ALL / LC_MESSAGES / LANG populated by their shells / login session,
// and detectLang already honors those.
func platformLang() string { return "" }
