package sshx

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dialThroughProxy opens a TCP connection to `targetAddr` by way of
// the proxy described by `proxyURL`. Two schemes are recognised:
//
//	socks5://[user:pass@]host:port   RFC 1928 + RFC 1929 (user/pass auth)
//	http://[user:pass@]host:port     HTTP CONNECT + Proxy-Authorization Basic
//
// The returned net.Conn is the underlying TCP connection to the
// proxy after the tunnel handshake completes; subsequent reads/
// writes flow end-to-end to the SSH server on the far side. Errors
// from the handshake (auth refused, proxy can't reach target,
// unsupported scheme) are returned verbatim -- callers MUST NOT
// silently fall back to direct connect; that would defeat the
// corporate egress policy the proxy is enforcing.
func dialThroughProxy(proxyURL string, targetAddr string, timeout time.Duration) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy url parse %q: %v", proxyURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy url has no host: %q", proxyURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		return dialSocks5(u, targetAddr, timeout)
	case "http":
		return dialHTTPConnect(u, targetAddr, timeout)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want socks5 or http)", u.Scheme)
	}
}

// dialHTTPConnect runs the HTTP/1.1 CONNECT method through the
// proxy. Auth is sent inline via Proxy-Authorization: Basic when
// the URL carries user info; otherwise the CONNECT goes anonymous
// and the proxy decides. Reads + discards response headers after
// the 200 status line so subsequent SSH bytes start cleanly.
func dialHTTPConnect(u *url.URL, targetAddr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("http proxy dial %s: %v", u.Host, err)
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if u.User != nil {
		username := u.User.Username()
		password, _ := u.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", auth)
	}
	sb.WriteString("\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("http proxy write: %v", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http proxy read status: %v", err)
	}
	status = strings.TrimRight(status, "\r\n")
	// "HTTP/1.1 200 Connection established" -- accept any 2xx.
	parts := strings.SplitN(status, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[1], "2") {
		conn.Close()
		return nil, fmt.Errorf("http proxy refused: %s", status)
	}
	// Drain remaining headers up to the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("http proxy read headers: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}
	// Clear the per-handshake deadline; SSH traffic from here on
	// must not be bounded.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialSocks5 walks RFC 1928 (greeting + CONNECT) plus RFC 1929
// (username/password auth) when the URL carries credentials. The
// "socks5h" alias is accepted so users coming from curl/SSH
// conventions get hostname resolution at the proxy without having
// to learn srv-specific spelling -- we always send the hostname
// over the wire (ATYP 0x03) when the target isn't already an IP.
func dialSocks5(u *url.URL, targetAddr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s: %v", u.Host, err)
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	hasAuth := u.User != nil
	methods := []byte{0x00}
	if hasAuth {
		methods = []byte{0x00, 0x02}
	}
	hello := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(hello); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 greeting: %v", err)
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(conn, greet); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 greeting reply: %v", err)
	}
	if greet[0] != 0x05 {
		conn.Close()
		return nil, fmt.Errorf("socks5: bad protocol version %d", greet[0])
	}
	switch greet[1] {
	case 0x00:
		// No auth required.
	case 0x02:
		if !hasAuth {
			conn.Close()
			return nil, errors.New("socks5: server requires auth but profile has no credentials")
		}
		username := u.User.Username()
		password, _ := u.User.Password()
		if len(username) > 255 || len(password) > 255 {
			conn.Close()
			return nil, errors.New("socks5: username or password too long (>255 bytes)")
		}
		req := []byte{0x01, byte(len(username))}
		req = append(req, []byte(username)...)
		req = append(req, byte(len(password)))
		req = append(req, []byte(password)...)
		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 auth write: %v", err)
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 auth reply: %v", err)
		}
		if authResp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5 auth rejected (status=%d)", authResp[1])
		}
	case 0xff:
		conn.Close()
		return nil, errors.New("socks5: server rejected all offered auth methods")
	default:
		conn.Close()
		return nil, fmt.Errorf("socks5: server picked unsupported auth method %d", greet[1])
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 target parse %q: %v", targetAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 0xffff {
		conn.Close()
		return nil, fmt.Errorf("socks5 target port: %q", portStr)
	}

	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = []byte{0x05, 0x01, 0x00, 0x01}
			req = append(req, ip4...)
		} else {
			req = []byte{0x05, 0x01, 0x00, 0x04}
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			conn.Close()
			return nil, errors.New("socks5: hostname too long (>255 bytes)")
		}
		req = []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect write: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect reply head: %v", err)
	}
	if head[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed: %s", socks5RepName(head[1]))
	}
	var bndLen int
	switch head[3] {
	case 0x01:
		bndLen = 4
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 bnd domain len: %v", err)
		}
		bndLen = int(lb[0])
	case 0x04:
		bndLen = 16
	default:
		conn.Close()
		return nil, fmt.Errorf("socks5: unexpected ATYP %d in reply", head[3])
	}
	skip := make([]byte, bndLen+2) // BND.ADDR + BND.PORT
	if _, err := io.ReadFull(conn, skip); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 drain bnd: %v", err)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// socks5RepName turns the REP byte into the human-readable error
// name from RFC 1928 §6. Used to surface "host unreachable" rather
// than "connect failed (rep=4)" in user-facing errors.
func socks5RepName(rep byte) string {
	switch rep {
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown REP %d", rep)
	}
}
