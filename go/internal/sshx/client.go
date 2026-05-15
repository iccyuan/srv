package sshx

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client holds an ssh.Client, the profile it connected with, and a lazily
// opened *sftp.Client for file transfer. When connecting through ProxyJump
// hosts, the intermediate ssh.Client objects are stashed in `chain` so
// Close() can tear them down in reverse order without leaking sockets.
type Client struct {
	Profile *config.Profile
	Conn    *ssh.Client
	chain   []*ssh.Client
	sftpMu  sync.Mutex
	sftp    *sftp.Client
	// stopCh is closed by Close() so the keepalive goroutine returns
	// immediately instead of waiting up to one full keepalive_interval
	// for its next tick. Matters for short-lived clients (every MCP
	// `run` that bypasses the daemon dials a fresh client) where many
	// idle keepalive goroutines pile up otherwise.
	stopCh    chan struct{}
	closeOnce sync.Once
}

// dialOpts controls the auth/host-key behavior for one Dial call.
type DialOptions struct {
	// StrictHostKey: if true, only accept hosts already in known_hosts.
	// If false (default), accept-new -- writes back to known_hosts on
	// first connect. Unknown-key on existing entry always rejects.
	StrictHostKey bool
	// Timeout overrides profile.connect_timeout if non-zero.
	Timeout time.Duration
}

// Dial connects to the host described by profile, returning a *Client whose
// .Close() must be called when done.
func Dial(profile *config.Profile) (*Client, error) {
	return DialOpts(profile, DialOptions{})
}

func DialOpts(profile *config.Profile, opts DialOptions) (*Client, error) {
	auths, err := buildAuthMethods(profile)
	if err != nil {
		return nil, err
	}
	hkc, err := buildHostKeyCallback(opts.StrictHostKey)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = time.Duration(profile.GetConnectTimeout()) * time.Second
	}
	defaultUser := profile.User
	if defaultUser == "" {
		defaultUser = os.Getenv("USER")
		if defaultUser == "" {
			defaultUser = os.Getenv("USERNAME")
		}
	}

	mkConfig := func(user string) *ssh.ClientConfig {
		cfg := &ssh.ClientConfig{
			User:              user,
			Auth:              auths,
			HostKeyCallback:   hkc,
			Timeout:           timeout,
			HostKeyAlgorithms: profile.HostKeyAlgorithms,
		}
		// Pin crypto algorithms only when the profile explicitly listed
		// some -- a nil slice means "library default" which is what we
		// want for the common case. The fields live on the embedded
		// ssh.Config struct; we set them directly so the ssh library
		// uses our preference order in the handshake.
		if len(profile.Ciphers) > 0 {
			cfg.Ciphers = profile.Ciphers
		}
		if len(profile.MACs) > 0 {
			cfg.MACs = profile.MACs
		}
		if len(profile.KeyExchanges) > 0 {
			cfg.KeyExchanges = profile.KeyExchanges
		}
		return cfg
	}

	attempts := profile.GetDialAttempts()
	backoff := profile.GetDialBackoff()

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		c, err := dialOnce(profile, defaultUser, mkConfig, timeout)
		if err == nil {
			return c, nil
		}
		// Auth / host-key errors don't get any better with retries -- fail
		// fast so the user sees the real cause.
		if !IsRetryableDialErr(err) {
			return nil, err
		}
		lastErr = err
		if attempt >= attempts {
			break
		}
		wait := backoff << uint(attempt-1) // doubles each round
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		fmt.Fprintf(os.Stderr,
			"srv: dial attempt %d/%d failed: %v (retrying in %v)\n",
			attempt, attempts, err, wait)
		time.Sleep(wait)
	}
	return nil, lastErr
}

