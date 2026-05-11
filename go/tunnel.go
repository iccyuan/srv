package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// cmdTunnel dispatches between three forms:
//
//	srv tunnel <spec>           -- legacy one-shot, blocks until Ctrl-C
//	srv tunnel -R <spec>        -- same, reverse direction
//	srv tunnel <action> [args]  -- saved-tunnel management (add/up/down/list/show/remove)
//
// The action keywords are chosen to never collide with valid port specs
// (which start with a digit or `-R`), so the old form keeps working
// without a flag.
func cmdTunnel(args []string, cfg *Config, profileOverride string) error {
	if len(args) == 0 {
		printTunnelUsage()
		return exitCode(2)
	}
	switch args[0] {
	case "add":
		return tunnelAdd(args[1:], cfg, profileOverride)
	case "remove", "rm":
		return tunnelRemove(args[1:], cfg)
	case "list", "ls":
		return tunnelList(cfg)
	case "show":
		return tunnelShow(args[1:], cfg)
	case "up":
		return tunnelUp(args[1:])
	case "down":
		return tunnelDown(args[1:])
	}
	return cmdTunnelOneShot(args, cfg, profileOverride)
}

func printTunnelUsage() {
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

// cmdTunnelOneShot is the legacy `srv tunnel <spec>` foreground form.
// Kept verbatim so existing muscle memory keeps working; the new saved-
// tunnel surface is opt-in via the `add` / `up` action keywords.
func cmdTunnelOneShot(args []string, cfg *Config, profileOverride string) error {
	reverse := false
	if args[0] == "-R" || args[0] == "--reverse" {
		reverse = true
		args = args[1:]
	}
	if len(args) == 0 {
		printTunnelUsage()
		return exitCode(2)
	}
	lp, rh, rp, err := parseTunnelSpec(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel:", err)
		return exitCode(2)
	}

	_, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
	}
	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return exitCode(255)
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
		runErr = runReverseForwarder(c, lp, rh, rp, stopCh, sigCh)
	} else {
		listenLabel = net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
		fmt.Fprintf(os.Stderr,
			"srv tunnel: %s -> %s -> %s:%d   (Ctrl-C to stop)\n",
			listenLabel, profile.Host, rh, rp,
		)
		runErr = runLocalForwarder(c, lp, rh, rp, stopCh, sigCh)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel:", runErr)
		return exitCode(1)
	}
	return nil
}

// runLocalForwarder is the body of a local-to-remote forwarder, shared
// by the one-shot CLI and the daemon-hosted persistent tunnel. Returns
// when the listener closes or the ssh connection drops.
//
// stopCh: external close signal (daemon shutdown / explicit `tunnel
// down`). sigCh: optional; nil under daemon, set under CLI to allow
// Ctrl-C to break the accept loop.
func runLocalForwarder(c *Client, localPort int, remoteHost string, remotePort int, stopCh <-chan struct{}, sigCh <-chan os.Signal) error {
	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer listener.Close()

	stopOnce := sync.Once{}
	stop := func() { stopOnce.Do(func() { _ = listener.Close() }) }
	go func() {
		select {
		case <-stopCh:
			stop()
		case <-sigChOrNil(sigCh):
			stop()
		}
	}()
	go func() {
		err := c.Conn.Wait()
		fmt.Fprintf(os.Stderr, "srv tunnel: ssh connection closed: %v\n", err)
		stop()
	}()

	remoteAddr := net.JoinHostPort(remoteHost, strconv.Itoa(remotePort))
	var wg sync.WaitGroup
	for {
		local, err := listener.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func(local net.Conn) {
			defer wg.Done()
			defer local.Close()
			remote, err := c.Conn.Dial("tcp", remoteAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "srv tunnel: remote dial %s: %v\n", remoteAddr, err)
				return
			}
			defer remote.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			<-done
		}(local)
	}
	wg.Wait()
	return nil
}

// runReverseForwarder mirrors runLocalForwarder for the -R direction:
// the remote side does the listen, local dials happen per accepted
// connection.
func runReverseForwarder(c *Client, remotePort int, localHost string, localPort int, stopCh <-chan struct{}, sigCh <-chan os.Signal) error {
	remoteListen := net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort))
	listener, err := c.Conn.Listen("tcp", remoteListen)
	if err != nil {
		return fmt.Errorf("remote listen %s: %w", remoteListen, err)
	}
	defer listener.Close()

	stopOnce := sync.Once{}
	stop := func() { stopOnce.Do(func() { _ = listener.Close() }) }
	go func() {
		select {
		case <-stopCh:
			stop()
		case <-sigChOrNil(sigCh):
			stop()
		}
	}()
	go func() {
		err := c.Conn.Wait()
		fmt.Fprintf(os.Stderr, "srv tunnel -R: ssh connection closed: %v\n", err)
		stop()
	}()

	localAddr := net.JoinHostPort(localHost, strconv.Itoa(localPort))
	var wg sync.WaitGroup
	for {
		remote, err := listener.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func(remote net.Conn) {
			defer wg.Done()
			defer remote.Close()
			local, err := net.Dial("tcp", localAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "srv tunnel -R: local dial %s: %v\n", localAddr, err)
				return
			}
			defer local.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			<-done
		}(remote)
	}
	wg.Wait()
	return nil
}

// sigChOrNil returns the channel directly when non-nil; otherwise a
// channel that never fires. Lets the same select-block work in both
// CLI mode (with SIGINT/SIGTERM) and daemon mode (where signals are
// already handled at the daemon level).
func sigChOrNil(sigCh <-chan os.Signal) <-chan os.Signal {
	if sigCh != nil {
		return sigCh
	}
	return nil
}

// parseTunnelSpec turns "8080" / "8080:9090" / "8080:host:9090" into its
// components. Anything else is an error.
func parseTunnelSpec(spec string) (localPort int, remoteHost string, remotePort int, err error) {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		p, e := strconv.Atoi(parts[0])
		if e != nil || p <= 0 || p > 65535 {
			return 0, "", 0, fmt.Errorf("port not a valid number: %q", parts[0])
		}
		return p, "127.0.0.1", p, nil
	case 2:
		lp, e1 := strconv.Atoi(parts[0])
		rp, e2 := strconv.Atoi(parts[1])
		if e1 != nil || lp <= 0 || lp > 65535 {
			return 0, "", 0, fmt.Errorf("local port not valid: %q", parts[0])
		}
		if e2 != nil || rp <= 0 || rp > 65535 {
			return 0, "", 0, fmt.Errorf("remote port not valid: %q", parts[1])
		}
		return lp, "127.0.0.1", rp, nil
	case 3:
		lp, e1 := strconv.Atoi(parts[0])
		rp, e2 := strconv.Atoi(parts[2])
		if e1 != nil || lp <= 0 || lp > 65535 {
			return 0, "", 0, fmt.Errorf("local port not valid: %q", parts[0])
		}
		if e2 != nil || rp <= 0 || rp > 65535 {
			return 0, "", 0, fmt.Errorf("remote port not valid: %q", parts[2])
		}
		if parts[1] == "" {
			return 0, "", 0, fmt.Errorf("empty remote host in %q", spec)
		}
		return lp, parts[1], rp, nil
	default:
		return 0, "", 0, fmt.Errorf("expected port, lp:rp, or lp:host:rp, got %q", spec)
	}
}
