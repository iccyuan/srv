package main

import (
	"fmt"
	"os"
	"sort"
	"srv/internal/srvtty"
	"strings"
)

func cmdEnv(args []string, cfg *Config, profileOverride string) error {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
	}
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "list":
		keys := make([]string, 0, len(profile.Env))
		for k := range profile.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, profile.Env[k])
		}
	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: srv env set <key> <value>")
			return exitCode(2)
		}
		if profile.Env == nil {
			profile.Env = map[string]string{}
		}
		profile.Env[args[1]] = strings.Join(args[2:], " ")
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitCode(1)
		}
		fmt.Printf("%s.%s=%s\n", name, args[1], profile.Env[args[1]])
	case "unset":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: srv env unset <key>")
			return exitCode(2)
		}
		delete(profile.Env, args[1])
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitCode(1)
		}
	case "clear":
		profile.Env = nil
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitCode(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: srv env [list|set|unset|clear]")
		return exitCode(2)
	}
	return nil
}

// applyRemoteEnv prepends `KEY=value ...` exports to the user's command,
// using the profile's per-target env map. Sorted for deterministic shape
// (so the same command builds to the same string under repeated calls).
func applyRemoteEnv(profile *Profile, cmd string) string {
	if profile == nil || len(profile.Env) == 0 {
		return cmd
	}
	keys := make([]string, 0, len(profile.Env))
	for k := range profile.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		parts = append(parts, k+"="+srvtty.ShQuote(profile.Env[k]))
	}
	if len(parts) == 0 {
		return cmd
	}
	return strings.Join(parts, " ") + " " + cmd
}