// dialOnce performs one full dial: ProxyJump chain (if any) followed by
// the final hop. Each TCP-level dial gets OS keepalive enabled so a
// silently-dead conn shows up as an EOF inside seconds rather than
// blocking forever on a write.
func dialOnce(profile *config.Profile, defaultUser string, mkConfig func(string) *ssh.ClientConfig, timeout time.Duration) (*Client, error) {
	var chain []*ssh.Client
	var err error
	for _, spec := range profile.Jump {
		hopUser, hopHost, hopPort := parseHostSpec(spec, defaultUser, 22)
		hopAddr := net.JoinHostPort(hopHost, srvutil.IntToStr(hopPort))
		hopCfg := mkConfig(hopUser)
		var hop *ssh.Client
		if len(chain) == 0 {
			hop, err = sshDialTCP(hopAddr, hopCfg, timeout)
		} else {
			hop, err = dialThrough(chain[len(chain)-1], hopAddr, hopCfg, timeout)
		}
		if err != nil {
			closeChain(chain)
			return nil, fmt.Errorf("jump %q: %w", spec, err)
		}
		chain = append(chain, hop)
	}

	targetAddr := net.JoinHostPort(profile.Host, srvutil.IntToStr(profile.GetPort()))
	targetCfg := mkConfig(defaultUser)
	var conn *ssh.Client
	if len(chain) == 0 {
		conn, err = sshDialTCP(targetAddr, targetCfg, timeout)
	} else {
		conn, err = dialThrough(chain[len(chain)-1], targetAddr, targetCfg, timeout)
	}
	if err != nil {
		closeChain(chain)
		return nil, err
	}

	c := &Client{Profile: profile, Conn: conn, chain: chain, stopCh: make(chan struct{})}
	// If the profile asks for agent forwarding AND a local agent is
	// reachable, register the route. Per-session RequestAgentForwarding
	// is still required, so callers that want forwarding for a session
	// must call MaybeRequestAgent(sess). Failures here are best-effort:
	// a missing/unparseable agent socket shouldn't kill the dial.
	if profile.GetAgentForwarding() {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if ac, derr := net.Dial("unix", sock); derr == nil {
				_ = agent.ForwardToAgent(conn, agent.NewClient(ac))
			}
		}
	}
	if interval := profile.GetKeepaliveInterval(); interval > 0 {
		go c.runKeepalive(interval, profile.GetKeepaliveCount())
	}
	return c, nil
}

// MaybeRequestAgent enables agent forwarding for `sess` when the
// profile has it on. Best-effort: agent.RequestAgentForwarding errors
// are reported to stderr but don't fail the caller -- a remote that
// refuses forwarding still gets to run the command, just without the
// forwarded keys. Caller MUST invoke this BEFORE sess.Run / Shell /
// Start, since the request is part of the channel-open handshake.
func (c *Client) MaybeRequestAgent(sess *ssh.Session) {
	if c == nil || c.Profile == nil || !c.Profile.GetAgentForwarding() {
		return
	}
	if err := agent.RequestAgentForwarding(sess); err != nil {
		fmt.Fprintf(os.Stderr, "srv: agent forwarding refused: %v\n", err)
	}
}

// sshDialTCP replaces ssh.Dial("tcp", ...) so we can flip on OS-level
// TCP keepalive (SO_KEEPALIVE) on the underlying socket. SSH-level
// keepalive (runKeepalive) covers the application layer; TCP-level
// catches dead conns at the kernel before bytes get queued forever.
// 15s is a friendly compromise between "notice fast" and "don't add
// chatter on healthy idle pools".
func sshDialTCP(addr string, cfg *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	d := net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(15 * time.Second)
	}
	if timeout > 0 {
		_ = raw.SetDeadline(time.Now().Add(timeout))
	}
	cc, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

// IsRetryableDialErr decides which errors are worth a backoff + redial.
// Auth and host-key errors are deterministic from the client's point of
// view -- another round trip won't change the answer, so let them fail
// immediately with the real diagnosis.
func IsRetryableDialErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "unable to authenticate") ||
		strings.Contains(s, "no supported methods remain") ||
		strings.Contains(s, "host key") ||
		strings.Contains(s, "knownhosts:") ||
		strings.Contains(s, "permission denied") {
		return false
	}
	return true
}

