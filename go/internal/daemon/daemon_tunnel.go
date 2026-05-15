package daemon

import (
	"fmt"
	"net"
	"os"
	"srv/internal/config"
	"srv/internal/sshx"
	"strconv"
	"sync"
	"time"
)

// Daemon-side runtime for saved tunnels. The CLI's `srv tunnel up` /
// `srv tunnel down` send tunnel_up / tunnel_down requests; we host
// the forwarder goroutine here so the tunnel survives the CLI exit.
//
// Autostart entries come up at daemon boot in a background goroutine
// so a slow / dead host can't block ls / cd / run readiness.

// handleTunnelUp resolves the config.TunnelDef, dials (or reuses the pooled
// client), and launches the forwarder. Idempotent on already-running:
// re-up is a no-op so MCP loops / startup scripts can call it freely.
func (s *daemonState) handleTunnelUp(req Request) Response {
	if req.Name == "" {
		return Response{OK: false, Err: "tunnel name is required"}
	}
	s.tunnelsMu.Lock()
	if existing, ok := s.tunnels[req.Name]; ok {
		listen := existing.listen
		s.tunnelsMu.Unlock()
		return Response{OK: true, Listen: listen}
	}
	s.tunnelsMu.Unlock()

	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return Response{OK: false, Err: fmt.Sprintf("load config: %v", err)}
	}
	def, ok := cfg.Tunnels[req.Name]
	if !ok {
		return Response{OK: false, Err: fmt.Sprintf("tunnel %q not defined", req.Name)}
	}

	at, err := s.startTunnel(req.Name, def)
	if err != nil {
		return Response{OK: false, Err: err.Error()}
	}
	return Response{OK: true, Listen: at.listen}
}

func (s *daemonState) handleTunnelDown(req Request) Response {
	if req.Name == "" {
		return Response{OK: false, Err: "tunnel name is required"}
	}
	s.tunnelsMu.Lock()
	at, ok := s.tunnels[req.Name]
	if ok {
		delete(s.tunnels, req.Name)
	}
	s.tunnelsMu.Unlock()
	if !ok {
		return Response{OK: false, Err: fmt.Sprintf("tunnel %q not running", req.Name)}
	}
	at.stop()
	// Bounded wait so a stuck forwarder can't hang the CLI -- if it
	// doesn't finish in 2s, we return OK anyway; the goroutine still
	// has to clean up its listener (cheap) and exit on its own.
	select {
	case <-at.done:
	case <-time.After(2 * time.Second):
	}
	return Response{OK: true}
}

func (s *daemonState) handleTunnelList(req Request) Response {
	s.tunnelsMu.Lock()
	out := make([]TunnelInfo, 0, len(s.tunnels))
	for _, at := range s.tunnels {
		out = append(out, TunnelInfo{
			Name:     at.name,
			Type:     at.def.Type,
			Spec:     at.def.Spec,
			Profile:  at.profile,
			Listen:   at.listen,
			Started:  at.startedAt.Unix(),
			OnDemand: at.def.OnDemand,
		})
	}
	s.tunnelsMu.Unlock()

	s.tunnelErrMu.Lock()
	errs := make(map[string]string, len(s.tunnelErr))
	for name, msg := range s.tunnelErr {
		errs[name] = msg
	}
	s.tunnelErrMu.Unlock()
	return Response{OK: true, Tunnels: out, TunnelErrors: errs}
}

// recordTunnelErr / clearTunnelErr are the two operations on the
// per-tunnel last-error cache. Both take the lock briefly so they
// can be called from anywhere in the startTunnel flow.
func (s *daemonState) recordTunnelErr(name string, err error) {
	if err == nil {
		return
	}
	s.tunnelErrMu.Lock()
	s.tunnelErr[name] = err.Error()
	s.tunnelErrMu.Unlock()
}

