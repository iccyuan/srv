package hooks

import (
	"fmt"
	"os"
	"sort"
	"srv/internal/config"
	"srv/internal/srvutil"
	"strings"
)

func usageText() string {
	return `usage: srv hooks <list|show|set|add|rm|run> [args]
  srv hooks list                     show every configured hook
  srv hooks show <event>             show commands for one event
  srv hooks set <event> <cmd>        replace the hook list for <event>
  srv hooks add <event> <cmd>        append one more command for <event>
  srv hooks rm <event> [<index>]     remove all (or just the n-th) commands
  srv hooks run <event>              fire a hook manually (good for debugging)

events: ` + strings.Join(KnownEvents, ", ")
}

// Cmd dispatches `srv hooks ...` subcommands. cfg is loaded once so we
// re-use its in-memory map; persistence routes back through config.Save.
func Cmd(args []string, cfg *config.Config) error {
	if len(args) == 0 {
		return cmdList(cfg)
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "list":
		return cmdList(cfg)
	case "show":
		if len(rest) == 0 {
			return srvutil.Errf(1, "%s", usageText())
		}
		return cmdShow(cfg, rest[0])
	case "set":
		if len(rest) < 2 {
			return srvutil.Errf(1, "%s", usageText())
		}
		return cmdSet(cfg, rest[0], strings.Join(rest[1:], " "), false)
	case "add":
		if len(rest) < 2 {
			return srvutil.Errf(1, "%s", usageText())
		}
		return cmdSet(cfg, rest[0], strings.Join(rest[1:], " "), true)
	case "rm", "remove":
		if len(rest) == 0 {
			return srvutil.Errf(1, "%s", usageText())
		}
		return cmdRm(cfg, rest)
	case "run":
		if len(rest) == 0 {
			return srvutil.Errf(1, "%s", usageText())
		}
		return cmdRun(rest[0])
	case "events":
		for _, e := range KnownEvents {
			fmt.Println(e)
		}
		return nil
	}
	return srvutil.Errf(1, "%s", usageText())
}

func cmdList(cfg *config.Config) error {
	if len(cfg.Hooks) == 0 {
		fmt.Println("(no hooks configured)")
		fmt.Println()
		fmt.Println("known events:")
		for _, e := range KnownEvents {
			fmt.Printf("  %s\n", e)
		}
		return nil
	}
	events := make([]string, 0, len(cfg.Hooks))
	for k := range cfg.Hooks {
		events = append(events, k)
	}
	sort.Strings(events)
	for _, ev := range events {
		cmds := cfg.Hooks[ev]
		if len(cmds) == 0 {
			continue
		}
		fmt.Printf("%s\n", ev)
		for i, c := range cmds {
			fmt.Printf("  [%d] %s\n", i, c)
		}
	}
	return nil
}

func cmdShow(cfg *config.Config, event string) error {
	if !IsKnownEvent(event) {
		fmt.Fprintf(os.Stderr, "srv: warning: %q is not a recognised event (known: %s)\n",
			event, strings.Join(KnownEvents, ", "))
	}
	cmds := cfg.Hooks[event]
	if len(cmds) == 0 {
		fmt.Printf("(no commands for %s)\n", event)
		return nil
	}
	for i, c := range cmds {
		fmt.Printf("[%d] %s\n", i, c)
	}
	return nil
}

func cmdSet(cfg *config.Config, event, cmdStr string, appendMode bool) error {
	if !IsKnownEvent(event) {
		return srvutil.Errf(1, "srv: unknown event %q (known: %s)",
			event, strings.Join(KnownEvents, ", "))
	}
	if cfg.Hooks == nil {
		cfg.Hooks = map[string][]string{}
	}
	if appendMode {
		cfg.Hooks[event] = append(cfg.Hooks[event], cmdStr)
	} else {
		cfg.Hooks[event] = []string{cmdStr}
	}
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "error: %v", err)
	}
	verb := "set"
	if appendMode {
		verb = "added"
	}
	fmt.Printf("%s %s: %s\n", verb, event, cmdStr)
	return nil
}

func cmdRm(cfg *config.Config, args []string) error {
	event := args[0]
	if cfg.Hooks == nil || len(cfg.Hooks[event]) == 0 {
		fmt.Printf("(no commands for %s)\n", event)
		return nil
	}
	if len(args) == 1 {
		delete(cfg.Hooks, event)
		if err := config.Save(cfg); err != nil {
			return srvutil.Errf(1, "error: %v", err)
		}
		fmt.Printf("removed all hooks for %s\n", event)
		return nil
	}
	// Numeric index removal: keep stable ordering of remaining.
	idx, err := parseIndex(args[1])
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	cmds := cfg.Hooks[event]
	if idx < 0 || idx >= len(cmds) {
		return srvutil.Errf(1, "srv: index %d out of range [0,%d)", idx, len(cmds))
	}
	removed := cmds[idx]
	cfg.Hooks[event] = append(cmds[:idx], cmds[idx+1:]...)
	if len(cfg.Hooks[event]) == 0 {
		delete(cfg.Hooks, event)
	}
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "error: %v", err)
	}
	fmt.Printf("removed %s[%d]: %s\n", event, idx, removed)
	return nil
}

func cmdRun(event string) error {
	if !IsKnownEvent(event) {
		fmt.Fprintf(os.Stderr, "srv: warning: %q is not a recognised event\n", event)
	}
	Run(Event{Name: event})
	return nil
}

func parseIndex(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("srv: missing index")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("srv: index must be a non-negative integer (got %q)", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