// dialThrough opens a TCP connection from `via` to `addr`, then performs the
// SSH handshake on it, returning a new ssh.Client tunneled through `via`.
func dialThrough(via *ssh.Client, addr string, cfg *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	// via.Dial doesn't honor a deadline directly, but the handshake call
	// below does via cfg.Timeout. We accept the dial-itself blocking on
	// network conditions; the cfg.Timeout protects the SSH handshake.
	netConn, err := via.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		_ = netConn.SetDeadline(time.Now().Add(timeout))
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		_ = netConn.Close()
		return nil, err
	}
	// Clear the deadline so subsequent traffic isn't bounded.
	_ = netConn.SetDeadline(time.Time{})
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// parseHostSpec splits a "[user@]host[:port]" into its components, falling
// back to defaultUser / defaultPort when omitted.
func parseHostSpec(spec, defaultUser string, defaultPort int) (user, host string, port int) {
	user = defaultUser
	port = defaultPort
	if i := strings.Index(spec, "@"); i >= 0 {
		user = spec[:i]
		spec = spec[i+1:]
	}
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		// IPv6 literals are bracketed; only treat the trailing :N as a port
		// when there's no closing ']' after it (i.e., the colon isn't part
		// of an [::1]:22 form, which we leave to net.JoinHostPort downstream).
		portStr := spec[i+1:]
		if p, err := strconv.Atoi(portStr); err == nil {
			port = p
			spec = spec[:i]
		}
	}
	host = strings.TrimPrefix(strings.TrimSuffix(spec, "]"), "[")
	return
}

// closeChain tears down a chain of ssh.Clients in reverse order. Used when
// a later hop in a ProxyJump chain fails so we don't leak earlier ones.
func closeChain(chain []*ssh.Client) {
	for i := len(chain) - 1; i >= 0; i-- {
		_ = chain[i].Close()
	}
}

func (c *Client) runKeepalive(intervalSec, maxFails int) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			if c.Conn == nil {
				return
			}
			_, _, err := c.Conn.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				failures++
				if failures >= maxFails {
					_ = c.Conn.Close()
					return
				}
			} else {
				failures = 0
			}
		}
	}
}

func (c *Client) Close() error {
	// Signal the keepalive goroutine first so it returns immediately
	// rather than waiting up to one keepalive_interval (default 30s) for
	// its next tick. closeOnce protects against double-close panics.
	c.closeOnce.Do(func() {
		if c.stopCh != nil {
			close(c.stopCh)
		}
	})
	c.sftpMu.Lock()
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	c.sftpMu.Unlock()
	var err error
	if c.Conn != nil {
		err = c.Conn.Close()
		c.Conn = nil
	}
	// Tear down ProxyJump intermediates in reverse order. Errors here are
	// best-effort; the primary connection's error is what we surface.
	closeChain(c.chain)
	c.chain = nil
	return err
}

// SFTP returns a lazily-opened sftp client. Caller must NOT close it; it's
// owned by the *Client and closed via c.Close().
func (c *Client) SFTP() (*sftp.Client, error) {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	s, err := sftp.NewClient(c.Conn)
	if err != nil {
		return nil, err
	}
	c.sftp = s
	return s, nil
}

// RunCaptureResult bundles the output of a captured remote command.
type RunCaptureResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Cwd      string `json:"cwd"`
}

// RunCapture runs `command` on the remote (in the persisted cwd, if cwd
// is non-empty), capturing both stdout and stderr. Returns a result struct
// with the exit code populated even on non-zero exits.
//
// When the client's profile has compress_streams=true, this routes
// through runCaptureGzip which gzip-wraps stdout on the wire and
// decompresses locally. Stderr stays raw because it's typically small.
// On any decode failure we fall back to the un-compressed path so a
// remote without gzip can't break the call.
func (c *Client) RunCapture(command string, cwd string) (*RunCaptureResult, error) {
	if c.Profile != nil && c.Profile.GetCompressStreams() {
		if res, ok := c.runCaptureGzip(command, cwd); ok {
			return res, nil
		}
		// Fall through to the un-compressed path on any glitch -- a
		// remote without gzip, a pipefail-unaware /bin/sh, etc.
	}
	full := WrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	err = sess.Run(full)
	exit := 0
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			// Other failure -- channel close, network issue. Surface as -1.
			return &RunCaptureResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String() + "\n" + err.Error(),
				ExitCode: -1,
				Cwd:      cwd,
			}, nil
		}
	}
	return &RunCaptureResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exit,
		Cwd:      cwd,
	}, nil
}

