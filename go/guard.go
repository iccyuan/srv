package main

import (
	"fmt"
	"os"
	"srv/internal/session"
	"strings"
)

// cmdGuard implements `srv guard [on|off|status]`. Toggles or shows the
// per-session high-risk-op confirmation guard; off by default. Honors
// the SRV_GUARD env override (which the `status` form prints alongside
// the session record's value so users can tell where a yes/no is coming
// from). Output is intentionally one-line so it pipes cleanly.
func cmdGuard(args []string) error {
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
		sid, err := session.SetGuard(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard on:", err)
			return exitCode(1)
		}
		fmt.Printf("guard: on  (session=%s)%s\n", sid, envHint())
		return nil
	case "off", "disable":
		sid, err := session.SetGuard(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard off:", err)
			return exitCode(1)
		}
		fmt.Printf("guard: off (session=%s)%s\n", sid, envHint())
		return nil
	case "status", "":
		sid := session.ID()
		state := "off"
		if session.GuardOn() {
			state = "on"
		}
		fmt.Printf("guard: %s (session=%s)%s\n", state, sid, envHint())
		return nil
	}
	fmt.Fprintln(os.Stderr, "usage: srv guard [on|off|status]")
	return exitCode(2)
}
