package tunnel

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"srv/internal/config"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strconv"
	"syscall"
)

// Cmd dispatches between three forms:
//
//	srv tunnel <spec>           -- legacy one-shot, blocks until Ctrl-C
//	srv tunnel -R <spec>        -- same, reverse direction
//	srv tunnel <action> [args]  -- saved-tunnel management (add/up/down/list/show/remove)
//
// The action keywords are chosen to never collide with valid port specs
// (which start with a digit or `-R`), so the old form keeps working
// without a flag.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		printUsage()
		return srvutil.Code(2)
	}
	switch args[0] {
	case "add":
		return cmdAdd(args[1:], cfg, profileOverride)
	case "remove", "rm":
		return cmdRemove(args[1:], cfg)
	case "list", "ls":
		return cmdList(cfg)
	case "show":
		return cmdShow(args[1:], cfg)
	case "up":
		return cmdUp(args[1:])
	case "down":
		return cmdDown(args[1:])
	}
	return cmdOneShot(args, cfg, profileOverride)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Forms:")
	fmt.Fprintln(os.Stderr, "  srv tunnel [-R] <port-spec>           one-shot (blocks until Ctrl-C)")
	fmt.Fprintln(os.Stderr, "  srv tunnel add <name> [-R] <spec> [-P <profile>] [--autostart]")
	fmt.Fprintln(os.Stderr, "  srv tunnel up <name>                  start a saved tunnel (via daemon)")
	fmt.Fprintln(os.Stderr, "  srv tunnel down <name>                stop a running tunnel")
	fmt.Fprintln(os.Stderr, "  srv tunnel list                       list saved tunnels + status")
	fmt.Fprintln(os.Stderr, "  srv tunnel show <name>                show one tunnel's definition")
	fmt.Fprintln(os.Stderr, "  srv tunnel remove <name>              delete a saved tunnel")
	fmt.Fprintln(os.Stderr, "Port spec:")
	fmt.Fprintln(os.Stderr, "  N             local 127.0.0.1:N -> remote 127.0.0.1:N")
	fmt.Fprintln(os.Stderr, "  L:R           local 127.0.0.1:L -> remote 127.0.0.1:R")
	fmt.Fprintln(os.Stderr, "  L:host:R      local 127.0.0.1:L -> host:R (resolved on remote)")
}

// cmdOneShot is the legacy `srv tunnel <spec>` foreground form.
// Kept verbatim so existing muscle memory keeps working; the new saved-
// tunnel surface is opt-in via the `add` / `up` action keywords.
func cmdOneShot(args []string, cfg *config.Config, profileOverride string) error {
	reverse := false
	if args[0] == "-R" || args[0] == "--reverse" {
		reverse = true
		args = args[1:]
	}
	if len(args) == 0 {
		printUsage()
		return srvutil.Code(2)
	}
	lp, rh, rp, err := sshx.ParseTunnelSpec(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel:", err)
		return srvutil.Code(2)
	}

	_, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return srvutil.Code(1)
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srv tunnel: ssh dial %s: %v\n", profile.Host, err)
		return srvutil.Code(255)
	}
	defer c.Close()

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nsrv tunnel: stopping.")
		case <-stopCh:
		}
	}()

	var listenLabel string
	var runErr error
	if reverse {
		listenLabel = fmt.Sprintf("%s:127.0.0.1:%d", profile.Host, lp)
		fmt.Fprintf(os.Stderr,
			"srv tunnel -R: %s -> local %s   (Ctrl-C to stop)\n",
			listenLabel, net.JoinHostPort(rh, strconv.Itoa(rp)),
		)
		runErr = sshx.RunReverseForwarder(c, lp, rh, rp, stopCh, sigCh)
	} else {
		listenLabel = net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
		fmt.Fprintf(os.Stderr,
			"srv tunnel: %s -> %s -> %s:%d   (Ctrl-C to stop)\n",
			listenLabel, profile.Host, rh, rp,
		)
		runErr = sshx.RunLocalForwarder(c, lp, rh, rp, stopCh, sigCh)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel:", runErr)
		return srvutil.Code(1)
	}
	return nil
}
