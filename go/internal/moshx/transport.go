package moshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Transport is the reliable, roaming-tolerant duplex stream sitting
// on top of a single UDP socket and a peer Codec pair. Per side:
//
//   - Send(payload, kind)        -> queues + transmits, may retransmit
//   - Recv() <-chan inbound      -> in-order delivery of remote frames
//   - SetPeerAddr(addr)          -> server adopts roamed client addr
//   - Close()                    -> sends Bye, drains
//
// Reliability: every outbound non-ACK frame is held in sendBuf until
// its seq is ACKed by the peer (peer.Ack >= seq). The retransmitter
// resends entries older than RTO (200ms initial, doubles to 2s cap).
//
// In-order delivery: receiver buffers out-of-order seqs until the gap
// fills, then delivers contiguously. Duplicate seqs are silently
// dropped at the receiver.
//
// Roaming: the receiver adopts whichever remote UDP address sent the
// most recent successfully-decrypted frame. So if the client's NAT
// rebinds or it switches Wi-Fi → cellular, the server replies to the
// new address starting on the next outbound frame. The encrypted
// frame's authenticity (GCM tag) is the security check that lets us
// trust an unknown source addr at all.

// Inbound is what Recv() yields: just the decrypted Frame.
// Out-of-order delivery is handled inside Transport so the consumer
// always sees frames in sender-order.
type Inbound struct {
	Frame *Frame
}

// Transport bundles the encrypted UDP socket with the reliability
// state machine. Use NewClientTransport / NewServerTransport (sugar)
// or NewTransport (full control).
type Transport struct {
	conn      *net.UDPConn
	sendCodec *Codec
	recvCodec *Codec
	expectDir Direction // direction we expect on inbound

	mu          sync.Mutex
	peerAddr    *net.UDPAddr
	sendSeq     uint64
	sendBuf     map[uint64]*pendingFrame
	recvExpect  uint64            // next seq to deliver in-order
	recvBuf     map[uint64]*Frame // out-of-order holding pen
	highestPeer uint64            // highest peer seq we've delivered
	closed      bool
	rttSmooth   time.Duration

	inCh chan Inbound
	done chan struct{}

	rxBudget int // max in-flight unacked frames (back-pressure)
}

type pendingFrame struct {
	payload   []byte // sealed wire bytes
	kind      FrameKind
	createdAt time.Time
	lastSent  time.Time
	nextRTO   time.Duration
}

// TransportConfig is the dependency bag for NewTransport. Most fields
// have sensible defaults; the caller mainly has to supply the socket
// and the two codecs.
type TransportConfig struct {
	Conn       *net.UDPConn
	Send       *Codec
	Recv       *Codec
	ExpectDir  Direction     // peer's direction tag
	PeerAddr   *net.UDPAddr  // initial peer addr (server may leave nil)
	RXBudget   int           // max unacked in-flight; default 64
	InitialRTO time.Duration // default 200ms
	MaxRTO     time.Duration // default 2s
	InBufSize  int           // recv channel size; default 256
}

// NewTransport wires up the goroutines. Two run forever (until Close):
// the receiver (UDP read → decrypt → in-order deliver) and the
// retransmitter (periodic check of sendBuf for stale entries).
func NewTransport(cfg TransportConfig) (*Transport, error) {
	if cfg.Conn == nil || cfg.Send == nil || cfg.Recv == nil {
		return nil, errors.New("moshx: conn / send / recv required")
	}
	if cfg.RXBudget <= 0 {
		cfg.RXBudget = 64
	}
	if cfg.InitialRTO <= 0 {
		cfg.InitialRTO = 200 * time.Millisecond
	}
	if cfg.MaxRTO <= 0 {
		cfg.MaxRTO = 2 * time.Second
	}
	if cfg.InBufSize <= 0 {
		cfg.InBufSize = 256
	}
	t := &Transport{
		conn:      cfg.Conn,
		sendCodec: cfg.Send,
		recvCodec: cfg.Recv,
		expectDir: cfg.ExpectDir,
		peerAddr:  cfg.PeerAddr,
		sendBuf:   map[uint64]*pendingFrame{},
		recvBuf:   map[uint64]*Frame{},
		inCh:      make(chan Inbound, cfg.InBufSize),
		done:      make(chan struct{}),
		rxBudget:  cfg.RXBudget,
		rttSmooth: cfg.InitialRTO,
	}
	go t.receiver()
	go t.retransmitter(cfg.InitialRTO, cfg.MaxRTO)
	return t, nil
}

