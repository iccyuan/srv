package sshx

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// SSH-level TCP forwarding. Used in two places:
//
//  1. `srv tunnel <spec>` -- CLI one-shot; runs in the foreground and
//     blocks on Ctrl-C / EOF, passing a sigCh.
//  2. The daemon's saved-tunnel host -- runs many concurrent
//     forwarders, each driven by a stopCh the daemon closes on
//     `tunnel down` / shutdown. sigCh is nil there because signal
//     handling lives at the daemon level.
//
// Either forwarder returns nil when the caller stopped it cleanly
// (listener closed via stopCh / sigCh), or an error when the SSH
// transport dropped underneath -- the daemon records that as a
// last-attempt failure so `srv tunnel list` can surface it.

// RunLocalForwarder mirrors `ssh -L localPort:remoteHost:remotePort`.
// localPort: 127.0.0.1:<localPort> on the caller's machine.
// remoteHost:remotePort: dialed from the SSH server's perspective.
func RunLocalForwarder(c *Client, localPort int, remoteHost string, remotePort int, stopCh <-chan struct{}, sigCh <-chan os.Signal) error {
	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer listener.Close()

	var (
		causeMu sync.Mutex
		cause   error
	)
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
		causeMu.Lock()
		if cause == nil {
			cause = fmt.Errorf("ssh connection closed: %v", err)
		}
		causeMu.Unlock()
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
	causeMu.Lock()
	defer causeMu.Unlock()
	return cause
}

// RunLazyLocalForwarder is RunLocalForwarder for the on-demand
// daemon path: the local listener opens immediately but no SSH dial
// happens until the first accepted connection arrives. Subsequent
// accepts reuse the cached client; when its transport dies, the
// next accept triggers a fresh dial.
//
// dial() returns a usable *Client. Errors there are surfaced to the
// caller's accept loop as a log line + dropped connection, not as a
// fatal -- callers like the daemon expect the listener to stay up
// across transient dial failures so a flaky host doesn't permanently
// take the saved tunnel offline.
//
// Clean stop path (stopCh / sigCh) matches RunLocalForwarder: nil on
// caller-requested shutdown; non-nil error reserved for things that
// would constitute a "broken tunnel" status in `srv tunnel list`.
func RunLazyLocalForwarder(localPort int, remoteHost string, remotePort int, dial func() (*Client, error), stopCh <-chan struct{}, sigCh <-chan os.Signal) error {
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

	var (
		clientMu sync.Mutex
		client   *Client
	)
	// getOrDial returns a usable client, dialling if no current one or
	// if the cached client's transport has died. Serialized so two
	// near-simultaneous accepts don't race two parallel dials.
	getOrDial := func() (*Client, error) {
		clientMu.Lock()
		defer clientMu.Unlock()
		if client != nil {
			return client, nil
		}
		c, err := dial()
		if err != nil {
			return nil, err
		}
		client = c
		// Drop the cached reference when the transport dies so the
		// next accept reaches for a fresh dial instead of using a
		// half-dead handle.
		go func(c *Client) {
			_ = c.Conn.Wait()
			clientMu.Lock()
			if client == c {
				client = nil
			}
			clientMu.Unlock()
		}(c)
		return c, nil
	}

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
			c, err := getOrDial()
			if err != nil {
				fmt.Fprintf(os.Stderr, "srv tunnel (on-demand): dial: %v\n", err)
				return
			}
			remote, err := c.Conn.Dial("tcp", remoteAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "srv tunnel (on-demand): remote dial %s: %v\n", remoteAddr, err)
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

// RunReverseForwarder mirrors `ssh -R remotePort:localHost:localPort`.
// The remote side listens; local dials happen per accepted connection.
// Same nil-on-clean-stop / err-on-ssh-drop contract as
// RunLocalForwarder, so the daemon's `tunnel forwarder exited` path
// can record the transport cause.
func RunReverseForwarder(c *Client, remotePort int, localHost string, localPort int, stopCh <-chan struct{}, sigCh <-chan os.Signal) error {
	remoteListen := net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort))
	listener, err := c.Conn.Listen("tcp", remoteListen)
	if err != nil {
		return fmt.Errorf("remote listen %s: %w", remoteListen, err)
	}
	defer listener.Close()

	var (
		causeMu sync.Mutex
		cause   error
	)
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
		causeMu.Lock()
		if cause == nil {
			cause = fmt.Errorf("ssh connection closed: %v", err)
		}
		causeMu.Unlock()
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
	causeMu.Lock()
	defer causeMu.Unlock()
	return cause
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

// ParseTunnelSpec turns "8080" / "8080:9090" / "8080:host:9090" into
// its components. Anything else is an error.
func ParseTunnelSpec(spec string) (localPort int, remoteHost string, remotePort int, err error) {
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