// runCaptureGzip runs the command with its stdout piped through gzip
// on the remote, decompresses locally, and returns a normal capture
// result. The wrapper sets pipefail so the actual command's exit
// propagates back (without it, the trailing gzip would always win
// with 0). Returns ok=false on any decode glitch so the caller can
// fall back to the plain path.
func (c *Client) runCaptureGzip(command, cwd string) (*RunCaptureResult, bool) {
	full := WrapWithCwd(command, cwd)
	wrapped := "set -o pipefail; (" + full + ") | gzip -c -1"
	sess, err := c.Conn.NewSession()
	if err != nil {
		return nil, false
	}
	defer sess.Close()
	var stdoutGz, stderr bytes.Buffer
	sess.Stdout = &stdoutGz
	sess.Stderr = &stderr
	runErr := sess.Run(wrapped)
	exit := 0
	if runErr != nil {
		var ee *ssh.ExitError
		if !errors.As(runErr, &ee) {
			return nil, false
		}
		exit = ee.ExitStatus()
	}
	// gzip on an empty pipeline still produces a 10-byte minimal
	// stream; "" stdout would mean the remote shell never even
	// reached gzip, so we treat zero bytes as a decode miss and
	// fall back.
	if stdoutGz.Len() == 0 {
		return &RunCaptureResult{Stdout: "", Stderr: stderr.String(), ExitCode: exit, Cwd: cwd}, true
	}
	gr, gerr := gzip.NewReader(&stdoutGz)
	if gerr != nil {
		return nil, false
	}
	defer gr.Close()
	plain, rerr := io.ReadAll(gr)
	if rerr != nil {
		return nil, false
	}
	return &RunCaptureResult{
		Stdout:   string(plain),
		Stderr:   stderr.String(),
		ExitCode: exit,
		Cwd:      cwd,
	}, true
}

// RunInteractive streams the remote command's stdio to the local terminal.
// If tty is true, allocates a pseudo-terminal on the remote (for vim, htop,
// sudo password prompt, etc.). Returns the remote exit code.
func (c *Client) RunInteractive(command string, cwd string, tty bool) (int, error) {
	full := WrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	// Agent forwarding (when profile.agent_forwarding=true) applies to
	// interactive runs: git push / pull on the remote, scp through the
	// pivot, etc. No-op for profiles that left the flag off.
	c.MaybeRequestAgent(sess)

	if tty {
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		w, h := 80, 24
		if cw, ch := srvtty.Size(); cw > 0 && ch > 0 {
			w, h = cw, ch
		}
		term := os.Getenv("TERM")
		if term == "" {
			term = "xterm-256color"
		}
		if err := sess.RequestPty(term, h, w, modes); err != nil {
			return -1, err
		}
		// Put local terminal in raw mode if possible.
		restore, _ := srvtty.MakeStdinRaw()
		if restore != nil {
			defer restore()
		}
		// Explicit VT-output enable on Windows so ANSI escape codes
		// from the remote render as colour/cursor moves rather than
		// visible garbage. No-op on Unix and on modern Windows
		// terminals that already enabled the bit.
		restoreVT := srvtty.EnableLocalVTOutput()
		defer restoreVT()
		// Forward local window-size changes to the remote PTY so
		// vim/htop/less keep redrawing at the right dimensions when
		// the user resizes their terminal mid-session. SIGWINCH on
		// Unix, polled GetConsoleScreenBufferInfo on Windows.
		stopResize := srvtty.WatchWindowResize(func(cols, rows int) {
			_ = sess.WindowChange(rows, cols)
		})
		defer stopResize()
	}

	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	if srvtty.IsStdinTTY() && tty {
		sess.Stdin = os.Stdin
	} else if !srvtty.IsStdinTTY() {
		// Pipe local stdin (e.g. `cat foo | srv "wc -l"`).
		sess.Stdin = os.Stdin
	}
	// else: terminal stdin in non-tty mode -- don't forward, lets ssh exit
	// promptly when remote command finishes.

	err = sess.Run(full)
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return ee.ExitStatus(), nil
		}
		return -1, err
	}
	return 0, nil
}

