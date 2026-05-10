package main

import (
	"fmt"
	"os"
	"strings"
)

// cmdGuard implements `srv guard [on|off|status]`. Toggles or shows the
// per-session high-risk-op confirmation guard; off by default. Honors
// the SRV_GUARD env override (which the `status` form prints alongside
// the session record's value so users can tell where a yes/no is coming
// from). Output is intentionally one-line so it pipes cleanly.
func cmdGuard(args []string) int {
	envHint := func() string {
		if v := os.Getenv("SRV_GUARD"); v != "" {
			return fmt.Sprintf("  [SRV_GUARD=%s]", v)
		}
		return ""
	}
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	switch action {
	case "on", "enable":
		sid, err := SetGuard(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard on:", err)
			return 1
		}
		fmt.Printf("guard: on  (session=%s)%s\n", sid, envHint())
		return 0
	case "off", "disable":
		sid, err := SetGuard(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard off:", err)
			return 1
		}
		fmt.Printf("guard: off (session=%s)%s\n", sid, envHint())
		return 0
	case "status", "":
		sid := SessionID()
		state := "off"
		if GuardOn() {
			state = "on"
		}
		fmt.Printf("guard: %s (session=%s)%s\n", state, sid, envHint())
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: srv guard [on|off|status]")
	return 2
}
