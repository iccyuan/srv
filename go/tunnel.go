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

// cmdTunnel runs a local-to-remote TCP forwarder over the SSH connection
// (the ssh -L equivalent). Spec forms:
//
//	N             local 127.0.0.1:N -> remote 127.0.0.1:N
//	L:R           local 127.0.0.1:L -> remote 127.0.0.1:R
//	L:host:R      local 127.0.0.1:L -> host:R   (resolved on the remote side)
//
// Stops cleanly on SIGINT/SIGTERM, or when the underlying ssh connection
// drops -- whichever comes first.
func cmdTunnel(args []string, cfg *Config, profileOverride string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv tunnel <localPort>[:[<remoteHost>:]<remotePort>]")
		fmt.Fprintln(os.Stderr, "  srv tunnel 8080            # local 8080 -> remote 127.0.0.1:8080")
		fmt.Fprintln(os.Stderr, "  srv tunnel 8080:9090       # local 8080 -> remote 127.0.0.1:9090")
		fmt.Fprintln(os.Stderr, "  srv tunnel 8080:db:5432    # local 8080 -> db:5432 (resolved on remote)")
		return 2
	}
	lp, rh, rp, err := parseTunnelSpec(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel:", err)
		return 2
	}

	_, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return 255
	}
	defer c.Close()

	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv tunnel: listen:", err)
		return 1
	}
	defer listener.Close()

	fmt.Fprintf(os.Stderr,
		"srv tunnel: 127.0.0.1:%d -> %s -> %s:%d   (Ctrl-C to stop)\n",
		lp, profile.Host, rh, rp,
	)

	stopOnce := sync.Once{}
	stop := func() { stopOnce.Do(func() { _ = listener.Close() }) }

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nsrv tunnel: stopping.")
		stop()
	}()

	// If the SSH connection drops, every subsequent c.Conn.Dial fails;
	// surface that and stop the listener so the user notices.
	go func() {
		err := c.Conn.Wait()
		fmt.Fprintf(os.Stderr, "srv tunnel: ssh connection closed: %v\n", err)
		stop()
	}()

	remoteAddr := net.JoinHostPort(rh, strconv.Itoa(rp))
	var wg sync.WaitGroup
	for {
		local, err := listener.Accept()
		if err != nil {
			break // listener closed
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
	return 0
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