// Shell opens an interactive remote shell with a PTY allocated. Honors
// `cwd` by chaining `cd <cwd> && exec $SHELL -l` so the shell starts in
// the persisted directory and replaces our session (clean exit code).
func (c *Client) Shell(cwd string) (int, error) {
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	c.MaybeRequestAgent(sess)

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	w, h := 80, 24
	if cw, ch := srvtty.Size(); cw > 0 && ch > 0 {
		w, h = cw, ch
	}
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}
	if err := sess.RequestPty(term, h, w, modes); err != nil {
		return -1, err
	}

	restore, _ := srvtty.MakeStdinRaw()
	if restore != nil {
		defer restore()
	}
	// VT-output enable for ANSI escape rendering on Windows; no-op
	// on Unix and on terminals that already have the flag set.
	restoreVT := srvtty.EnableLocalVTOutput()
	defer restoreVT()
	// Window-size forwarding: same rationale as RunInteractive's PTY
	// branch. Without this the shell renders at the size it was
	// requested with, and a maximize-window action mid-session leaves
	// half the screen unused.
	stopResize := srvtty.WatchWindowResize(func(cols, rows int) {
		_ = sess.WindowChange(rows, cols)
	})
	defer stopResize()

	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	shellCmd := `exec "${SHELL:-/bin/bash}" -l`
	if cwd != "" {
		shellCmd = fmt.Sprintf("cd %s && %s", srvtty.ShQuotePath(cwd), shellCmd)
	}

	if err := sess.Run(shellCmd); err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return ee.ExitStatus(), nil
		}
		return -1, err
	}
	return 0, nil
}

// RunDetached spawns `command` on the remote with nohup, redirecting its
// stdout/stderr to ~/.srv-jobs/<jobID>.log. Returns the remote pid printed
// by the spawn line.
//
// The wrapper additionally writes the user command's exit code to
// ~/.srv-jobs/<jobID>.exit when it finishes. wait_job polls that file
// to know "done" without needing to keep an open SSH session for the
// duration of the user's job.
func (c *Client) RunDetached(command string, cwd string, jobID string) (int, error) {
	logPath := fmt.Sprintf("~/.srv-jobs/%s.log", jobID)
	exitPath := fmt.Sprintf("~/.srv-jobs/%s.exit", jobID)
	// Wrap the user command in a subshell so we can capture its exit
	// code regardless of how it terminates. base64 keeps quoting sane:
	// the user command may contain anything (heredocs, $vars, embedded
	// quotes) and we still ship it intact through `bash -c`.
	wrappedCmd := fmt.Sprintf("(%s); echo $? > %s", command, exitPath)
	encoded := srvtty.Base64Encode(wrappedCmd)
	wrapped := fmt.Sprintf(
		"mkdir -p ~/.srv-jobs && cd %s && (nohup bash -c \"$(echo %s | base64 -d)\" </dev/null >%s 2>&1 & echo $!)",
		srvtty.ShQuotePath(cwdOrTilde(cwd)), encoded, logPath,
	)
	res, err := c.RunCapture(wrapped, "")
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("spawn failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	pidStr := ""
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && srvutil.AllDigits(line) {
			pidStr = line
		}
	}
	if pidStr == "" {
		return 0, fmt.Errorf("remote did not return a pid; stdout=%q", res.Stdout)
	}
	pid := 0
	for _, ch := range pidStr {
		pid = pid*10 + int(ch-'0')
	}
	return pid, nil
}

// StreamChunkKind tags a streamed output line as stdout vs stderr so
// downstream consumers (MCP progress notifications, future log
// renderers) can format them differently.
type StreamChunkKind int

const (
	StreamStdout StreamChunkKind = iota
	StreamStderr
)

