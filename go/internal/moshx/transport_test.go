package moshx

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"
)

// Build a loopback pair of Transports sharing a 32-byte secret.
// Returns (client, server, closer).
func loopback(t *testing.T) (*Transport, *Transport, func()) {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	// Two UDP sockets on localhost; cross-wire them via initial PeerAddr.
	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		clientConn.Close()
		t.Fatal(err)
	}
	clientToServer, _ := NewCodec(secret, ClientToServer)
	serverToClient, _ := NewCodec(secret, ServerToClient)
	clientRecv, _ := NewCodec(secret, ServerToClient)
	serverRecv, _ := NewCodec(secret, ClientToServer)

	clientT, err := NewTransport(TransportConfig{
		Conn:       clientConn,
		Send:       clientToServer,
		Recv:       clientRecv,
		ExpectDir:  ServerToClient,
		PeerAddr:   serverConn.LocalAddr().(*net.UDPAddr),
		InitialRTO: 30 * time.Millisecond,
		MaxRTO:     200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverT, err := NewTransport(TransportConfig{
		Conn:      serverConn,
		Send:      serverToClient,
		Recv:      serverRecv,
		ExpectDir: ClientToServer,
		// Server discovers peer addr from first inbound frame.
		InitialRTO: 30 * time.Millisecond,
		MaxRTO:     200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		clientT.Close()
		serverT.Close()
	}
	return clientT, serverT, cleanup
}

func TestFrameSealOpenRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	send, _ := NewCodec(secret, ClientToServer)
	recv, _ := NewCodec(secret, ClientToServer)
	in := &Frame{Seq: 7, Ack: 3, Kind: KindData, Body: []byte("hello world")}
	wire, err := send.Seal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := recv.Open(wire, ClientToServer)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != 7 || out.Ack != 3 || out.Kind != KindData || string(out.Body) != "hello world" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestFrameWrongDirectionRejected(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	send, _ := NewCodec(secret, ClientToServer)
	recv, _ := NewCodec(secret, ClientToServer)
	wire, _ := send.Seal(&Frame{Seq: 1, Kind: KindData, Body: []byte("x")})
	// Tell Open to expect server-to-client; should reject the CTOS frame.
	if _, err := recv.Open(wire, ServerToClient); err == nil {
		t.Errorf("expected rejection of wrong-direction frame")
	}
}

func TestTransportBidirectionalDelivery(t *testing.T) {
	c, s, cleanup := loopback(t)
	defer cleanup()

	if err := c.SendData(context.Background(), []byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := waitFrame(t, s, time.Second)
	if got.Kind != KindData || string(got.Body) != "ping" {
		t.Errorf("server got %+v", got)
	}
	// Server now has client peer addr; reply.
	if err := s.SendData(context.Background(), []byte("pong")); err != nil {
		t.Fatal(err)
	}
	got = waitFrame(t, c, time.Second)
	if got.Kind != KindData || string(got.Body) != "pong" {
		t.Errorf("client got %+v", got)
	}
}

func TestTransportInOrderUnderReordering(t *testing.T) {
	// Send 50 frames quickly. Even if any individual UDP packet is
	// delayed slightly, the receiver should deliver them in order.
	c, s, cleanup := loopback(t)
	defer cleanup()

	for i := 0; i < 50; i++ {
		body := []byte{byte(i)}
		if err := c.SendData(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		got := waitFrame(t, s, 2*time.Second)
		if !bytes.Equal(got.Body, []byte{byte(i)}) {
			t.Errorf("seq mismatch at %d: got %v", i, got.Body)
		}
	}
}

func TestTransportWinsizeRoundTrip(t *testing.T) {
	c, s, cleanup := loopback(t)
	defer cleanup()
	if err := c.SendWinsize(40, 120); err != nil {
		t.Fatal(err)
	}
	got := waitFrame(t, s, time.Second)
	if got.Kind != KindWinsize {
		t.Fatalf("kind: %v", got.Kind)
	}
	rows := int(got.Body[0])<<8 | int(got.Body[1])
	cols := int(got.Body[2])<<8 | int(got.Body[3])
	if rows != 40 || cols != 120 {
		t.Errorf("rows/cols: %d/%d want 40/120", rows, cols)
	}
}

func TestTransportByeSurfaces(t *testing.T) {
	c, s, _ := loopback(t)
	// First frame so server learns client addr.
	c.SendData(context.Background(), []byte("hi"))
	waitFrame(t, s, time.Second)
	// Now client closes -- server's Recv should get a Bye.
	c.Close()
	got := waitFrame(t, s, time.Second)
	if got.Kind != KindBye {
		t.Errorf("expected Bye, got %v", got.Kind)
	}
	s.Close()
}

func TestTransportRoamingAdoptsNewClientAddr(t *testing.T) {
	// In-process roam: same client Transport (preserving codec state
	// and sequence numbers), but the underlying UDP packet appears
	// to arrive from a different source address. This models what
	// happens during NAT rebind / Wi-Fi → cellular: the user-space
	// client keeps its session state; only the kernel-visible 4-tuple
	// changes. The server should accept the GCM-authenticated frame
	// and adopt the new addr for outbound replies.
	secret := make([]byte, 32)
	rand.Read(secret)

	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	sToC, _ := NewCodec(secret, ServerToClient)
	sRecv, _ := NewCodec(secret, ClientToServer)
	serverT, _ := NewTransport(TransportConfig{
		Conn:       serverConn,
		Send:       sToC,
		Recv:       sRecv,
		ExpectDir:  ClientToServer,
		InitialRTO: 30 * time.Millisecond,
		MaxRTO:     200 * time.Millisecond,
	})
	defer serverT.Close()

	// Same codec used for "before" and "after" roam frames; this
	// guarantees the seq + nonce stream stays monotonic.
	cToS, _ := NewCodec(secret, ClientToServer)
	send := func(srcConn *net.UDPConn, seq uint64, body []byte) {
		f := &Frame{Seq: seq, Ack: 0, Kind: KindData, Body: body}
		wire, _ := cToS.Seal(f)
		_, _ = srcConn.WriteToUDP(wire, serverConn.LocalAddr().(*net.UDPAddr))
	}

	srcA, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	send(srcA, 1, []byte("from-A"))
	got := waitFrame(t, serverT, time.Second)
	if string(got.Body) != "from-A" {
		t.Fatalf("first round: %s", got.Body)
	}
	addr1 := serverT.PeerAddr().String()
	srcA.Close()

	srcB, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	defer srcB.Close()
	send(srcB, 2, []byte("from-B"))
	got = waitFrame(t, serverT, time.Second)
	if string(got.Body) != "from-B" {
		t.Fatalf("post-roam: %s", got.Body)
	}
	addr2 := serverT.PeerAddr().String()
	if addr1 == addr2 {
		t.Errorf("expected server to adopt a new peer addr (%s -> %s)", addr1, addr2)
	}
}

func waitFrame(t *testing.T, tr *Transport, d time.Duration) *Frame {
	t.Helper()
	select {
	case in := <-tr.Recv():
		return in.Frame
	case <-time.After(d):
		t.Fatalf("timeout waiting for frame on %s", tr)
		return nil
	}
}
