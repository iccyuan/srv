// Package guard implements `srv guard [on|off|status]` -- the
// per-session high-risk-op confirmation toggle. ON by default; the
// gate fires unless the user runs `srv guard off`. When on, the
// built-in set requires an explicit confirm flag from the MCP
// client for: irreversible destruction (rm -rf, mkfs, dd of=, drop
// database, truncate table, :> /path, > /dev/disk) plus host
// power-control (shutdown, reboot, halt, poweroff). Pure precursors
// like `chattr -i` are NOT in the default set -- add them with
// `srv guard add` if you want them gated too.
//
// Rule management (list/add/rm/allow/defaults) and the on/off/status
// toggle share one flat `guard` action space: `srv guard list`,
// `srv guard add <name> <re>`, `srv guard on`, etc.
//
// Storage lives in internal/session (the sessions.json file). This
// package is just the CLI shim that toggles / reports state and
// echoes the SRV_GUARD env override so users can tell where a yes/no
// answer is coming from.
package guard

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"srv/internal/config"
	"srv/internal/mcp"
	"srv/internal/session"
	"srv/internal/srvutil"
	"strings"
)

// Cmd implements `srv guard [on|off|status|list|add|rm|allow|defaults|test]`.
// Default action is `status`. Output is intentionally one-line so it
// pipes cleanly. list/add/rm/allow/defaults manage the persisted
// deny/allow rule set; on/off/status toggle the per-session gate.
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
	case "list", "add", "rm", "remove", "allow", "defaults":
		// Rule management, flat under `guard`. cmdRules treats
		// args[0] as the rule action, so pass the full args.
		return cmdRules(args)
	case "test", "dry-run":
		if len(args) < 2 {
			return srvutil.Errf(2, "usage: srv guard test \"<command>\"")
		}
		cmd := strings.Join(args[1:], " ")
		hit := mcp.RiskyMatchPublic(cmd)
		if hit == "" {
			fmt.Printf("PASS  no match (command would be allowed by current rules)\n")
			return nil
		}
		fmt.Printf("BLOCK matches rule %q\n", hit)
		return srvutil.Code(1)
	case "on", "enable":
		sid, err := session.SetGuard(true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard on:", err)
			return srvutil.Code(1)
		}
		fmt.Printf("guard: on  (session=%s)%s\n", sid, envHint())
		return nil
	case "off", "disable":
		sid, err := session.SetGuard(false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guard off:", err)
			return srvutil.Code(1)
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
	fmt.Fprintln(os.Stderr, "usage: srv guard [on|off|status|list|add|rm|allow|defaults|test \"<cmd>\"]")
	return srvutil.Code(2)
}

// cmdRules dispatches the flat rule actions `srv guard <list|add|rm|
// allow|defaults>` (args[0] is the action). Reads / writes the
// persisted GuardConfig in ~/.srv/config.json.
func cmdRules(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return srvutil.Errf(1, "config load: %v", err)
	}
	if cfg == nil {
		cfg = config.New()
	}
	if len(args) == 0 || args[0] == "list" {
		return rulesList(cfg)
	}
	switch args[0] {
	case "add":
		if len(args) < 3 {
			return srvutil.Errf(2, "usage: srv guard add <name> <regex>")
		}
		return rulesAdd(cfg, args[1], strings.Join(args[2:], " "))
	case "rm", "remove":
		if len(args) < 2 {
			return srvutil.Errf(2, "usage: srv guard rm <name>")
		}
		return rulesRm(cfg, args[1])
	case "allow":
		if len(args) < 2 {
			return rulesAllowList(cfg)
		}
		if args[1] == "rm" {
			if len(args) < 3 {
				return srvutil.Errf(2, "usage: srv guard allow rm <regex>")
			}
			return rulesAllowRm(cfg, args[2])
		}
		return rulesAllowAdd(cfg, strings.Join(args[1:], " "))
	case "defaults":
		if len(args) < 2 {
			fmt.Printf("defaults: %s\n", boolDisplay(!gcDisableDefaults(cfg)))
			return nil
		}
		on := strings.ToLower(args[1]) != "off" && args[1] != "false"
		if cfg.Guard == nil {
			cfg.Guard = &config.GuardConfig{}
		}
		cfg.Guard.DisableDefaults = !on
		if err := config.Save(cfg); err != nil {
			return srvutil.Errf(1, "%v", err)
		}
		fmt.Printf("defaults: %s\n", boolDisplay(on))
		return nil
	}
	return srvutil.Errf(2, "usage: srv guard [list|add|rm|allow|defaults]")
}