// RunStream runs `command` (optionally chdir'd to cwd) and invokes
// `onChunk` for every line of stdout/stderr as it arrives, then
// returns the exit code plus the FULL captured output for the caller
// to use as the final result.
//
// "Line" here is delimited by `\n` -- partial lines at EOF are still
// emitted. We cap each chunk at 8 KiB so a single mega-line (e.g.
// `cat huge-binary`) doesn't produce a single 1 GB callback payload.
// The trailing newline is preserved on each chunk so callers can
// emit them verbatim.
//
// Used by the MCP `run_stream` tool to push progress notifications
// while the command is still executing, sidestepping the MCP per-tool
// timeout for medium-length commands (20-50s builds/tests).
func (c *Client) RunStream(command string, cwd string, onChunk func(kind StreamChunkKind, line string)) (int, string, string, error) {
	full := WrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, "", "", err
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return -1, "", "", err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return -1, "", "", err
	}
	if err := sess.Start(full); err != nil {
		return -1, "", "", err
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go forwardStreamReader(stdout, StreamStdout, &stdoutBuf, onChunk, &wg)
	go forwardStreamReader(stderr, StreamStderr, &stderrBuf, onChunk, &wg)

	waitErr := sess.Wait()
	wg.Wait()

	exit := 0
	if waitErr != nil {
		var ee *ssh.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitStatus()
		} else {
			return -1, stdoutBuf.String(), stderrBuf.String(), waitErr
		}
	}
	return exit, stdoutBuf.String(), stderrBuf.String(), nil
}

// forwardStreamReader reads from `src` line-by-line, mirrors each line
// into `buf` (for the final captured-output return), and calls
// `onChunk` with the kind + line. Single 8 KiB read buffer caps the
// per-chunk size for callers that wrap each chunk in a message frame.
func forwardStreamReader(src io.Reader, kind StreamChunkKind, buf *bytes.Buffer, onChunk func(StreamChunkKind, string), wg *sync.WaitGroup) {
	defer wg.Done()
	rd := bufio.NewReaderSize(src, 8*1024)
	for {
		line, err := rd.ReadString('\n')
		if len(line) > 0 {
			buf.WriteString(line)
			if onChunk != nil {
				onChunk(kind, line)
			}
		}
		if err != nil {
			return
		}
	}
}

// RunStreamStdin runs a command on the remote with `stdin` providing the
// child's stdin (e.g., a local `tar -cf -` for sync). Stdout and stderr go
// to local stdout/stderr. Returns remote exit code.
// RunCaptureStdin streams `stdin` to the remote command while capturing
// its stdout/stderr into the returned RunCaptureResult. Used by callers
// that need to feed a large input list (NUL-separated paths, JSON,
// etc.) but also read structured output back -- bake-into-command
// doesn't scale once the input runs to thousands of entries.
//
// Wraps the command with `cd <cwd>` the same way RunCapture does so
// cwd handling is symmetric.
func (c *Client) RunCaptureStdin(command string, cwd string, stdin io.Reader) (*RunCaptureResult, error) {
	full := WrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	sess.Stdin = stdin
	err = sess.Run(full)
	exit := 0
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			return &RunCaptureResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String() + "\n" + err.Error(),
				ExitCode: -1,
				Cwd:      cwd,
			}, nil
		}
	}
	return &RunCaptureResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exit,
		Cwd:      cwd,
	}, nil
}

// RunStreamStdout runs `command` (wrapped with cwd if non-empty),
// piping its stdout to the supplied io.Writer in real time. stderr
// goes to os.Stderr so error noise still surfaces to the user.
// Returns the remote exit code, matching RunStreamStdin's shape.
//
// Used by streaming receivers like sync --pull's TarDownloadStream
// where the caller needs to consume stdout as it arrives rather
// than buffering the whole result into memory.
func (c *Client) RunStreamStdout(command string, cwd string, stdout io.Writer) (int, error) {
	full := WrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	sess.Stdout = stdout
	sess.Stderr = os.Stderr
	err = sess.Run(full)
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return ee.ExitStatus(), nil
		}
		return -1, err
	}
	return 0, nil
}

func (c *Client) RunStreamStdin(command string, stdin io.Reader) (int, error) {
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	sess.Stdin = stdin
	err = sess.Run(command)
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return ee.ExitStatus(), nil
		}
		return -1, err
	}
	return 0, nil
}

