package tunnel

import (
	"fmt"
	"os"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/sshx"
	"srv/internal/tunnelproc"
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
		return clierr.Errf(2, "usage: srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart] [--on-demand] [--independent]")
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
				return clierr.Errf(2, "%s requires a value", a)
			}
			def.Profile = rest[i+1]
			i++
		case len(a) > 10 && a[:10] == "--profile=":
			def.Profile = a[10:]
		default:
			if specSeen {
				return clierr.Errf(2, "unexpected arg %q (already have spec %q)", a, def.Spec)
			}
			if _, _, _, err := sshx.ParseTunnelSpec(a); err != nil {
				return clierr.Errf(2, "bad spec %q: %v", a, err)
			}
			def.Spec = a
			specSeen = true
		}
	}
	if !specSeen {
		return clierr.Errf(2, "missing port spec (e.g. 8080 or 5432:db:5432)")
	}
	if def.OnDemand && def.Type == "remote" {
		return clierr.Errf(2, "--on-demand is local-direction only (reverse tunnels need the SSH session up to register the remote listener)")
	}
	if def.Profile != "" {
		if _, ok := cfg.Profiles[def.Profile]; !ok {
			return clierr.Errf(1, "profile %q not found", def.Profile)
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
		return clierr.Errf(2, "usage: srv tunnel remove <name>")
	}
	name := args[0]
	if _, ok := cfg.Tunnels[name]; !ok {
		return clierr.Errf(1, "tunnel %q not found", name)
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
		return clierr.Errf(2, "usage: srv tunnel show <name>")
	}
	name := args[0]
	def, ok := cfg.Tunnels[name]
	if !ok {
		return clierr.Errf(1, "tunnel %q not found", name)
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

// cmdUp brings a saved tunnel up. Routing:
//
//   - def.Independent=true -> spawn `srv _tunnel_run <name>` as a
//     detached process. The tunnel survives daemon restarts; status
//     flows through ~/.srv/tunnels/<name>.json.
//   - otherwise            -> daemon RPC `tunnel_up`. The daemon owns
//     the goroutine so the tunnel outlives this CLI process but dies
//     with the daemon.
//
// daemon.Ensure() runs in the daemon-hosted branch only -- the
// independent path doesn't need the daemon.
func cmdUp(args []string) error {
	if len(args) < 1 {
		return clierr.Errf(2, "usage: srv tunnel up <name>")
	}
	name := args[0]
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return clierr.Errf(1, "load config: %v", err)
	}
	def, ok := cfg.Tunnels[name]
	if !ok {
		return clierr.Errf(1, "tunnel %q not defined", name)
	}
	if def.Independent {
		if err := tunnelproc.Spawn(name); err != nil {
			return clierr.Errf(1, "%v", err)
		}
		// Poll for the status file so we can report "listening on
		// <addr>" instead of leaving the user guessing whether the
		// subprocess actually came up. ~2s budget covers the SSH
		// handshake on slow links; the spawn itself is async.
		for i := 0; i < 40; i++ {
			if st, _ := tunnelproc.ReadStatus(name); st != nil {
				fmt.Printf("tunnel %q up (listening on %s, pid %d)\n", name, st.Listen, st.PID)
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Printf("tunnel %q spawned (status file not yet visible; check ~/.srv/tunnels/%s.log)\n", name, name)
		return nil
	}

	if !daemon.Ensure() {
		return clierr.Errf(1, "could not start daemon; tunnel up needs the daemon to host the listener")
	}
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		return clierr.Errf(1, "daemon unreachable")
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_up", Name: name}, 10*time.Second)
	if err != nil {
		return clierr.Errf(1, "daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return clierr.Errf(1, "tunnel up %s: %s", name, msg)
	}
	if resp.Listen != "" {
		fmt.Printf("tunnel %q up (listening on %s)\n", name, resp.Listen)
	} else {
		fmt.Printf("tunnel %q up\n", name)
	}
	return nil
}

// cmdDown stops a saved tunnel. We try the independent-process
// path FIRST regardless of what def.Independent says -- a tunnel
// may have been started independent and then had its config flag
// flipped, and we'd rather stop whichever process actually has the
// listener than leave a stray one running. The daemon RPC then runs
// as a second pass to catch the daemon-hosted case.
func cmdDown(args []string) error {
	if len(args) < 1 {
		return clierr.Errf(2, "usage: srv tunnel down <name>")
	}
	name := args[0]
	stoppedIndependent := false
	if st, _ := tunnelproc.ReadStatus(name); st != nil {
		if err := tunnelproc.Stop(name); err == nil {
			stoppedIndependent = true
		} else {
			fmt.Fprintf(os.Stderr, "srv tunnel: independent stop failed: %v\n", err)
		}
	}
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		if stoppedIndependent {
			fmt.Printf("tunnel %q stopped\n", name)
			return nil
		}
		return clierr.Errf(1, "daemon not running (nothing to stop)")
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_down", Name: name}, 5*time.Second)
	if err != nil {
		if stoppedIndependent {
			fmt.Printf("tunnel %q stopped\n", name)
			return nil
		}
		return clierr.Errf(1, "daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		if stoppedIndependent {
			// Daemon didn't know about this tunnel (it was
			// independent), but we did stop the actual process.
			fmt.Printf("tunnel %q stopped\n", name)
			return nil
		}
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return clierr.Errf(1, "tunnel down %s: %s", name, msg)
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

// LoadStatuses fans out across BOTH tunnel-hosting paths and
// returns the union: daemon-hosted entries via the daemon RPC,
// independent-process entries via ~/.srv/tunnels/*.json. Names
// collide gracefully -- independent wins because if both report a
// listener for the same name, the independent one's status file
// was written by the actually-running process; the daemon's view
// of "I started one too" is stale until its forwarder loop notices.
//
// Callers that need both (CLI `tunnel list`, the UI dashboard)
// should prefer this over reaching into either path directly.
func LoadStatuses() (active map[string]daemon.TunnelInfo, errs map[string]string) {
	active = map[string]daemon.TunnelInfo{}
	errs = map[string]string{}
	conn := daemon.DialSock(500 * time.Millisecond)
	if conn != nil {
		defer conn.Close()
		resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_list"}, 2*time.Second)
		if err == nil && resp != nil && resp.OK {
			for _, t := range resp.Tunnels {
				active[t.Name] = t
			}
			for n, msg := range resp.TunnelErrors {
				errs[n] = msg
			}
		}
	}
	// Independent-process layer: walk the status files and overlay.
	// Failures here are silent because a missing tunnels dir or an
	// unparseable status file shouldn't break `tunnel list`.
	if ind, err := tunnelproc.ListStatuses(); err == nil {
		for name, st := range ind {
			active[name] = daemon.TunnelInfo{
				Name:    name,
				Type:    st.Type,
				Spec:    st.Spec,
				Profile: st.Profile,
				Listen:  st.Listen,
				Started: st.Started,
			}
			// If both paths reported an error for this name, the
			// independent path is now considered authoritative
			// "running", so clear any stale daemon error.
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
