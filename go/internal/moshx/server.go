package moshx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ServerOptions configures one `srv mosh-server` invocation. The
// client side fills in Command (the user shell or one-shot command);
// other fields take pragmatic defaults.
type ServerOptions struct {
	// Command is what the server PTY-spawns. If empty, defaults to the
	// $SHELL (then /bin/sh).
	Command []string
	// Env is passed to the spawned command; defaults to os.Environ().
	Env []string
	// BindAddr is the local UDP listen interface (default "0.0.0.0",
	// any port).
	BindAddr string
	// IdleTimeout terminates the server after this long with no
	// inbound traffic. mosh defaults to a few hours; here we use
	// 30 minutes to keep zombie servers bounded. 0 disables.
	IdleTimeout time.Duration
}

// RunServer is the entrypoint invoked when `srv mosh-server` is
// launched on the remote. It:
//
//  1. Binds a UDP socket on a random port.
//  2. Generates a 32-byte session secret.
//  3. Prints `SRV-MOSH-CONNECT <port> <hex-secret>` to stdout and
//     flushes -- the client reads this line over the SSH bootstrap.
//  4. Closes stdin/stdout (the SSH session can now end).
//  5. Waits for the first client frame -> adopts the addr, builds a
//     Transport, spawns the user command in a PTY, and pumps bytes.
//
// On unix the PTY path runs the user command interactively. On
// Windows the server stub returns an error -- only the client side
// works there.
func RunServer(opts ServerOptions) error {
	if opts.BindAddr == "" {
		opts.BindAddr = "0.0.0.0"
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(opts.BindAddr), Port: 0})
	if err != nil {
		return fmt.Errorf("bind udp: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}

	// Bootstrap line: client parses this verbatim. Stable wire format
	// (one line, space-separated, ends with \n) so future clients can
	// detect server versions cheaply.
	fmt.Fprintf(os.Stdout, "SRV-MOSH-CONNECT %d %s\n", port, hex.EncodeToString(secret))
	_ = os.Stdout.Sync()

	// Detach stdio so the parent SSH session can close cleanly while
	// the server keeps running over UDP. We don't truly daemonize
	// (no fork()) because Go's runtime makes that awkward; instead
	// we ignore SIGHUP so the shell hangup doesn't kill us.
	signal.Ignore(syscall.SIGHUP)
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		os.Stdin.Close()
		os.Stdout.Close()
		os.Stdin = devnull
		os.Stdout = devnull
	}

	// Configure the codecs. Server seals as STOC, expects CTOS on input.
	sToC, err := NewCodec(secret, ServerToClient)
	if err != nil {
		return err
	}
	cRecv, err := NewCodec(secret, ClientToServer)
	if err != nil {
		return err
	}
	t, err := NewTransport(TransportConfig{
		Conn:      conn,
		Send:      sToC,
		Recv:      cRecv,
		ExpectDir: ClientToServer,
	})
	if err != nil {
		return err
	}
	defer t.Close()

	// Wait for the client's Hello so we know the peer addr + initial
	// window size. Idle deadline keeps a never-connected server from
	// hanging around forever.
	helloTimeout := 30 * time.Second
	if opts.IdleTimeout > 0 && opts.IdleTimeout < helloTimeout {
		helloTimeout = opts.IdleTimeout
	}
	rows, cols, err := waitForHello(t, helloTimeout)
	if err != nil {
		return err
	}

	// Spawn the user's command in a PTY.
	cmdName, cmdArgs := resolveCommand(opts.Command)
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	master, proc, err := startPTYCommand(cmdName, cmdArgs, env, rows, cols)
	if err != nil {
		return err
	}
	defer master.Close()
	return runServerLoop(t, master, proc, opts.IdleTimeout)
}

// waitForHello blocks until the first Hello frame or timeout. The
// Hello body is 4 bytes: rows(uint16 BE), cols(uint16 BE).
func waitForHello(t *Transport, timeout time.Duration) (rows, cols uint16, err error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case in := <-t.Recv():
			if in.Frame.Kind == KindHello && len(in.Frame.Body) >= 4 {
				rows = uint16(in.Frame.Body[0])<<8 | uint16(in.Frame.Body[1])
				cols = uint16(in.Frame.Body[2])<<8 | uint16(in.Frame.Body[3])
				if rows == 0 {
					rows = 24
				}
				if cols == 0 {
					cols = 80
				}
				return rows, cols, nil
			}
			// Other frames before Hello: ignore (could be a retry).
		case <-deadline.C:
			return 0, 0, errors.New("srv mosh: client did not Hello within deadline")
		}
	}
}