// WrapWithCwd wraps `command` with `cd <cwd> && (...)` when cwd is non-empty.
//
// The leading and trailing newlines inside the subshell are deliberate:
// they keep heredoc terminators (e.g. `EOF`) on their own line even when
// the user's command doesn't end with a newline. Without them, a command
// like `bash <<EOF\n...EOF` would get wrapped as `(bash <<EOF\n...EOF)`,
// putting `EOF)` on one line and breaking heredoc termination.
func WrapWithCwd(command, cwd string) string {
	if cwd == "" {
		return command
	}
	return fmt.Sprintf("cd %s && (\n%s\n)", srvtty.ShQuotePath(cwd), command)
}

func cwdOrTilde(cwd string) string {
	if cwd == "" {
		return "~"
	}
	return cwd
}

// buildAuthMethods returns the auth chain for an ssh.ClientConfig. Order:
//   - SSH agent (if SSH_AUTH_SOCK set)
//   - profile.identity_file (if set)
//   - common defaults: ~/.ssh/id_ed25519, ~/.ssh/id_rsa
func buildAuthMethods(profile *config.Profile) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(c)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	keyPaths := []string{}
	if profile.IdentityFile != "" {
		keyPaths = append(keyPaths, expandHome(profile.IdentityFile))
	} else {
		home, _ := os.UserHomeDir()
		keyPaths = append(keyPaths,
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
			filepath.Join(home, ".ssh", "id_ecdsa"),
		)
	}
	for _, p := range keyPaths {
		signer, err := loadPrivateKey(p)
		if err != nil || signer == nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable auth methods: agent unavailable and no key files found (looked for %s)", strings.Join(keyPaths, ", "))
	}
	return methods, nil
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		// Could be passphrase-protected. Try prompting if interactive.
		if _, ok := err.(*ssh.PassphraseMissingError); ok && srvtty.IsStdinTTY() {
			pass, perr := srvtty.PromptPassphrase(path)
			if perr != nil {
				return nil, perr
			}
			return ssh.ParsePrivateKeyWithPassphrase(b, pass)
		}
		return nil, err
	}
	return signer, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// buildHostKeyCallback returns a callback that:
//   - Accepts host keys recorded in ~/.ssh/known_hosts.
//   - On strict=false, also accepts a brand-new host (writes back to file).
//   - Always rejects when an entry exists but the key has changed.
func buildHostKeyCallback(strict bool) (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	khPath := filepath.Join(home, ".ssh", "known_hosts")

	// If the file doesn't exist yet, create an empty one so knownhosts.New
	// has something to read.
	if _, err := os.Stat(khPath); os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(khPath), 0o700)
		_ = os.WriteFile(khPath, []byte{}, 0o600)
	}

	verifier, err := knownhosts.New(khPath)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verifier(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 && !strict {
				// Brand new host: append to known_hosts.
				return appendKnownHost(khPath, hostname, remote, key)
			}
		}
		return err
	}, nil
}

func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(remote.String()), knownhosts.Normalize(hostname)}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// ExpandRemoteHome resolves a path with leading "~" to the remote
// user's $HOME via a single `echo $HOME` round-trip. Pure ~ returns
// just $HOME; "~/path" returns $HOME + path[1:]. No leading ~ means
// p is returned untouched.
//
// Lives on Client (and not as a free helper) because the lookup
// reuses the same SSH session the caller is already operating on --
// keeping that locality saves a re-Dial in code paths like push/pull/
// edit that hit ExpandRemoteHome once per file and would otherwise
// duplicate the connection setup.
func (c *Client) ExpandRemoteHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	res, err := c.RunCapture("echo $HOME", "")
	if err != nil {
		return p, err
	}
	if res.ExitCode != 0 {
		return p, fmt.Errorf("remote $HOME lookup failed: %s", strings.TrimSpace(res.Stderr))
	}
	home := strings.TrimSpace(res.Stdout)
	if home == "" {
		return p, fmt.Errorf("remote $HOME empty")
	}
	if p == "~" {
		return home, nil
	}
	return home + p[1:], nil
}
