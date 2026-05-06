package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client holds an ssh.Client, the profile it connected with, and a lazily
// opened *sftp.Client for file transfer.
type Client struct {
	Profile *Profile
	Conn    *ssh.Client
	sftpMu  sync.Mutex
	sftp    *sftp.Client
}

// dialOpts controls the auth/host-key behavior for one Dial call.
type dialOpts struct {
	// strictHostKey: if true, only accept hosts already in known_hosts.
	// If false (default), accept-new -- writes back to known_hosts on first
	// connect. Unknown-key on existing entry always rejects.
	strictHostKey bool
	// timeout overrides profile.connect_timeout if non-zero.
	timeout time.Duration
}

// Dial connects to the host described by profile, returning a *Client whose
// .Close() must be called when done.
func Dial(profile *Profile) (*Client, error) {
	return DialOpts(profile, dialOpts{})
}

func DialOpts(profile *Profile, opts dialOpts) (*Client, error) {
	auths, err := buildAuthMethods(profile)
	if err != nil {
		return nil, err
	}
	hkc, err := buildHostKeyCallback(opts.strictHostKey)
	if err != nil {
		return nil, err
	}
	timeout := opts.timeout
	if timeout == 0 {
		timeout = time.Duration(profile.GetConnectTimeout()) * time.Second
	}
	user := profile.User
	if user == "" {
		user = os.Getenv("USER")
		if user == "" {
			user = os.Getenv("USERNAME")
		}
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hkc,
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(profile.Host, intToStr(profile.GetPort()))
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}

	c := &Client{Profile: profile, Conn: conn}

	// Optional: send keepalives so the server doesn't drop us on idle.
	if interval := profile.GetKeepaliveInterval(); interval > 0 {
		go c.runKeepalive(interval, profile.GetKeepaliveCount())
	}
	return c, nil
}

func (c *Client) runKeepalive(intervalSec, maxFails int) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	failures := 0
	for range ticker.C {
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

func (c *Client) Close() error {
	c.sftpMu.Lock()
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	c.sftpMu.Unlock()
	if c.Conn != nil {
		err := c.Conn.Close()
		c.Conn = nil
		return err
	}
	return nil
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
	Stdout   string
	Stderr   string
	ExitCode int
	Cwd      string
}

// RunCapture runs `command` on the remote (in the persisted cwd, if cwd
// is non-empty), capturing both stdout and stderr. Returns a result struct
// with the exit code populated even on non-zero exits.
func (c *Client) RunCapture(command string, cwd string) (*RunCaptureResult, error) {
	full := wrapWithCwd(command, cwd)
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

// RunInteractive streams the remote command's stdio to the local terminal.
// If tty is true, allocates a pseudo-terminal on the remote (for vim, htop,
// sudo password prompt, etc.). Returns the remote exit code.
func (c *Client) RunInteractive(command string, cwd string, tty bool) (int, error) {
	full := wrapWithCwd(command, cwd)
	sess, err := c.Conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()

	if tty {
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		w, h := 80, 24
		if cw, ch := terminalSize(); cw > 0 && ch > 0 {
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
		restore, _ := makeStdinRaw()
		if restore != nil {
			defer restore()
		}
	}

	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	if isStdinTTY() && tty {
		sess.Stdin = os.Stdin
	} else if !isStdinTTY() {
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

// RunDetached spawns `command` on the remote with nohup, redirecting its
// stdout/stderr to ~/.srv-jobs/<jobID>.log. Returns the remote pid printed
// by the spawn line.
func (c *Client) RunDetached(command string, cwd string, jobID string) (int, error) {
	logPath := fmt.Sprintf("~/.srv-jobs/%s.log", jobID)
	// base64 the command so quoting can't bite us, identical to the Python
	// implementation.
	encoded := base64Encode(command)
	wrapped := fmt.Sprintf(
		"mkdir -p ~/.srv-jobs && cd %s && (nohup bash -c \"$(echo %s | base64 -d)\" </dev/null >%s 2>&1 & echo $!)",
		shQuote(cwdOrTilde(cwd)), encoded, logPath,
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
		if line != "" && allDigits(line) {
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

// RunStreamStdin runs a command on the remote with `stdin` providing the
// child's stdin (e.g., a local `tar -cf -` for sync). Stdout and stderr go
// to local stdout/stderr. Returns remote exit code.
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

// wrapWithCwd wraps `command` with `cd <cwd> && (...)` when cwd is non-empty.
func wrapWithCwd(command, cwd string) string {
	if cwd == "" {
		return command
	}
	return fmt.Sprintf("cd %s && (%s)", shQuote(cwd), command)
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
func buildAuthMethods(profile *Profile) ([]ssh.AuthMethod, error) {
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
		if _, ok := err.(*ssh.PassphraseMissingError); ok && isStdinTTY() {
			pass, perr := promptPassphrase(path)
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
