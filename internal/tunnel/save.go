package tunnel

import (
	"errors"
	"fmt"
	"sort"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"time"
)

// Saved-tunnel management: persists named tunnel definitions in
// Config.Tunnels (handled here) and talks to the daemon to bring them
// up / down (the daemon owns the actual goroutines so the tunnel
// outlives the CLI process).

// cmdAdd parses the same arg shape as one-shot `srv tunnel` plus a
// leading <name> and optional --autostart / --on-demand / --independent,
// then saves the result.
//
//	srv tunnel add db -L 5432:db.internal:5432 -P prod --autostart
//	srv tunnel add web 8080
//	srv tunnel add api 8080 --on-demand
//	srv tunnel add critical 5432:db:5432 --independent
func cmdAdd(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) < 2 {
		return srvutil.Errf(2, "usage: srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart] [--on-demand] [--independent]")
	}
	name := args[0]
	rest := args[1:]

	def := &config.TunnelDef{Type: "local"}
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
		case a == "--on-demand":
			def.OnDemand = true
		case a == "--independent":
			def.Independent = true
		case a == "-P" || a == "--profile":
			if i+1 >= len(rest) {
				return srvutil.Errf(2, "%s requires a value", a)
			}
			def.Profile = rest[i+1]
			i++
		case len(a) > 10 && a[:10] == "--profile=":
			def.Profile = a[10:]
		default:
			if specSeen {
				return srvutil.Errf(2, "unexpected arg %q (already have spec %q)", a, def.Spec)
			}
			if _, _, _, err := sshx.ParseTunnelSpec(a); err != nil {
				return srvutil.Errf(2, "bad spec %q: %v", a, err)
			}
			def.Spec = a
			specSeen = true
		}
	}
	if !specSeen {
		return srvutil.Errf(2, "missing port spec (e.g. 8080 or 5432:db:5432)")
	}
	if def.OnDemand && def.Type == "remote" {
		return srvutil.Errf(2, "--on-demand is local-direction only (reverse tunnels need the SSH session up to register the remote listener)")
	}
	if def.Profile != "" {
		if _, ok := cfg.Profiles[def.Profile]; !ok {
			return srvutil.Errf(1, "profile %q not found", def.Profile)
		}
	}

	if cfg.Tunnels == nil {
		cfg.Tunnels = map[string]*config.TunnelDef{}
	}
	cfg.Tunnels[name] = def
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("tunnel %q saved (%s %s", name, def.Type, def.Spec)
	if def.Profile != "" {
		fmt.Printf(", profile=%s", def.Profile)
	}
	if def.Autostart {
		fmt.Print(", autostart")
	}
	if def.OnDemand {
		fmt.Print(", on-demand")
	}
	if def.Independent {
		fmt.Print(", independent")
	}
	fmt.Println(")")
	return nil
}

func cmdRemove(args []string, cfg *config.Config) error {
	if len(args) < 1 {
		return srvutil.Errf(2, "usage: srv tunnel remove <name>")
	}
	name := args[0]
	if _, ok := cfg.Tunnels[name]; !ok {
		return srvutil.Errf(1, "tunnel %q not found", name)
	}
	delete(cfg.Tunnels, name)
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("tunnel %q removed (consider `srv tunnel down %s` to stop a running instance)\n", name, name)
	return nil
}