// Recv returns the inbound channel. Consumers must drain it; a
// blocked consumer eventually back-pressures the receiver
// (in-channel full → frames stay in recvBuf → peer's send window
// fills via unacked → peer stalls).
func (t *Transport) Recv() <-chan Inbound { return t.inCh }

// PeerAddr is the current adopted remote endpoint, or nil before the
// first inbound frame on a server-side Transport.
func (t *Transport) PeerAddr() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peerAddr
}

// Send queues a frame for transmission. Blocks (with a context-aware
// poll) if the unacked-window is full; this is the back-pressure
// signal user code uses to slow down.
func (t *Transport) Send(ctx context.Context, kind FrameKind, body []byte) error {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return errors.New("moshx: transport closed")
		}
		if len(t.sendBuf) < t.rxBudget {
			seq := t.sendSeq + 1
			t.sendSeq = seq
			ack := t.highestPeer
			f := &Frame{Seq: seq, Ack: ack, Kind: kind, Body: body}
			payload, err := t.sendCodec.Seal(f)
			if err != nil {
				t.mu.Unlock()
				return err
			}
			pf := &pendingFrame{
				payload:   payload,
				kind:      kind,
				createdAt: time.Now(),
				lastSent:  time.Now(),
				nextRTO:   t.rttSmooth,
			}
			// ACK-only frames don't need retransmission; they're
			// disposable. Skip the sendBuf entry so we never end up
			// retransmitting a stale ACK.
			if kind != KindAckOnly {
				t.sendBuf[seq] = pf
			}
			peer := t.peerAddr
			t.mu.Unlock()
			if peer != nil {
				_, _ = t.conn.WriteToUDP(payload, peer)
			}
			return nil
		}
		t.mu.Unlock()
		// Window full. Wait briefly + retry, but respect caller ctx.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// SendAckOnly emits an ack frame with no body. Cheap; receiver fires
// these when an inbound data frame arrives but the sender has nothing
// outbound to piggyback the ACK on.
func (t *Transport) SendAckOnly() error {
	return t.Send(context.Background(), KindAckOnly, nil)
}

// Close sends a Bye, waits briefly for in-flight to drain, then
// signals the goroutines to exit.
func (t *Transport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	peer := t.peerAddr
	t.mu.Unlock()
	if peer != nil {
		_ = t.sendUnreliable(KindBye, nil)
	}
	close(t.done)
	return t.conn.Close()
}

// sendUnreliable bypasses sendBuf; used for Bye / immediate ACKs.
// Returns nil even if peer not yet adopted (a server-side close
// before the first inbound is a no-op).
//
// Critical: ACK-only and Bye frames carry seq=0 so they do NOT
// consume sequence numbers. Otherwise the peer's in-order delivery
// would treat them as gaps that need filling -- and our retransmit
// of a real data frame would carry a stale seq, breaking the window.
func (t *Transport) sendUnreliable(kind FrameKind, body []byte) error {
	t.mu.Lock()
	if t.peerAddr == nil {
		t.mu.Unlock()
		return nil
	}
	ack := t.highestPeer
	peer := t.peerAddr
	t.mu.Unlock()
	f := &Frame{Seq: 0, Ack: ack, Kind: kind, Body: body}
	payload, err := t.sendCodec.Seal(f)
	if err != nil {
		return err
	}
	_, err = t.conn.WriteToUDP(payload, peer)
	return err
}

func (t *Transport) receiver() {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-t.done:
			return
		default:
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			// Read deadline tick OR genuine error. Check done; loop.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		frame, derr := t.recvCodec.Open(buf[:n], t.expectDir)
		if derr != nil {
			// Bad packet: drop silently. Don't update peer addr from
			// a sender we can't authenticate.
			continue
		}
		t.handleFrame(frame, addr)
	}
}

