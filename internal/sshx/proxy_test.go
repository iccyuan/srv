package sshx

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDialHTTPConnectAuthAndTunnel exercises the happy path:
// our dialer issues a CONNECT, supplies Basic auth from the URL,
// reads the 200, drains headers, and hands back a conn the caller
// can write to. Proxy here is an in-process loopback listener that
// returns the prescribed 200 reply; we don't need a real upstream
// because the dialer's contract ends at the tunnel handshake.
func TestDialHTTPConnectAuthAndTunnel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Fake proxy goroutine. Expect "CONNECT host:port HTTP/1.1\r\n"
	// followed by headers; reply "HTTP/1.1 200 Connection
	// established\r\n\r\n". Then keep the conn open until the test
	// drives the next stage.
	gotCONNECT := make(chan string, 1)
	gotAuth := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, _ := br.ReadString('\n')
		gotCONNECT <- strings.TrimRight(req, "\r\n")
		auth := ""
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(strings.ToLower(trimmed), "proxy-authorization:") {
				auth = strings.TrimSpace(strings.TrimPrefix(trimmed, "Proxy-Authorization:"))
				auth = strings.TrimSpace(strings.TrimPrefix(auth, "proxy-authorization:"))
			}
			if trimmed == "" {
				break
			}
		}
		gotAuth <- auth
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		// Echo a marker byte so the test confirms the tunnel is
		// transparent end-to-end. Anything in -> anything out.
		buf := make([]byte, 5)
		n, _ := br.Read(buf)
		if n > 0 {
			conn.Write(buf[:n])
		}
	}()

	conn, err := dialThroughProxy("http://alice:swordfish@"+ln.Addr().String(), "remote.example.com:22", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-gotCONNECT:
		if !strings.HasPrefix(got, "CONNECT remote.example.com:22 HTTP/") {
			t.Errorf("CONNECT line %q didn't match expected target", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never received CONNECT")
	}
	select {
	case auth := <-gotAuth:
		wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:swordfish"))
		if auth != wantBasic {
			t.Errorf("auth header %q != %q", auth, wantBasic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth header never observed")
	}

	// Tunnel transparency check: write 5 bytes, expect them back.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(echo) != "hello" {
		t.Errorf("tunnel garbled bytes: got %q", echo)
	}
}

// TestDialHTTPConnectProxyRefusal asserts that a non-2xx reply
// from the proxy turns into a real error rather than a silent
// fall-through to direct.
func TestDialHTTPConnectProxyRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		// Drain request
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimRight(line, "\r\n") == "" {
				break
			}
		}
		conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	}()
	_, err = dialThroughProxy("http://"+ln.Addr().String(), "remote:22", time.Second)
	if err == nil {
		t.Fatal("expected dial to fail on 407 proxy reply")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Errorf("error %q should mention the proxy's status line", err)
	}
}