// cmdList prints saved tunnels and overlays daemon status so the
// user sees in one shot which are defined and which are actually
// running. Last-attempt errors (autostart-on-boot or explicit
// up-failure) are surfaced as a `failed: <msg>` line under the
// entry so a misconfigured tunnel doesn't quietly read "stopped".
// Sorted by name so output is reproducible.
func cmdList(cfg *config.Config) error {
	if len(cfg.Tunnels) == 0 {
		fmt.Println("(no saved tunnels)")
		return nil
	}
	active, errs := LoadStatuses()
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
			flags += " autostart"
		}
		if def.OnDemand {
			flags += " on-demand"
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

func cmdShow(args []string, cfg *config.Config) error {
	if len(args) < 1 {
		return srvutil.Errf(2, "usage: srv tunnel show <name>")
	}
	name := args[0]
	def, ok := cfg.Tunnels[name]
	if !ok {
		return srvutil.Errf(1, "tunnel %q not found", name)
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
	fmt.Printf("on-demand: %v\n", def.OnDemand)
	active, errs := LoadStatuses()
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

// cmdUp brings a saved tunnel up by delegating to whichever
// TunnelHost the def selects. The host returns a UpInfo describing
// where the listener landed; we render it for the user.
func cmdUp(args []string) error {
	if len(args) < 1 {
		return srvutil.Errf(2, "usage: srv tunnel up <name>")
	}
	name := args[0]
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return srvutil.Errf(1, "load config: %v", err)
	}
	def, ok := cfg.Tunnels[name]
	if !ok {
		return srvutil.Errf(1, "tunnel %q not defined", name)
	}
	info, err := hostFor(def).Up(name, def)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	if info.Listen != "" && info.PID > 0 {
		fmt.Printf("tunnel %q up (listening on %s, pid %d)\n", name, info.Listen, info.PID)
	} else if info.Listen != "" {
		fmt.Printf("tunnel %q up (listening on %s)\n", name, info.Listen)
	} else {
		fmt.Printf("tunnel %q spawned (status file not yet visible; check ~/.srv/tunnels/%s.log)\n", name, name)
	}
	return nil
}

// cmdDown stops a saved tunnel by fanning out across every
// TunnelHost: at least one of them runs the tunnel; the others
// return ErrNotHosted and we treat that as "ask the next host."
// This handles the edge case where def.Independent flipped between
// up and down -- the actually-running host gets to stop the
// listener even if the current def disagrees about where it should
// be hosted.
func cmdDown(args []string) error {
	if len(args) < 1 {
		return srvutil.Errf(2, "usage: srv tunnel down <name>")
	}
	name := args[0]
	stopped := false
	var lastErr error
	for _, h := range allHosts() {
		err := h.Down(name)
		if err == nil {
			stopped = true
			continue
		}
		if errors.Is(err, ErrNotHosted) {
			continue
		}
		// Real failure from a host that DID claim ownership. Hold
		// onto it -- we'll surface this only if no other host
		// managed to stop the tunnel.
		lastErr = err
	}
	if !stopped {
		if lastErr != nil {
			return srvutil.Errf(1, "tunnel down %s: %v", name, lastErr)
		}
		return srvutil.Errf(1, "tunnel %q not running anywhere", name)
	}
	fmt.Printf("tunnel %q stopped\n", name)
	return nil
}

// LoadActive asks the daemon what's currently running. Returns
// an empty map when the daemon is down (so `srv tunnel list` still
// shows definitions, just all "stopped"). Errors from prior attempts
// (autostart failures, manual `up` failures) are surfaced via
// LoadErrors instead.
func LoadActive() map[string]daemon.TunnelInfo {
	active, _ := LoadStatuses()
	return active
}

// LoadErrors returns the daemon's "last attempt failed" map
// keyed by tunnel name. Empty when the daemon is down OR when every
// saved tunnel either started cleanly or hasn't been tried yet.
func LoadErrors() map[string]string {
	_, errs := LoadStatuses()
	return errs
}

// LoadStatuses unions every TunnelHost's view into a single
// (active, errs) pair. Iteration order in allHosts() determines
// overlay semantics: later hosts overwrite earlier ones on name
// collision, so independent (returned second by allHosts) wins
// over daemon -- the independent path's status file was written by
// the actually-running process, while the daemon's view of "I
// started one too" could be stale until its forwarder loop notices.
//
// Per-host List errors are not bubbled up because the function's
// contract is "best-effort enumeration"; a host that's temporarily
// down (daemon not running) is just absent from the result, not a
// hard failure for the whole call.
func LoadStatuses() (active map[string]daemon.TunnelInfo, errs map[string]string) {
	active = map[string]daemon.TunnelInfo{}
	errs = map[string]string{}
	for _, h := range allHosts() {
		a, e, _ := h.List()
		for k, v := range a {
			active[k] = v
		}
		for n, msg := range e {
			errs[n] = msg
		}
		// Drop any error entry for a name that's now active under
		// this (or any prior) host -- "currently up" beats "last
		// attempt failed" in the list view.
		for name := range a {
			delete(errs, name)
		}
	}
	return
}

// ApplySpec is a tiny helper used by both the one-shot path and
// the daemon when resolving a config.TunnelDef into (port, host, port).
// Returns localPort/remoteHost/remotePort -- semantics flip in the
// caller based on config.TunnelDef.Type.
func ApplySpec(spec string) (int, string, int, error) {
	return sshx.ParseTunnelSpec(spec)
}
