package main

import (
	"fmt"
	"sort"
	"srv/internal/sshx"
	"time"
)

// Saved-tunnel management: persists named tunnel definitions in
// Config.Tunnels (handled here) and talks to the daemon to bring them
// up / down (the daemon owns the actual goroutines so the tunnel
// outlives the CLI process).

// tunnelAdd parses the same arg shape as one-shot `srv tunnel` plus a
// leading <name> and optional --autostart, then saves the result.
//
//	srv tunnel add db -L 5432:db.internal:5432 -P prod --autostart
//	srv tunnel add web 8080
func tunnelAdd(args []string, cfg *Config, profileOverride string) error {
	if len(args) < 2 {
		return exitErr(2, "usage: srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart]")
	}
	name := args[0]
	rest := args[1:]

	def := &TunnelDef{Type: "local"}
	if profileOverride != "" {
		def.Profile = profileOverride
	}
	specSeen := false

	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-R" || a == "--reverse":
			def.Type = "remote"
		case a == "-L" || a == "--local":
			def.Type = "local"
		case a == "--autostart":
			def.Autostart = true
		case a == "-P" || a == "--profile":
			if i+1 >= len(rest) {
				return exitErr(2, "%s requires a value", a)
			}
			def.Profile = rest[i+1]
			i++
		case len(a) > 10 && a[:10] == "--profile=":
			def.Profile = a[10:]
		default:
			if specSeen {
				return exitErr(2, "unexpected arg %q (already have spec %q)", a, def.Spec)
			}
			if _, _, _, err := sshx.ParseTunnelSpec(a); err != nil {
				return exitErr(2, "bad spec %q: %v", a, err)
			}
			def.Spec = a
			specSeen = true
		}
	}
	if !specSeen {
		return exitErr(2, "missing port spec (e.g. 8080 or 5432:db:5432)")
	}
	if def.Profile != "" {
		if _, ok := cfg.Profiles[def.Profile]; !ok {
			return exitErr(1, "profile %q not found", def.Profile)
		}
	}

	if cfg.Tunnels == nil {
		cfg.Tunnels = map[string]*TunnelDef{}
	}
	cfg.Tunnels[name] = def
	if err := SaveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("tunnel %q saved (%s %s", name, def.Type, def.Spec)
	if def.Profile != "" {
		fmt.Printf(", profile=%s", def.Profile)
	}
	if def.Autostart {
		fmt.Print(", autostart")
	}
	fmt.Println(")")
	return nil
}

func tunnelRemove(args []string, cfg *Config) error {
	if len(args) < 1 {
		return exitErr(2, "usage: srv tunnel remove <name>")
	}
	name := args[0]
	if _, ok := cfg.Tunnels[name]; !ok {
		return exitErr(1, "tunnel %q not found", name)
	}
	delete(cfg.Tunnels, name)
	if err := SaveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("tunnel %q removed (consider `srv tunnel down %s` to stop a running instance)\n", name, name)
	return nil
}

// tunnelList prints saved tunnels and overlays daemon status so the
// user sees in one shot which are defined and which are actually
// running. Last-attempt errors (autostart-on-boot or explicit
// up-failure) are surfaced as a `failed: <msg>` line under the
// entry so a misconfigured tunnel doesn't quietly read "stopped".
// Sorted by name so output is reproducible.
func tunnelList(cfg *Config) error {
	if len(cfg.Tunnels) == 0 {
		fmt.Println("(no saved tunnels)")
		return nil
	}
	active, errs := loadTunnelStatuses()
	names := make([]string, 0, len(cfg.Tunnels))
	for n := range cfg.Tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		def := cfg.Tunnels[n]
		status := "stopped"
		listen := ""
		if a, ok := active[n]; ok {
			status = "running"
			listen = a.Listen
		} else if _, ok := errs[n]; ok {
			status = "failed"
		}
		flags := ""
		if def.Autostart {
			flags = " autostart"
		}
		profile := def.Profile
		if profile == "" {
			profile = "(default)"
		}
		line := fmt.Sprintf("%-16s %-7s %s %s  profile=%s%s",
			n, def.Type, def.Spec, status, profile, flags)
		if listen != "" {
			line += " listen=" + listen
		}
		fmt.Println(line)
		if msg, ok := errs[n]; ok {
			fmt.Printf("                 failed: %s\n", msg)
		}
	}
	return nil
}