func (t *Transport) handleFrame(f *Frame, src *net.UDPAddr) {
	t.mu.Lock()
	// Roaming: adopt the new source addr after a successful decrypt.
	if t.peerAddr == nil || !udpAddrsEqual(t.peerAddr, src) {
		t.peerAddr = src
	}
	// Process peer's ACK: drop all sendBuf entries with seq <= f.Ack.
	for seq := range t.sendBuf {
		if seq <= f.Ack {
			delete(t.sendBuf, seq)
		}
	}

	// Skip the deliver path for pure ACKs and Byes -- we still want
	// the ack-side effect on sendBuf above.
	if f.Kind == KindAckOnly || f.Kind == KindBye {
		t.mu.Unlock()
		if f.Kind == KindBye {
			// Surface a synthetic delivery so the consumer can react.
			t.inCh <- Inbound{Frame: f}
		}
		return
	}

	if f.Seq <= t.highestPeer {
		// Already delivered. Re-ACK so the sender can stop retransmitting.
		t.mu.Unlock()
		_ = t.sendUnreliable(KindAckOnly, nil)
		return
	}
	if f.Seq == t.highestPeer+1 {
		// In-order. Deliver, then drain any contiguous out-of-order.
		t.highestPeer = f.Seq
		t.mu.Unlock()
		t.inCh <- Inbound{Frame: f}
		for {
			t.mu.Lock()
			next, ok := t.recvBuf[t.highestPeer+1]
			if !ok {
				t.mu.Unlock()
				break
			}
			delete(t.recvBuf, t.highestPeer+1)
			t.highestPeer++
			t.mu.Unlock()
			t.inCh <- Inbound{Frame: next}
		}
		_ = t.sendUnreliable(KindAckOnly, nil)
		return
	}
	// Out-of-order. Hold; ack with our current high-water so the
	// sender retransmits the gap.
	t.recvBuf[f.Seq] = f
	t.mu.Unlock()
	_ = t.sendUnreliable(KindAckOnly, nil)
}

func (t *Transport) retransmitter(initialRTO, maxRTO time.Duration) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
		}
		now := time.Now()
		t.mu.Lock()
		peer := t.peerAddr
		var resend [][]byte
		for _, pf := range t.sendBuf {
			if peer == nil {
				continue
			}
			if now.Sub(pf.lastSent) >= pf.nextRTO {
				resend = append(resend, pf.payload)
				pf.lastSent = now
				pf.nextRTO *= 2
				if pf.nextRTO > maxRTO {
					pf.nextRTO = maxRTO
				}
			}
		}
		t.mu.Unlock()
		for _, p := range resend {
			if peer != nil {
				_, _ = t.conn.WriteToUDP(p, peer)
			}
		}
	}
}

// SendData is a convenience for the common case: emit a data frame.
func (t *Transport) SendData(ctx context.Context, body []byte) error {
	return t.Send(ctx, KindData, body)
}

// SendWinsize tells the peer the local terminal size changed.
// Format: rows(uint16 BE), cols(uint16 BE).
func (t *Transport) SendWinsize(rows, cols uint16) error {
	body := make([]byte, 4)
	body[0] = byte(rows >> 8)
	body[1] = byte(rows)
	body[2] = byte(cols >> 8)
	body[3] = byte(cols)
	return t.Send(context.Background(), KindWinsize, body)
}

// SendHello is the very first frame each side emits. Body is opaque
// banner bytes the application layer can use (version, capability
// strings, etc.) -- transport itself doesn't inspect it.
func (t *Transport) SendHello(body []byte) error {
	return t.Send(context.Background(), KindHello, body)
}

// udpAddrsEqual returns true when two UDP addresses point at the
// same endpoint. net.UDPAddr.IP supports both v4 and v6 byte forms;
// String() normalizes them for comparison.
func udpAddrsEqual(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.String() == b.String()
}

// String emits a compact diagnostic line. Used by the debug bits in
// the CLI to print "I'm sending to X" without exposing the secret.
func (t *Transport) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	peer := "(none)"
	if t.peerAddr != nil {
		peer = t.peerAddr.String()
	}
	return fmt.Sprintf("moshx.Transport{peer=%s, sendSeq=%d, highestPeer=%d, unacked=%d}",
		peer, t.sendSeq, t.highestPeer, len(t.sendBuf))
}
