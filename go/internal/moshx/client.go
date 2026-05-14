package moshx

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"srv/internal/srvtty"
	"syscall"
	"time"
)

// ClientOptions configures one `srv mosh` session on the local side.
type ClientOptions struct {
	// RemoteHost is what the SSH bootstrap reported (the dial target
	// from the user's perspective; not necessarily the IP the kernel
	// will route to).
	RemoteHost string
	// RemotePort is the UDP port the server bound to (from the
	// bootstrap line).
	RemotePort int
	// Secret is the 32-byte shared secret from the bootstrap line.
	Secret []byte
	// InitialRows / InitialCols are the local terminal size at start.
	// 0 falls back to 24x80.
	InitialRows, InitialCols uint16
	// InitialBanner is opaque bytes the client wants delivered with
	// its Hello (e.g. "client=srv-mosh v=2.6.6"). Optional.
	InitialBanner []byte
}

// RunClient connects to a running `srv mosh-server` over UDP, sets
// up the local terminal in raw mode, and proxies stdio bytes both
// ways until the user exits.
//
// Returns nil on a graceful Bye from either side; non-nil on a
// transport-level failure (peer unreachable, etc.).
func RunClient(opts ClientOptions) error {
	if len(opts.Secret) != 32 {
		return fmt.Errorf("srv mosh: secret must be 32 bytes")
	}
	if opts.RemotePort == 0 || opts.RemoteHost == "" {
		return fmt.Errorf("srv mosh: remote host:port required")
	}
	remote, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", opts.RemoteHost, opts.RemotePort))
	if err != nil {
		return fmt.Errorf("resolve %s:%d: %w", opts.RemoteHost, opts.RemotePort, err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0})
	if err != nil {
		return fmt.Errorf("local bind: %w", err)
	}

	cToS, err := NewCodec(opts.Secret, ClientToServer)
	if err != nil {
		return err
	}
	sRecv, err := NewCodec(opts.Secret, ServerToClient)
	if err != nil {
		return err
	}
	t, err := NewTransport(TransportConfig{
		Conn:      conn,
		Send:      cToS,
		Recv:      sRecv,
		ExpectDir: ServerToClient,
		PeerAddr:  remote,
	})
	if err != nil {
		return err
	}
	defer t.Close()

	rows, cols := opts.InitialRows, opts.InitialCols
	if rows == 0 || cols == 0 {
		w, h := srvtty.Size()
		if rows == 0 {
			rows = uint16(h)
		}
		if cols == 0 {
			cols = uint16(w)
		}
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	// First frame: Hello with initial size embedded.
	hello := make([]byte, 4+len(opts.InitialBanner))
	hello[0] = byte(rows >> 8)
	hello[1] = byte(rows)
	hello[2] = byte(cols >> 8)
	hello[3] = byte(cols)
	copy(hello[4:], opts.InitialBanner)
	if err := t.SendHello(hello); err != nil {
		return err
	}

	// Raw mode local stdin so per-keystroke bytes flow without
	// waiting for line buffering. Restore on exit.
	restore, _ := srvtty.MakeStdinRaw()
	if restore != nil {
		defer restore()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stdin -> UDP. Read in small chunks so a slow link doesn't
	// pile keystrokes into one frame at the cost of latency.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
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

	// UDP -> stdout. Bye / lifecycle frames cancel ctx.
	go func() {
		for {
			select {
			case in, ok := <-t.Recv():
				if !ok {
					cancel()
					return
				}
				switch in.Frame.Kind {
				case KindData, KindHello:
					_, _ = os.Stdout.Write(in.Frame.Body)
				case KindBye:
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// SIGWINCH -> remote resize. Also SIGINT/SIGTERM -> graceful close.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	winchCh := registerSigwinch()

	// Periodic keepalive: a tiny ACK-only frame every 30s so a
	// firewall idle timeout doesn't drop the UDP NAT mapping. Real
	// mosh sends them every 3s; we go gentler because most NATs
	// hold UDP state for ~5 min.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = t.sendUnreliable(KindBye, nil)
			return nil
		case <-sigCh:
			cancel()
		case <-winchCh:
			w, h := srvtty.Size()
			if w > 0 && h > 0 {
				_ = t.SendWinsize(uint16(h), uint16(w))
			}
		case <-keepalive.C:
			_ = t.SendAckOnly()
		}
	}
}