// runServerLoop pumps PTY <-> UDP until the user command exits or
// the idle timeout fires. Three goroutines:
//
//   - master -> UDP   (PTY output to client)
//   - UDP   -> master (client keystrokes to PTY)
//   - waiter           (Process.Wait + close)
//
// Channel `done` lets any of them stop the others on first exit.
func runServerLoop(t *Transport, master *os.File, proc *exec.Cmd, idleTimeout time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PTY -> UDP.
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				// Slice copy: Send may queue this past the next loop
				// iteration that reuses `buf`.
				out := make([]byte, n)
				copy(out, buf[:n])
				if err := t.SendData(ctx, out); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// UDP -> PTY (and lifecycle frames).
	var idleTimer *time.Timer
	resetIdle := func() {
		if idleTimer != nil {
			idleTimer.Reset(idleTimeout)
		}
	}
	if idleTimeout > 0 {
		idleTimer = time.AfterFunc(idleTimeout, cancel)
	}
	go func() {
		for {
			select {
			case in, ok := <-t.Recv():
				if !ok {
					cancel()
					return
				}
				resetIdle()
				switch in.Frame.Kind {
				case KindData:
					_, _ = master.Write(in.Frame.Body)
				case KindWinsize:
					if len(in.Frame.Body) >= 4 {
						r := uint16(in.Frame.Body[0])<<8 | uint16(in.Frame.Body[1])
						c := uint16(in.Frame.Body[2])<<8 | uint16(in.Frame.Body[3])
						_ = setWinsize(master.Fd(), r, c)
						sendWinch(proc)
					}
				case KindBye:
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Reaper.
	waitErr := make(chan error, 1)
	go func() { waitErr <- proc.Wait() }()
	select {
	case err := <-waitErr:
		_ = t.sendUnreliable(KindBye, nil)
		return err
	case <-ctx.Done():
		// Idle / client Bye: kill the subprocess so we don't leak.
		if proc.Process != nil {
			_ = proc.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
}

func resolveCommand(cmd []string) (string, []string) {
	if len(cmd) > 0 {
		return cmd[0], cmd[1:]
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell, []string{"-l"}
}

// ParseBootstrapLine is the inverse of the bootstrap print: takes a
// line read from the SSH session and returns (port, secret). Used by
// the client side. Tolerates extra whitespace.
func ParseBootstrapLine(line string) (int, []byte, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "SRV-MOSH-CONNECT ") {
		return 0, nil, fmt.Errorf("not a bootstrap line: %q", line)
	}
	parts := strings.Fields(strings.TrimPrefix(line, "SRV-MOSH-CONNECT "))
	if len(parts) != 2 {
		return 0, nil, fmt.Errorf("bootstrap line malformed: %q", line)
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil || port <= 0 || port > 65535 {
		return 0, nil, fmt.Errorf("bad port in bootstrap: %q", parts[0])
	}
	secret, err := hex.DecodeString(parts[1])
	if err != nil || len(secret) != 32 {
		return 0, nil, fmt.Errorf("bad secret in bootstrap: %q", parts[1])
	}
	return port, secret, nil
}

// ReadBootstrapLine pulls one bootstrap line from r, ignoring any
// shell noise (PS1 prompts, stderr greetings) that arrives before it.
// Bounded by total bytes so a misconfigured remote can't deadlock the
// client.
func ReadBootstrapLine(r io.Reader) (int, []byte, error) {
	br := bufio.NewReader(r)
	limit := 64 * 1024 // ~64 KB of pre-banner is plenty
	consumed := 0
	for {
		line, err := br.ReadString('\n')
		consumed += len(line)
		if strings.HasPrefix(strings.TrimSpace(line), "SRV-MOSH-CONNECT ") {
			return ParseBootstrapLine(line)
		}
		if err != nil {
			return 0, nil, fmt.Errorf("bootstrap line not found before EOF (read %d bytes)", consumed)
		}
		if consumed >= limit {
			return 0, nil, fmt.Errorf("bootstrap line not found within %d bytes of remote output", limit)
		}
	}
}

// Silence unused import on platforms where some imports trim.
var _ = sync.Mutex{}