func (s *daemonState) clearTunnelErr(name string) {
	s.tunnelErrMu.Lock()
	delete(s.tunnelErr, name)
	s.tunnelErrMu.Unlock()
}

// startTunnel resolves the profile, opens (or reuses) the SSH client,
// boots the forwarder goroutine, and registers the tunnel as active.
// Returns the *activeTunnel so the caller can read .listen for echo.
// On any failure path the error is also recorded in s.tunnelErr so
// `tunnel_list` can surface "last try failed for X" to the user;
// on success any prior recorded error is cleared.
func (s *daemonState) startTunnel(name string, def *config.TunnelDef) (at *activeTunnel, err error) {
	defer func() {
		if err != nil {
			s.recordTunnelErr(name, err)
		} else {
			s.clearTunnelErr(name)
		}
	}()

	// Resolve profile NOW so up-time errors (missing profile / bad
	// spec) surface to the user rather than getting buried in a
	// background goroutine.
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil, fmt.Errorf("load config: %v", err)
	}
	profileName := def.Profile
	if profileName == "" {
		profileName = cfg.DefaultProfile
	}
	if profileName == "" {
		return nil, fmt.Errorf("tunnel %q has no profile and no global default is set", name)
	}
	if _, ok := cfg.Profiles[profileName]; !ok {
		return nil, fmt.Errorf("tunnel %q references unknown profile %q", name, profileName)
	}
	lp, rh, rp, err := sshx.ParseTunnelSpec(def.Spec)
	if err != nil {
		return nil, fmt.Errorf("tunnel %q spec: %w", name, err)
	}

	// On-demand only applies to local-direction forwarders. Reverse
	// (`-R`) tunnels need the SSH session up to register the remote
	// listener so the laziness model doesn't apply -- reject early
	// rather than silently downgrade.
	onDemand := def.OnDemand && def.Type != "remote"

	// Eager path: dial up front so spec/profile errors surface
	// immediately. Lazy path skips this so a defined-but-rarely-used
	// tunnel costs nothing until its first client connects.
	var (
		client  *sshx.Client
		profile *config.Profile
	)
	if !onDemand {
		client, profile, err = s.getClient(profileName)
		if err != nil {
			return nil, fmt.Errorf("dial profile %q: %w", profileName, err)
		}
	} else {
		// Still resolve profile for the listen-label string (host
		// names show in `tunnel show`). The actual SSH dial happens
		// on first accept inside RunLazyLocalForwarder.
		profile = cfg.Profiles[profileName]
	}

	at = &activeTunnel{
		name:      name,
		def:       def,
		profile:   profileName,
		listen:    tunnelListenLabel(def, profile),
		startedAt: time.Now(),
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	s.tunnelsMu.Lock()
	if existing, ok := s.tunnels[name]; ok {
		// Lost a race to a concurrent up; return the winner.
		s.tunnelsMu.Unlock()
		return existing, nil
	}
	s.tunnels[name] = at
	s.tunnelsMu.Unlock()

	go func() {
		defer close(at.done)
		var runErr error
		switch {
		case onDemand:
			runErr = sshx.RunLazyLocalForwarder(lp, rh, rp, func() (*sshx.Client, error) {
				c, _, err := s.getClient(profileName)
				return c, err
			}, at.stopCh, nil)
		case def.Type == "remote":
			runErr = sshx.RunReverseForwarder(client, lp, rh, rp, at.stopCh, nil)
		default:
			runErr = sshx.RunLocalForwarder(client, lp, rh, rp, at.stopCh, nil)
		}
		// runErr is non-nil ONLY when the SSH transport died under
		// the forwarder (clean stop via stopCh returns nil). Record
		// it so `srv tunnel list` shows "failed: ssh connection
		// closed: ..." instead of the misleading "stopped" the user
		// would have seen otherwise. The pooled SSH client is also
		// likely toast at this point, so evict it from the pool --
		// otherwise the next `tunnel up` reuses a dead connection.
		if runErr != nil {
			s.recordTunnelErr(name, fmt.Errorf("forwarder crashed: %v", runErr))
			fmt.Fprintf(os.Stderr, "daemon: tunnel %q forwarder exited: %v\n", name, runErr)
			s.evictPooledClient(profileName)
		}
		// Self-deregister so list reflects "stopped" even if nobody
		// called tunnel_down (e.g. ssh connection dropped under us).
		s.tunnelsMu.Lock()
		if cur, ok := s.tunnels[name]; ok && cur == at {
			delete(s.tunnels, name)
		}
		s.tunnelsMu.Unlock()
	}()
	return at, nil
}