func tunnelShow(args []string, cfg *Config) error {
	if len(args) < 1 {
		return exitErr(2, "usage: srv tunnel show <name>")
	}
	name := args[0]
	def, ok := cfg.Tunnels[name]
	if !ok {
		return exitErr(1, "tunnel %q not found", name)
	}
	fmt.Printf("name:      %s\n", name)
	fmt.Printf("type:      %s\n", def.Type)
	fmt.Printf("spec:      %s\n", def.Spec)
	if def.Profile != "" {
		fmt.Printf("profile:   %s\n", def.Profile)
	} else {
		fmt.Printf("profile:   (default at up-time)\n")
	}
	fmt.Printf("autostart: %v\n", def.Autostart)
	active, errs := loadTunnelStatuses()
	if a, ok := active[name]; ok {
		fmt.Printf("status:    running (listen=%s, started %s)\n",
			a.Listen, time.Unix(a.Started, 0).Format(time.RFC3339))
	} else if msg, ok := errs[name]; ok {
		fmt.Printf("status:    failed\n")
		fmt.Printf("error:     %s\n", msg)
	} else {
		fmt.Printf("status:    stopped\n")
	}
	return nil
}

// tunnelUp asks the daemon to bring a saved tunnel up. The daemon owns
// the goroutine so the tunnel survives this CLI process exiting.
// ensureDaemon() is called first because the daemon must be alive to
// host the tunnel.
func tunnelUp(args []string) error {
	if len(args) < 1 {
		return exitErr(2, "usage: srv tunnel up <name>")
	}
	name := args[0]
	if !ensureDaemon() {
		return exitErr(1, "could not start daemon; tunnel up needs the daemon to host the listener")
	}
	conn := daemonDial(2 * time.Second)
	if conn == nil {
		return exitErr(1, "daemon unreachable")
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "tunnel_up", Name: name}, 10*time.Second)
	if err != nil {
		return exitErr(1, "daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return exitErr(1, "tunnel up %s: %s", name, msg)
	}
	if resp.Listen != "" {
		fmt.Printf("tunnel %q up (listening on %s)\n", name, resp.Listen)
	} else {
		fmt.Printf("tunnel %q up\n", name)
	}
	return nil
}

func tunnelDown(args []string) error {
	if len(args) < 1 {
		return exitErr(2, "usage: srv tunnel down <name>")
	}
	name := args[0]
	conn := daemonDial(2 * time.Second)
	if conn == nil {
		return exitErr(1, "daemon not running (nothing to stop)")
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "tunnel_down", Name: name}, 5*time.Second)
	if err != nil {
		return exitErr(1, "daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return exitErr(1, "tunnel down %s: %s", name, msg)
	}
	fmt.Printf("tunnel %q stopped\n", name)
	return nil
}

// loadActiveTunnels asks the daemon what's currently running. Returns
// an empty map when the daemon is down (so `srv tunnel list` still
// shows definitions, just all "stopped"). Errors from prior attempts
// (autostart failures, manual `up` failures) are surfaced via
// loadTunnelErrors instead.
func loadActiveTunnels() map[string]tunnelInfo {
	active, _ := loadTunnelStatuses()
	return active
}

// loadTunnelErrors returns the daemon's "last attempt failed" map
// keyed by tunnel name. Empty when the daemon is down OR when every
// saved tunnel either started cleanly or hasn't been tried yet.
func loadTunnelErrors() map[string]string {
	_, errs := loadTunnelStatuses()
	return errs
}

// loadTunnelStatuses is the single-RPC version: one round-trip to
// the daemon yields both the active set and the error set. Callers
// that need both (CLI `tunnel list`, the UI dashboard) should prefer
// this over calling the singletons separately.
func loadTunnelStatuses() (active map[string]tunnelInfo, errs map[string]string) {
	active = map[string]tunnelInfo{}
	errs = map[string]string{}
	conn := daemonDial(500 * time.Millisecond)
	if conn == nil {
		return
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "tunnel_list"}, 2*time.Second)
	if err != nil || resp == nil || !resp.OK {
		return
	}
	for _, t := range resp.Tunnels {
		active[t.Name] = t
	}
	for n, msg := range resp.TunnelErrors {
		errs[n] = msg
	}
	return
}

// applyTunnelSpec is a tiny helper used by both the one-shot path and
// the daemon when resolving a TunnelDef into (port, host, port).
// Returns localPort/remoteHost/remotePort -- semantics flip in the
// caller based on TunnelDef.Type.
func applyTunnelSpec(spec string) (int, string, int, error) {
	return sshx.ParseTunnelSpec(spec)
}