func rulesList(cfg *config.Config) error {
	gc := cfg.Guard
	if gc == nil {
		gc = &config.GuardConfig{}
	}
	fmt.Printf("defaults: %s\n", boolDisplay(!gc.DisableDefaults))
	if len(gc.Rules) == 0 && len(gc.Allow) == 0 {
		fmt.Println("(no custom rules; using built-in patterns only)")
		return nil
	}
	if len(gc.Rules) > 0 {
		fmt.Println("\ndeny rules:")
		names := make([]string, 0, len(gc.Rules))
		idx := map[string]int{}
		for i, r := range gc.Rules {
			names = append(names, r.Name)
			idx[r.Name] = i
		}
		sort.Strings(names)
		for _, n := range names {
			r := gc.Rules[idx[n]]
			fmt.Printf("  %-20s %s\n", r.Name, r.Pattern)
		}
	}
	if len(gc.Allow) > 0 {
		fmt.Println("\nallow patterns (bypass deny rules):")
		for _, p := range gc.Allow {
			fmt.Printf("  %s\n", p)
		}
	}
	return nil
}

func rulesAdd(cfg *config.Config, name, pattern string) error {
	if _, err := regexp.Compile(pattern); err != nil {
		return srvutil.Errf(2, "bad regex %q: %v", pattern, err)
	}
	if cfg.Guard == nil {
		cfg.Guard = &config.GuardConfig{}
	}
	// Replace existing rule by name rather than appending duplicates.
	for i, r := range cfg.Guard.Rules {
		if r.Name == name {
			cfg.Guard.Rules[i].Pattern = pattern
			if err := config.Save(cfg); err != nil {
				return srvutil.Errf(1, "%v", err)
			}
			fmt.Printf("updated rule %q\n", name)
			return nil
		}
	}
	cfg.Guard.Rules = append(cfg.Guard.Rules, config.GuardRule{Name: name, Pattern: pattern})
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	fmt.Printf("added rule %q\n", name)
	return nil
}

func rulesRm(cfg *config.Config, name string) error {
	if cfg.Guard == nil {
		return srvutil.Errf(1, "no rule %q", name)
	}
	out := cfg.Guard.Rules[:0]
	removed := false
	for _, r := range cfg.Guard.Rules {
		if r.Name == name {
			removed = true
			continue
		}
		out = append(out, r)
	}
	if !removed {
		return srvutil.Errf(1, "no rule %q", name)
	}
	cfg.Guard.Rules = out
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	fmt.Printf("removed rule %q\n", name)
	return nil
}

func rulesAllowList(cfg *config.Config) error {
	if cfg.Guard == nil || len(cfg.Guard.Allow) == 0 {
		fmt.Println("(no allow patterns)")
		return nil
	}
	for _, p := range cfg.Guard.Allow {
		fmt.Println(p)
	}
	return nil
}

func rulesAllowAdd(cfg *config.Config, pattern string) error {
	if _, err := regexp.Compile(pattern); err != nil {
		return srvutil.Errf(2, "bad regex %q: %v", pattern, err)
	}
	if cfg.Guard == nil {
		cfg.Guard = &config.GuardConfig{}
	}
	for _, p := range cfg.Guard.Allow {
		if p == pattern {
			fmt.Printf("(already present)\n")
			return nil
		}
	}
	cfg.Guard.Allow = append(cfg.Guard.Allow, pattern)
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	fmt.Printf("added allow %q\n", pattern)
	return nil
}

func rulesAllowRm(cfg *config.Config, pattern string) error {
	if cfg.Guard == nil {
		return srvutil.Errf(1, "no allow pattern %q", pattern)
	}
	out := cfg.Guard.Allow[:0]
	removed := false
	for _, p := range cfg.Guard.Allow {
		if p == pattern {
			removed = true
			continue
		}
		out = append(out, p)
	}
	if !removed {
		return srvutil.Errf(1, "no allow pattern %q", pattern)
	}
	cfg.Guard.Allow = out
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	fmt.Printf("removed allow %q\n", pattern)
	return nil
}

func gcDisableDefaults(cfg *config.Config) bool {
	return cfg.Guard != nil && cfg.Guard.DisableDefaults
}

func boolDisplay(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