// startAutostartTunnels iterates config.Tunnels and brings up every
// entry flagged autostart=true. Runs in a goroutine so daemon startup
// doesn't block on slow dials; per-tunnel failures are logged.
func (s *daemonState) startAutostartTunnels() {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return
	}
	for name, def := range cfg.Tunnels {
		if !def.Autostart {
			continue
		}
		if _, err := s.startTunnel(name, def); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: autostart tunnel %q failed: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "daemon: autostarted tunnel %q\n", name)
	}
}

// startAutoconnectProfiles dials every profile flagged
// autoconnect=true into the daemon's SSH pool, in parallel, so the
// first user request against them is 0-RTT instead of paying the
// cold-handshake cost. Failures are logged but never block daemon
// startup -- a profile pointing at a down host shouldn't keep cd /
// ls / run from working against every other profile.
//
// Lives next to startAutostartTunnels because both are "act on
// config-flagged entries at daemon boot" routines; they're
// independent but share the same shape and the same don't-let-one-
// failure-block-the-rest policy.
func (s *daemonState) startAutoconnectProfiles() {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return
	}
	// Parallel dial: the whole point is to not serialize cold
	// handshakes. Two profiles taking 500ms each finish in 500ms,
	// not 1000ms. WaitGroup is only for diagnostics ("warmed N
	// profiles in T ms"); we don't actually need to wait before
	// returning since the daemon's accept loop is already running.
	var wg sync.WaitGroup
	started := time.Now()
	var attempted int
	for name, prof := range cfg.Profiles {
		if !prof.GetAutoconnect() {
			continue
		}
		attempted++
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, _, err := s.getClient(name); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: autoconnect %q failed: %v\n", name, err)
				return
			}
			fmt.Fprintf(os.Stderr, "daemon: autoconnected %q\n", name)
		}(name)
	}
	if attempted == 0 {
		return
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "daemon: autoconnect: %d profile(s) warmed in %v\n",
		attempted, time.Since(started).Round(time.Millisecond))
}

// stopAllTunnels signals every active tunnel to shut down. Used from
// closeAll() during daemon shutdown.
func (s *daemonState) stopAllTunnels() {
	s.tunnelsMu.Lock()
	all := make([]*activeTunnel, 0, len(s.tunnels))
	for _, at := range s.tunnels {
		all = append(all, at)
	}
	s.tunnels = map[string]*activeTunnel{}
	s.tunnelsMu.Unlock()
	for _, at := range all {
		at.stop()
	}
	for _, at := range all {
		select {
		case <-at.done:
		case <-time.After(time.Second):
		}
	}
}

// tunnelListenLabel formats the human-facing "I'm listening here"
// line for an active TunnelDef. Pulled out of tunnel_save so daemon
// status and CLI both format identically; called from buildTunnelInfo.
func tunnelListenLabel(def *config.TunnelDef, profile *config.Profile) string {
	lp, _, _, err := sshx.ParseTunnelSpec(def.Spec)
	if err != nil {
		return def.Spec
	}
	switch def.Type {
	case "remote":
		host := "localhost"
		if profile != nil && profile.Host != "" {
			host = profile.Host
		}
		return host + ":" + strconv.Itoa(lp)
	default:
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
	}
}