// TestDialSocks5NoAuthAndTunnel walks the RFC 1928 handshake with
// METHOD 0x00 (no auth), then CONNECT to a hostname target, then
// verifies the tunnel echoes bytes end-to-end.
func TestDialSocks5NoAuthAndTunnel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	gotTarget := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Greeting: [VER NMETHODS METHODS...]
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		nm := int(head[1])
		if _, err := io.ReadFull(conn, make([]byte, nm)); err != nil {
			return
		}
		conn.Write([]byte{0x05, 0x00}) // pick no-auth
		// CONNECT request: VER CMD RSV ATYP DST.ADDR DST.PORT
		req := make([]byte, 4)
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		var host string
		switch req[3] {
		case 0x03:
			lb := make([]byte, 1)
			io.ReadFull(conn, lb)
			hb := make([]byte, lb[0])
			io.ReadFull(conn, hb)
			host = string(hb)
		case 0x01:
			ip := make([]byte, 4)
			io.ReadFull(conn, ip)
			host = net.IP(ip).String()
		}
		port := make([]byte, 2)
		io.ReadFull(conn, port)
		gotTarget <- host
		// Success reply: VER REP RSV ATYP=1 BND.ADDR BND.PORT
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		// Echo whatever the client writes.
		buf := make([]byte, 5)
		n, _ := conn.Read(buf)
		if n > 0 {
			conn.Write(buf[:n])
		}
	}()

	conn, err := dialThroughProxy("socks5://"+ln.Addr().String(), "remote.example.com:22", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-gotTarget:
		if got != "remote.example.com" {
			t.Errorf("socks5 target %q != remote.example.com", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never received CONNECT")
	}

	conn.Write([]byte("world"))
	echo := make([]byte, 5)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if !bytes.Equal(echo, []byte("world")) {
		t.Errorf("tunnel garbled bytes: %q", echo)
	}
}

// TestDialSocks5UsernamePasswordAuth runs the RFC 1929 sub-
// negotiation: server picks method 0x02, we send user/pass, server
// approves with status 0x00, dial proceeds.
func TestDialSocks5UsernamePasswordAuth(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	gotCreds := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Greeting: take method 0x02
		head := make([]byte, 2)
		io.ReadFull(conn, head)
		nm := int(head[1])
		io.ReadFull(conn, make([]byte, nm))
		conn.Write([]byte{0x05, 0x02})
		// Auth: VER=1 ULEN U PLEN P
		authHead := make([]byte, 2)
		io.ReadFull(conn, authHead)
		user := make([]byte, authHead[1])
		io.ReadFull(conn, user)
		plen := make([]byte, 1)
		io.ReadFull(conn, plen)
		pass := make([]byte, plen[0])
		io.ReadFull(conn, pass)
		gotCreds <- string(user) + ":" + string(pass)
		conn.Write([]byte{0x01, 0x00}) // auth ok
		// CONNECT then success
		req := make([]byte, 4)
		io.ReadFull(conn, req)
		// Skip variable address + port
		switch req[3] {
		case 0x03:
			lb := make([]byte, 1)
			io.ReadFull(conn, lb)
			io.ReadFull(conn, make([]byte, lb[0]+2))
		case 0x01:
			io.ReadFull(conn, make([]byte, 6))
		}
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()
	conn, err := dialThroughProxy("socks5://bob:secret@"+ln.Addr().String(), "10.0.0.1:22", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	select {
	case creds := <-gotCreds:
		if creds != "bob:secret" {
			t.Errorf("creds %q != bob:secret", creds)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never received auth")
	}
}

// TestDialSocks5ConnectFailure asserts the REP byte is surfaced
// with the RFC-mandated description rather than a numeric code.
func TestDialSocks5ConnectFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		head := make([]byte, 2)
		io.ReadFull(conn, head)
		io.ReadFull(conn, make([]byte, head[1]))
		conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		io.ReadFull(conn, req)
		switch req[3] {
		case 0x03:
			lb := make([]byte, 1)
			io.ReadFull(conn, lb)
			io.ReadFull(conn, make([]byte, lb[0]+2))
		}
		// REP=0x04 host unreachable
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()
	_, err = dialThroughProxy("socks5://"+ln.Addr().String(), "nowhere:22", time.Second)
	if err == nil {
		t.Fatal("expected REP=4 to fail dial")
	}
	if !strings.Contains(err.Error(), "host unreachable") {
		t.Errorf("error %q should name the REP code", err)
	}
}

// TestUnsupportedProxyScheme covers the input-validation path: we
// reject anything that isn't socks5 or http rather than silently
// fall through to direct.
func TestUnsupportedProxyScheme(t *testing.T) {
	_, err := dialThroughProxy("ftp://proxy:21", "target:22", time.Second)
	if err == nil || !strings.Contains(err.Error(), "unsupported proxy scheme") {
		t.Errorf("expected unsupported-scheme error, got: %v", err)
	}
}
