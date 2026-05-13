// Package guard implements `srv guard [on|off|status]` -- the
// per-session high-risk-op confirmation toggle. Off by default;
// when on, destructive remote commands (rm -rf, mkfs, drop database,
// ...) require an explicit confirm flag from the MCP client.
//
// Storage lives in internal/session (the sessions.json file). This
// package is just the CLI shim that toggles / reports state and
// echoes the SRV_GUARD env override so users can tell where a yes/no
// answer is coming from.
package guard

import (
	"fmt"
	"os"
	"srv/internal/clierr"
	"srv/internal/session"
	"strings"
)

// Cmd implements `srv guard [on|off|status]`. Default action is
// `status`. Output is intentionally one-line so it pipes cleanly.
func Cmd(args []string) error {
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
			return clierr.Code(1)
		}
		fmt.Printf("guard: on  (session=%s)%s\n", sid, envHint())
		return nil
	case "off", "disable":
		sid, err := session.SetGuard(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard off:", err)
			return clierr.Code(1)
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
	return clierr.Code(2)
}
