package tunnelproc

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"srv/internal/config"
	"srv/internal/sshx"
	"strconv"
	"syscall"
	"time"
)

// Run is the body of the `srv _tunnel_run <name>` hidden subcommand.
// One process == one tunnel: this function dials its own SSH client,
// stands up the local or reverse forwarder, writes the status file
// so the CLI knows we're alive, and stays running until SIGTERM (or
// the SSH transport dies).
//
// Errors are returned so the parent CLI -- in the rare case where it
// actually waits -- can surface them. In production the parent spawns
// us detached and never reads our exit code; the subprocess's stderr
// goes to ~/.srv/tunnels/<name>.log.
func Run(name string) error {
	def, err := DefFromConfig(name)
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	if cfg == nil {
		return fmt.Errorf("load config: nil")
	}
	profileName := def.Profile
	if profileName == "" {
		profileName = cfg.DefaultProfile
	}
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return fmt.Errorf("profile %q not defined", profileName)
	}
	profile.Name = profileName

	lp, rh, rp, err := sshx.ParseTunnelSpec(def.Spec)
	if err != nil {
		return fmt.Errorf("parse spec %q: %v", def.Spec, err)
	}

	// Dial our own SSH client. Independent tunnels deliberately do
	// NOT share the daemon's pool -- the whole point is to survive
	// daemon restarts, so our SSH connection lives and dies with
	// THIS process, not the daemon's lifecycle.
	client, err := sshx.Dial(profile)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %v", profile.Host, err)
	}
	defer client.Close()

	// Compose the listen-label string the daemon-hosted path uses
	// so `srv tunnel list` reads the same for both paths. For
	// reverse tunnels the "listen" is on the remote, so we render
	// "<host>:127.0.0.1:<localPort>".
	var listen string
	switch def.Type {
	case "remote":
		listen = fmt.Sprintf("%s:127.0.0.1:%d", profile.Host, lp)
	default:
		listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
	}

	st := Status{
		Name:    name,
		Type:    def.Type,
		Spec:    def.Spec,
		Profile: profileName,
		Listen:  listen,
		Started: time.Now().Unix(),
		PID:     os.Getpid(),
	}
	if err := WriteStatus(st); err != nil {
		return fmt.Errorf("write status: %v", err)
	}
	defer RemoveStatus(name)

	// Signal handling: SIGTERM (Unix) or process kill (Windows) ends
	// the forwarder. We catch SIGTERM/SIGINT here so the Unix path
	// gets a clean shutdown that runs the deferred RemoveStatus.
	// Windows can't deliver SIGTERM to a no-console detached process;
	// Stop() handles the file cleanup over there.
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		close(stopCh)
	}()

	// Forwarder kind is local vs reverse; on-demand only applies to
	// local. Independent tunnels can be on-demand the same way the
	// daemon-hosted ones can.
	onDemand := def.OnDemand && def.Type == "local"
	var runErr error
	switch {
	case onDemand:
		runErr = sshx.RunLazyLocalForwarder(lp, rh, rp, func() (*sshx.Client, error) {
			// On lazy reconnect, re-dial -- our held client may have
			// died asleep. Most of the time the held one is fine and
			// the forwarder reuses it; this callback only fires on
			// first accept after a transport failure.
			return sshx.Dial(profile)
		}, stopCh, nil)
	case def.Type == "remote":
		runErr = sshx.RunReverseForwarder(client, lp, rh, rp, stopCh, nil)
	default:
		runErr = sshx.RunLocalForwarder(client, lp, rh, rp, stopCh, nil)
	}
	if runErr != nil {
		// Don't treat clean-shutdown listener-closed errors as
		// fatal; only surface transport-level failures. The exact
		// shape of "clean" depends on the forwarder impl, so we
		// log unconditionally and let the caller (the subprocess's
		// log file) sort it out.
		return fmt.Errorf("forwarder exited: %v", runErr)
	}
	return nil
}
