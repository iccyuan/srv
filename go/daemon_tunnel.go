package main

import (
	"fmt"
	"os"
	"time"
)

// Daemon-side runtime for saved tunnels. The CLI's `srv tunnel up` /
// `srv tunnel down` send tunnel_up / tunnel_down requests; we host
// the forwarder goroutine here so the tunnel survives the CLI exit.
//
// Autostart entries come up at daemon boot in a background goroutine
// so a slow / dead host can't block ls / cd / run readiness.

// handleTunnelUp resolves the TunnelDef, dials (or reuses the pooled
// client), and launches the forwarder. Idempotent on already-running:
// re-up is a no-op so MCP loops / startup scripts can call it freely.
func (s *daemonState) handleTunnelUp(req daemonRequest) daemonResponse {
	if req.Name == "" {
		return daemonResponse{OK: false, Err: "tunnel name is required"}
	}
	s.tunnelsMu.Lock()
	if existing, ok := s.tunnels[req.Name]; ok {
		listen := existing.listen
		s.tunnelsMu.Unlock()
		return daemonResponse{OK: true, Listen: listen}
	}
	s.tunnelsMu.Unlock()

	cfg, err := LoadConfig()
	if err != nil || cfg == nil {
		return daemonResponse{OK: false, Err: fmt.Sprintf("load config: %v", err)}
	}
	def, ok := cfg.Tunnels[req.Name]
	if !ok {
		return daemonResponse{OK: false, Err: fmt.Sprintf("tunnel %q not defined", req.Name)}
	}

	at, err := s.startTunnel(req.Name, def)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	return daemonResponse{OK: true, Listen: at.listen}
}

func (s *daemonState) handleTunnelDown(req daemonRequest) daemonResponse {
	if req.Name == "" {
		return daemonResponse{OK: false, Err: "tunnel name is required"}
	}
	s.tunnelsMu.Lock()
	at, ok := s.tunnels[req.Name]
	if ok {
		delete(s.tunnels, req.Name)
	}
	s.tunnelsMu.Unlock()
	if !ok {
		return daemonResponse{OK: false, Err: fmt.Sprintf("tunnel %q not running", req.Name)}
	}
	at.stop()
	// Bounded wait so a stuck forwarder can't hang the CLI -- if it
	// doesn't finish in 2s, we return OK anyway; the goroutine still
	// has to clean up its listener (cheap) and exit on its own.
	select {
	case <-at.done:
	case <-time.After(2 * time.Second):
	}
	return daemonResponse{OK: true}
}

func (s *daemonState) handleTunnelList(req daemonRequest) daemonResponse {
	s.tunnelsMu.Lock()
	out := make([]tunnelInfo, 0, len(s.tunnels))
	for _, at := range s.tunnels {
		out = append(out, tunnelInfo{
			Name:    at.name,
			Type:    at.def.Type,
			Spec:    at.def.Spec,
			Profile: at.profile,
			Listen:  at.listen,
			Started: at.startedAt.Unix(),
		})
	}
	s.tunnelsMu.Unlock()

	s.tunnelErrMu.Lock()
	errs := make(map[string]string, len(s.tunnelErr))
	for name, msg := range s.tunnelErr {
		errs[name] = msg
	}
	s.tunnelErrMu.Unlock()
	return daemonResponse{OK: true, Tunnels: out, TunnelErrors: errs}
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
func (s *daemonState) startTunnel(name string, def *TunnelDef) (at *activeTunnel, err error) {
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
	cfg, err := LoadConfig()
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
	lp, rh, rp, err := parseTunnelSpec(def.Spec)
	if err != nil {
		return nil, fmt.Errorf("tunnel %q spec: %w", name, err)
	}

	client, profile, err := s.getClient(profileName)
	if err != nil {
		return nil, fmt.Errorf("dial profile %q: %w", profileName, err)
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
		switch def.Type {
		case "remote":
			runErr = runReverseForwarder(client, lp, rh, rp, at.stopCh, nil)
		default:
			runErr = runLocalForwarder(client, lp, rh, rp, at.stopCh, nil)
		}
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "daemon: tunnel %q forwarder exited: %v\n", name, runErr)
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
	cfg, err := LoadConfig()
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
