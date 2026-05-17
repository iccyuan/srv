package tunnel

import (
	"errors"
	"fmt"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/tunnelproc"
	"time"
)

// TunnelHost is the strategy interface for "something that can host
// a saved tunnel." There are two implementations today --
// daemonHost (the long-lived daemon process parks the listener) and
// independentHost (each tunnel is its own srv _tunnel_run
// subprocess). The CLI surface (cmdUp / cmdDown / cmdList /
// LoadStatuses) talks to this interface, not to either
// implementation directly; the def.Independent flag controls which
// host cmdUp routes to.
//
// Adding a third host (e.g. a systemd-unit wrapper or a launchd
// plist generator) means adding a struct that satisfies TunnelHost
// plus an entry in allHosts; nothing in the CLI dispatch has to
// change.
type TunnelHost interface {
	// Up brings `name` up using `def`. Returns where the listener
	// landed plus the PID if this host runs the tunnel as its own
	// process. Errors are propagated verbatim -- the caller turns
	// them into user-facing messages.
	Up(name string, def *config.TunnelDef) (UpInfo, error)
	// Down stops `name`. Returning ErrNotHosted means "I don't run
	// this tunnel; ask another host" so cmdDown can fan-out across
	// hosts without each one having to special-case the others.
	Down(name string) error
	// List returns this host's view of currently-running tunnels
	// plus any persisted last-attempt errors. The active map mirrors
	// daemon.TunnelInfo so callers can union daemon-hosted +
	// independent into one map without an extra translation step.
	List() (active map[string]daemon.TunnelInfo, errs map[string]string, err error)
}

// UpInfo is what Up returns on success: the human-readable listen
// address plus an optional PID for hosts where the tunnel is its
// own process. PID=0 means "not a process-based host" (i.e. daemon-
// hosted) so the CLI knows not to print a pid number.
type UpInfo struct {
	Listen string
	PID    int
}

// ErrNotHosted is the sentinel a host returns from Down when it
// doesn't run the named tunnel -- the CLI loop treats it as "try
// the next host" rather than a real failure. Using errors.Is keeps
// host implementations free to wrap this with their own diagnostic
// context.
var ErrNotHosted = errors.New("tunnel not hosted here")

// allHosts is the iteration order for fan-out queries. Daemon
// first, independent second; LoadStatuses's overlay semantics
// ("independent wins on name collision") fall out naturally from
// the later-overwrites-earlier loop body.
func allHosts() []TunnelHost {
	return []TunnelHost{daemonHost{}, independentHost{}}
}

// hostFor picks the right strategy for bringing UP a tunnel based
// on its def. Down and List are fan-out so they don't consult this;
// only Up needs to decide before doing work.
func hostFor(def *config.TunnelDef) TunnelHost {
	if def.Independent {
		return independentHost{}
	}
	return daemonHost{}
}

// --- daemonHost --------------------------------------------------

// daemonHost routes tunnel lifecycle through the long-lived daemon
// process. The daemon owns the listener goroutine, so the tunnel
// survives the CLI exit but dies with the daemon. Tradeoff: shares
// the daemon's SSH pool (cheaper) and goes away on daemon restart
// (less isolated).
type daemonHost struct{}

func (daemonHost) Up(name string, _ *config.TunnelDef) (UpInfo, error) {
	if !daemon.Ensure() {
		return UpInfo{}, fmt.Errorf("could not start daemon; tunnel up needs the daemon to host the listener")
	}
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		return UpInfo{}, fmt.Errorf("daemon unreachable")
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_up", Name: name}, 10*time.Second)
	if err != nil {
		return UpInfo{}, fmt.Errorf("daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return UpInfo{}, fmt.Errorf("tunnel up %s: %s", name, msg)
	}
	return UpInfo{Listen: resp.Listen, PID: 0}, nil
}

func (daemonHost) Down(name string) error {
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		return fmt.Errorf("%w: daemon not running", ErrNotHosted)
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_down", Name: name}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("daemon call: %v", err)
	}
	if resp == nil || !resp.OK {
		// Daemon didn't know about this tunnel -- standard "not
		// hosted by me" signal so the fan-out loop tries other
		// hosts before declaring failure.
		return ErrNotHosted
	}
	return nil
}

func (daemonHost) List() (map[string]daemon.TunnelInfo, map[string]string, error) {
	active := map[string]daemon.TunnelInfo{}
	errs := map[string]string{}
	conn := daemon.DialSock(500 * time.Millisecond)
	if conn == nil {
		// Daemon down isn't an error from List's perspective --
		// just means there's nothing daemon-hosted to report.
		return active, errs, nil
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_list"}, 2*time.Second)
	if err != nil || resp == nil || !resp.OK {
		return active, errs, nil
	}
	for _, t := range resp.Tunnels {
		active[t.Name] = t
	}
	for n, msg := range resp.TunnelErrors {
		errs[n] = msg
	}
	return active, errs, nil
}

// --- independentHost --------------------------------------------

// independentHost spawns each tunnel as its own srv _tunnel_run
// subprocess that lives until killed, with status persisted to
// ~/.srv/tunnels/<name>.json. The tunnel survives daemon restarts
// but pays the cost of its own SSH connection + Go runtime.
type independentHost struct{}

func (independentHost) Up(name string, _ *config.TunnelDef) (UpInfo, error) {
	if err := tunnelproc.Spawn(name); err != nil {
		return UpInfo{}, err
	}
	// Poll the status file so we can return Listen + PID synchronously.
	// Same 2s budget as before the refactor; covers the SSH handshake
	// on slow links.
	for i := 0; i < 40; i++ {
		if st, _ := tunnelproc.ReadStatus(name); st != nil {
			return UpInfo{Listen: st.Listen, PID: st.PID}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Subprocess started but didn't write its status file in time.
	// Return success with empty Listen so the caller surfaces a
	// "check the log" hint rather than a hard failure.
	return UpInfo{}, nil
}

func (independentHost) Down(name string) error {
	st, _ := tunnelproc.ReadStatus(name)
	if st == nil {
		return ErrNotHosted
	}
	return tunnelproc.Stop(name)
}

func (independentHost) List() (map[string]daemon.TunnelInfo, map[string]string, error) {
	active := map[string]daemon.TunnelInfo{}
	errs := map[string]string{}
	statuses, err := tunnelproc.ListStatuses()
	if err != nil {
		return active, errs, err
	}
	for name, st := range statuses {
		active[name] = daemon.TunnelInfo{
			Name:    name,
			Type:    st.Type,
			Spec:    st.Spec,
			Profile: st.Profile,
			Listen:  st.Listen,
			Started: st.Started,
		}
	}
	return active, errs, nil
}
