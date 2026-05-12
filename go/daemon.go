package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Daemon protocol -- one JSON line per request, one JSON line per response.
//
//	request : { "id": <int>, "op": "<verb>", ... }
//	response: { "id": <int>, "ok": <bool>, "err": "<string>", ...payload }
//
// Verbs handled (v1):
//   - ls    {profile, prefix}              -> {entries: []string}
//   - cd    {profile, path}                -> {cwd: string}
//   - pwd   {profile}                      -> {cwd: string}
//   - run   {profile, command}             -> {stdout, stderr, exit_code}
//   - status                               -> {profiles_pooled: [...], uptime_sec}
//   - shutdown                             -> {ok: true}, then exits
//
// Heavy / streaming ops (push, pull, sync, shell, jobs) stay direct; the
// daemon only short-circuits the cold-handshake hot paths.

// DaemonProtoVersion is the wire format identifier sent on every request
// and response. Bump when a verb's argument shape or response shape
// changes incompatibly. Older daemons receive `"v": <newer>` and ignore
// it (json.Unmarshal default for unknown fields); newer daemons receive
// requests without `"v"` and treat them as v0 (unversioned, pre-2.4.1).
const DaemonProtoVersion = 1

type daemonRequest struct {
	V       int    `json:"v,omitempty"`
	ID      int    `json:"id"`
	Op      string `json:"op"`
	Profile string `json:"profile,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
	// Cwd is the *caller's* current working directory. The daemon never
	// reads from its own sessions.json -- the daemon's session id differs
	// from the calling shell's, so persisted cwd would be wrong. Instead
	// the CLI sends its cwd along with each request.
	Cwd string `json:"cwd,omitempty"`
	// Name carries the named entity (tunnel name today) for ops that
	// look up by name rather than by profile.
	Name string `json:"name,omitempty"`
	// PasswordB64 carries a sudo password on sudo_cache_set, base64
	// encoded so JSON quoting / control characters in the password
	// can't break the wire format. Not logged on either side.
	PasswordB64 string `json:"password_b64,omitempty"`
	// TTLSec is the sudo cache lifetime requested by the caller.
	// Capped server-side to a reasonable max regardless of what the
	// client asks for.
	TTLSec int `json:"ttl_sec,omitempty"`
}

type daemonResponse struct {
	V        int      `json:"v,omitempty"`
	ID       int      `json:"id"`
	OK       bool     `json:"ok"`
	Err      string   `json:"err,omitempty"`
	Data     any      `json:"data,omitempty"`
	Error    *wireErr `json:"error,omitempty"`
	Entries  []string `json:"entries,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Profiles []string `json:"profiles_pooled,omitempty"`
	Uptime   int64    `json:"uptime_sec,omitempty"`
	// Tunnels carries the active-tunnel snapshot returned by
	// tunnel_list. Empty otherwise.
	Tunnels []tunnelInfo `json:"tunnels,omitempty"`
	// TunnelErrors maps tunnel name -> last-attempt error message for
	// saved tunnels the daemon tried but failed to bring up
	// (autostart-on-boot or explicit tunnel_up). Cleared on
	// subsequent successful start. Names that appear here are NOT
	// also in Tunnels (a tunnel can't be both running and errored).
	TunnelErrors map[string]string `json:"tunnel_errors,omitempty"`
	// Listen is the human-readable listen address reported by
	// tunnel_up so the CLI can echo "listening on 127.0.0.1:5432".
	Listen string `json:"listen,omitempty"`
	// PasswordB64 is the sudo password returned by sudo_cache_get,
	// base64 encoded. Empty on cache miss.
	PasswordB64 string `json:"password_b64,omitempty"`
}

type wireErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// streamChunk is one frame of the stream_run multi-line response.
//
//	K = "out"  -> stdout chunk; B is base64-encoded bytes
//	K = "err"  -> stderr chunk; B is base64-encoded bytes
//	K = "end"  -> command completed; C is exit code
//	K = "fail" -> pre-execution failure (e.g. dial); Err carries reason
type streamChunk struct {
	V   int    `json:"v,omitempty"`
	ID  int    `json:"id,omitempty"`
	K   string `json:"k"`
	B   string `json:"b,omitempty"`
	C   int    `json:"c,omitempty"`
	Err string `json:"err,omitempty"`
}

type pooledClient struct {
	client   *Client
	lastUsed time.Time
}

type lsCacheEntry struct {
	entries []string
	cached  time.Time
}

type daemonState struct {
	mu        sync.Mutex
	pool      map[string]*pooledClient
	lsCache   map[string]*lsCacheEntry // key: profile + "\x00" + target
	listener  net.Listener
	startedAt time.Time
	// lastReq updated on every request; idle-shutdown checks this.
	lastReq  time.Time
	stopCh   chan struct{}
	stopOnce sync.Once
	// Active named tunnels. Separate mutex so a busy tunnel accept loop
	// doesn't contend with the per-request mu used by ls / cd / run.
	tunnelsMu sync.Mutex
	tunnels   map[string]*activeTunnel
	// tunnelErr remembers the last failure reason per tunnel name so
	// `srv tunnel list` / `srv ui` can surface "tried to autostart,
	// failed for X" instead of the misleading "stopped" the user got
	// before the daemon exposed this. Cleared on the next successful
	// start of the same tunnel.
	tunnelErrMu sync.Mutex
	tunnelErr   map[string]string
	// In-memory sudo password cache, keyed by profile name. Expires
	// per entry via sudoCacheEntry.expires (compared on get).
	sudoMu    sync.Mutex
	sudoCache map[string]sudoCacheEntry
}

// sudoCacheEntry is one cached sudo password. Stored only in daemon
// process memory; never persisted. Expires after .expires.
type sudoCacheEntry struct {
	password []byte
	expires  time.Time
}

// activeTunnel is one running, daemon-hosted tunnel. The forwarder
// goroutine watches stopCh and unwinds the listener when it fires.
type activeTunnel struct {
	name      string
	def       *TunnelDef
	profile   string
	listen    string // human-readable listen address (cached for status)
	startedAt time.Time
	stopCh    chan struct{}
	stopOnce  sync.Once
	// done closes when the forwarder goroutine exits. Lets shutdown
	// wait briefly for clean tear-down before yanking the daemon.
	done chan struct{}
}

func (a *activeTunnel) stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
}

const (
	// How long an unused per-profile SSH connection stays open. Closed
	// connections are re-dialed on next use.
	connIdleTTL = 10 * time.Minute
	// Pooled connections idle longer than this get a keepalive ping
	// before reuse, so we don't hand callers a silently-dead conn.
	poolHealthThreshold = 30 * time.Second
	// Whole-daemon idle shutdown threshold.
	daemonIdleTTL = 30 * time.Minute
	// Cleanup tick.
	gcInterval = 60 * time.Second
	// Completion prefetch is a latency optimization. Keep it bounded so a
	// directory with hundreds of children does not spend seconds issuing
	// background remote ls calls after one tab completion.
	daemonPrefetchLimit = 24
)

func daemonSocketPath() string {
	return filepath.Join(ConfigDir(), "daemon.sock")
}

// cmdDaemon starts the daemon listener (foreground). Ctrl-C stops it
// cleanly and unlinks the socket file.
func cmdDaemon(args []string) error {
	// Subcommands of `srv daemon` itself: status / stop.
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return exitCode(daemonClientStatus(args[1:]))
		case "stop":
			return exitCode(daemonClientStop())
		case "restart":
			if rc := daemonClientStop(); rc != 0 {
				return exitCode(rc)
			}
			// Wait for the old daemon to actually exit before
			// spawning a new one. daemonClientStop only waits for
			// the shutdown RPC's response, not for the daemon
			// process to finish teardown. If we race ahead,
			// ensureDaemon's ping might hit the still-listening
			// dying daemon, declare success, and skip the spawn --
			// at which point autostart tunnels never come back up
			// because no fresh daemon ever runs startAutostartTunnels.
			waitDaemonGone(3 * time.Second)
			if ensureDaemon() {
				fmt.Println("daemon: restarted")
				return nil
			}
			return exitErr(1, "daemon: restart failed")
		case "logs":
			return exitCode(daemonClientLogs())
		case "prune-cache":
			return exitCode(daemonClientPruneCache())
		}
	}
	sockPath := daemonSocketPath()
	_ = os.MkdirAll(filepath.Dir(sockPath), 0o755)
	// Best-effort cleanup of stale socket.
	if _, err := os.Stat(sockPath); err == nil {
		// Try a quick ping; if unreachable, remove.
		if !daemonPing() {
			_ = os.Remove(sockPath)
		} else {
			fmt.Fprintln(os.Stderr, "daemon already running at", sockPath)
			return exitCode(1)
		}
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon listen:", err)
		return exitCode(1)
	}
	_ = os.Chmod(sockPath, 0o600)
	fmt.Fprintln(os.Stderr, "srv daemon listening at", sockPath)

	state := &daemonState{
		pool:      map[string]*pooledClient{},
		lsCache:   map[string]*lsCacheEntry{},
		listener:  listener,
		startedAt: time.Now(),
		lastReq:   time.Now(),
		stopCh:    make(chan struct{}),
		tunnels:   map[string]*activeTunnel{},
		tunnelErr: map[string]string{},
		sudoCache: map[string]sudoCacheEntry{},
	}

	// Background gc: close idle connections, exit if whole daemon idle.
	go state.runGC()

	// Bring up any tunnels flagged autostart=true. Failures are logged
	// but don't block daemon startup -- a misconfigured tunnel
	// shouldn't keep ls / cd / run from working.
	go state.startAutostartTunnels()

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nsrv daemon: stopping.")
		state.requestStop()
	}()

	// Accept loop.
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-state.stopCh:
				state.closeAll()
				_ = os.Remove(sockPath)
				return nil
			default:
				fmt.Fprintln(os.Stderr, "daemon accept:", err)
				continue
			}
		}
		go state.handleConn(conn)
	}
}

func (s *daemonState) handleConn(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	wr := bufio.NewWriter(conn)
	// Multiple goroutines (stdout/stderr forwarders during stream_run) can
	// write to wr concurrently; serialize.
	var wrMu sync.Mutex
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req daemonRequest
		if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
			wrMu.Lock()
			s.write(wr, daemonResponse{OK: false, Err: "parse: " + jerr.Error()})
			wrMu.Unlock()
			continue
		}
		s.mu.Lock()
		s.lastReq = time.Now()
		s.mu.Unlock()
		if req.Op == "stream_run" {
			s.handleStreamRun(req, wr, &wrMu)
			continue
		}
		resp := s.dispatch(req)
		resp.ID = req.ID
		wrMu.Lock()
		s.write(wr, resp)
		wrMu.Unlock()
		if req.Op == "shutdown" {
			s.requestStop()
			return
		}
	}
}

func (s *daemonState) requestStop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *daemonState) write(wr *bufio.Writer, resp daemonResponse) {
	resp.V = DaemonProtoVersion
	if resp.OK && resp.Data == nil {
		resp.Data = daemonData(resp)
	}
	if !resp.OK && resp.Error == nil && resp.Err != "" {
		resp.Error = &wireErr{Code: "daemon_error", Message: resp.Err}
	}
	b, _ := json.Marshal(resp)
	wr.Write(b)
	wr.WriteByte('\n')
	wr.Flush()
}

func daemonData(resp daemonResponse) any {
	data := map[string]any{}
	if len(resp.Entries) > 0 {
		data["entries"] = resp.Entries
	}
	if resp.Cwd != "" {
		data["cwd"] = resp.Cwd
	}
	if resp.Stdout != "" {
		data["stdout"] = resp.Stdout
	}
	if resp.Stderr != "" {
		data["stderr"] = resp.Stderr
	}
	if resp.ExitCode != 0 {
		data["exit_code"] = resp.ExitCode
	}
	if len(resp.Profiles) > 0 {
		data["profiles_pooled"] = resp.Profiles
	}
	if resp.Uptime != 0 {
		data["uptime_sec"] = resp.Uptime
	}
	if len(data) == 0 {
		return nil
	}
	return data
}

func (s *daemonState) dispatch(req daemonRequest) (resp daemonResponse) {
	defer func() {
		if r := recover(); r != nil {
			resp = daemonResponse{OK: false, Err: fmt.Sprintf("daemon panic: %v", r)}
		}
	}()
	switch req.Op {
	case "ping":
		return daemonResponse{OK: true}
	case "status":
		s.mu.Lock()
		profs := make([]string, 0, len(s.pool))
		for k := range s.pool {
			profs = append(profs, k)
		}
		s.mu.Unlock()
		return daemonResponse{
			OK:       true,
			Profiles: profs,
			Uptime:   int64(time.Since(s.startedAt).Seconds()),
		}
	case "shutdown":
		return daemonResponse{OK: true}
	case "ls":
		return s.handleLs(req)
	case "cd":
		return s.handleCd(req)
	case "pwd":
		return s.handlePwd(req)
	case "run":
		return s.handleRun(req)
	case "tunnel_up":
		return s.handleTunnelUp(req)
	case "tunnel_down":
		return s.handleTunnelDown(req)
	case "tunnel_list":
		return s.handleTunnelList(req)
	case "sudo_cache_get":
		return s.handleSudoCacheGet(req)
	case "sudo_cache_set":
		return s.handleSudoCacheSet(req)
	case "sudo_cache_clear":
		return s.handleSudoCacheClear(req)
	}
	return daemonResponse{OK: false, Err: "unknown op: " + req.Op}
}

// getClient returns a pooled (or freshly dialed) Client for the named
// profile. Errors propagate to the caller; the dialed client stays in the
// pool until idle-collected.
//
// The Dial step happens OUTSIDE the daemon mutex -- a slow handshake (or
// a hanging dial when the remote is unreachable) must NOT block other
// requests like `status` or `shutdown` that just want to read map state.
func (s *daemonState) getClient(profileName string) (*Client, *Profile, error) {
	cfg, err := LoadConfig()
	if err != nil || cfg == nil {
		return nil, nil, fmt.Errorf("load config: %v", err)
	}
	if profileName == "" {
		profileName = cfg.DefaultProfile
	}
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return nil, nil, fmt.Errorf("profile %q not found", profileName)
	}
	profile.Name = profileName

	// Fast path: already pooled. Health-check connections that have been
	// idle longer than `poolHealthThreshold` -- the per-Client keepalive
	// goroutine handles drops on actively-used connections, but a long-
	// idle pooled conn can be silently dead (NAT timeout, server-side
	// idle kill) and we'd hand the caller a zombie. One round-trip ping
	// is cheap insurance.
	s.mu.Lock()
	pc, ok := s.pool[profileName]
	s.mu.Unlock()
	if ok && pc.client != nil && pc.client.Conn != nil {
		alive := true
		if time.Since(pc.lastUsed) > poolHealthThreshold {
			if _, _, err := pc.client.Conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				alive = false
			}
		}
		if alive {
			s.mu.Lock()
			pc.lastUsed = time.Now()
			s.mu.Unlock()
			return pc.client, profile, nil
		}
		// Stale -- evict and re-dial below.
		s.mu.Lock()
		if cur, ok := s.pool[profileName]; ok && cur == pc {
			delete(s.pool, profileName)
		}
		s.mu.Unlock()
		_ = pc.client.Close()
	}

	// Slow path: dial without holding s.mu. Other requests stay responsive.
	c, err := Dial(profile)
	if err != nil {
		return nil, profile, err
	}

	// Reacquire and install. Race: another request for the same profile
	// could have raced us to dial; in that case discard our duplicate.
	s.mu.Lock()
	if existing, ok := s.pool[profileName]; ok && existing.client != nil && existing.client.Conn != nil {
		existing.lastUsed = time.Now()
		s.mu.Unlock()
		_ = c.Close()
		return existing.client, profile, nil
	}
	s.pool[profileName] = &pooledClient{client: c, lastUsed: time.Now()}
	s.mu.Unlock()
	return c, profile, nil
}

func (s *daemonState) handleLs(req daemonRequest) daemonResponse {
	c, profile, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	// Use the caller's cwd (sent in the request); fall back to default for
	// safety only.
	cwd := req.Cwd
	if cwd == "" {
		cwd = profile.GetDefaultCwd()
	}
	dirPart, basePart := splitRemotePrefix(req.Prefix)
	target := remoteListTarget(dirPart, cwd)

	// In-memory cache: same target for the same profile within 5s -> instant.
	listing, fromCache := s.cachedListing(profile.Name, target)
	if !fromCache {
		listing, err = s.runLs(c, target)
		if err != nil {
			return daemonResponse{OK: false, Err: err.Error()}
		}
		s.cacheListing(profile.Name, target, listing)
		// Fire-and-forget prefetch of immediate sub-dirs so the next-level
		// tab is instant. Skipped on cache hits (would be redundant).
		go s.prefetchSubdirs(profile.Name, target, listing)
	}

	out := []string{}
	for _, line := range listing {
		if !strings.HasPrefix(line, basePart) {
			continue
		}
		out = append(out, dirPart+line)
	}
	return daemonResponse{OK: true, Entries: out}
}

// runLs runs `ls -1Ap` on the remote and returns the raw entries (one
// per line; dirs carry trailing "/").
func (s *daemonState) runLs(c *Client, target string) ([]string, error) {
	cmd := fmt.Sprintf("ls -1Ap -- %s", shQuotePath(target))
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return []string{}, nil
	}
	out := []string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func (s *daemonState) lsCacheKey(profileName, target string) string {
	return profileName + "\x00" + target
}

func (s *daemonState) cachedListing(profileName, target string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.lsCache[s.lsCacheKey(profileName, target)]
	if !ok {
		return nil, false
	}
	if time.Since(e.cached) > lsCacheTTL {
		return nil, false
	}
	// Defensive copy so callers can't mutate cached value.
	cp := make([]string, len(e.entries))
	copy(cp, e.entries)
	return cp, true
}

func (s *daemonState) cacheListing(profileName, target string, entries []string) {
	cp := make([]string, len(entries))
	copy(cp, entries)
	s.mu.Lock()
	s.lsCache[s.lsCacheKey(profileName, target)] = &lsCacheEntry{
		entries: cp,
		cached:  time.Now(),
	}
	s.mu.Unlock()
}

// prefetchSubdirs runs `ls -1Ap` for each immediate sub-dir of `parent`
// found in `entries`, populating the in-memory cache. Bounded concurrency
// (sequential here -- the SSH session multiplexer can handle several
// channels but we keep it simple).
func (s *daemonState) prefetchSubdirs(profileName, parent string, entries []string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "daemon prefetch panic:", r)
		}
	}()
	c, _, err := s.getClient(profileName)
	if err != nil {
		return
	}
	parent = strings.TrimRight(parent, "/") + "/"
	prefetched := 0
	for _, e := range entries {
		if !strings.HasSuffix(e, "/") {
			continue
		}
		if prefetched >= daemonPrefetchLimit {
			return
		}
		sub := parent + e
		if _, hit := s.cachedListing(profileName, sub); hit {
			continue
		}
		listing, err := s.runLs(c, sub)
		if err != nil {
			continue
		}
		s.cacheListing(profileName, sub, listing)
		prefetched++
	}
}

// lsCacheTTL was already defined in completion_remote.go for the file
// cache; reuse it for the daemon's in-memory cache too. (Defined there.)

func (s *daemonState) handleCd(req daemonRequest) daemonResponse {
	c, _, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	current := req.Cwd
	if current == "" {
		current = "~"
	}
	target := req.Path
	if target == "" {
		target = "~"
	}
	cmd := fmt.Sprintf(
		"cd %s 2>/dev/null || cd ~; cd %s && pwd",
		shQuotePath(current), shQuotePath(target),
	)
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("cd failed (exit %d)", res.ExitCode)
		}
		return daemonResponse{OK: false, Err: stderr}
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		return daemonResponse{OK: false, Err: "remote did not return a path"}
	}
	return daemonResponse{OK: true, Cwd: strings.TrimSpace(lines[len(lines)-1])}
}

func (s *daemonState) handlePwd(req daemonRequest) daemonResponse {
	// pwd is purely local in the new model -- the CLI reads its session
	// directly. Kept for protocol completeness; returns the request's cwd.
	cwd := req.Cwd
	if cwd == "" {
		cwd = "~"
	}
	return daemonResponse{OK: true, Cwd: cwd}
}

// handleStreamRun runs `req.Command` on the remote and forwards stdout
// and stderr back to the client as base64-encoded chunks, line by line.
// Final frame {"k":"end","c":<exit>}. If the daemon's writer fails (the
// CLI disconnected, e.g. user hit Ctrl+C), the ssh session is closed so
// the remote process gets a SIGHUP and we don't leak it.
func (s *daemonState) handleStreamRun(req daemonRequest, wr *bufio.Writer, wrMu *sync.Mutex) {
	emit := func(ch streamChunk) error {
		ch.V = DaemonProtoVersion
		ch.ID = req.ID
		b, _ := json.Marshal(ch)
		wrMu.Lock()
		defer wrMu.Unlock()
		if _, err := wr.Write(b); err != nil {
			return err
		}
		if err := wr.WriteByte('\n'); err != nil {
			return err
		}
		return wr.Flush()
	}
	fail := func(why string) {
		_ = emit(streamChunk{K: "fail", Err: why})
	}

	c, _, err := s.getClient(req.Profile)
	if err != nil {
		fail(err.Error())
		return
	}
	sess, err := c.Conn.NewSession()
	if err != nil {
		fail("new session: " + err.Error())
		return
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		fail("stdout pipe: " + err.Error())
		return
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		fail("stderr pipe: " + err.Error())
		return
	}

	full := req.Command
	if cwd := req.Cwd; cwd != "" {
		full = fmt.Sprintf("cd %s && (%s)", shQuotePath(cwd), req.Command)
	}
	if err := sess.Start(full); err != nil {
		fail("start: " + err.Error())
		return
	}

	var wg sync.WaitGroup
	forward := func(src io.Reader, kind string) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				ch := streamChunk{
					K: kind,
					B: base64.StdEncoding.EncodeToString(buf[:n]),
				}
				if werr := emit(ch); werr != nil {
					// Client gone -- kill the remote command.
					_ = sess.Close()
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go forward(stdout, "out")
	go forward(stderr, "err")

	waitErr := sess.Wait()
	wg.Wait()

	exit := 0
	if waitErr != nil {
		var ee *ssh.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitStatus()
		} else {
			exit = -1
		}
	}
	_ = emit(streamChunk{K: "end", C: exit})
}

func (s *daemonState) handleRun(req daemonRequest) daemonResponse {
	c, _, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	cwd := req.Cwd
	res, err := c.RunCapture(req.Command, cwd)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	return daemonResponse{
		OK:       true,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		Cwd:      cwd,
	}
}

func (s *daemonState) runGC() {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.gc()
		}
	}
}

func (s *daemonState) gc() {
	now := time.Now()
	s.mu.Lock()
	for name, pc := range s.pool {
		if now.Sub(pc.lastUsed) > connIdleTTL {
			_ = pc.client.Close()
			delete(s.pool, name)
		}
	}
	// Drop expired ls cache entries. Without this the map's keys grow
	// without bound across a long-running daemon (every distinct directory
	// ever tab-completed sticks around forever, even though the TTL check
	// in cachedListing makes them functionally dead).
	for k, e := range s.lsCache {
		if now.Sub(e.cached) > lsCacheTTL {
			delete(s.lsCache, k)
		}
	}
	idle := now.Sub(s.lastReq) > daemonIdleTTL
	s.mu.Unlock()
	// Idle-shutdown only when there's nothing the daemon is uniquely
	// hosting. Active tunnels live inside the daemon process: if we
	// exit, every forwarder dies and nothing auto-restarts them
	// (autostart only fires at daemon boot, which won't happen again
	// until some other srv command spawns a fresh daemon). Pooled
	// SSH connections are different -- closing them is cheap because
	// a future call just re-dials.
	if idle {
		s.tunnelsMu.Lock()
		nTunnels := len(s.tunnels)
		s.tunnelsMu.Unlock()
		if nTunnels > 0 {
			return
		}
		fmt.Fprintln(os.Stderr, "srv daemon: idle for",
			daemonIdleTTL, "-- shutting down.")
		s.requestStop()
	}
}

// waitDaemonGone polls until daemonPing() reports no daemon, or the
// deadline expires. Used after a shutdown RPC so a follow-up
// ensureDaemon() definitely sees a clean slate instead of racing
// with the old daemon's teardown.
func waitDaemonGone(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if !daemonPing() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *daemonState) closeAll() {
	// Stop tunnels first: their listeners hold references to pooled
	// clients, so closing the pool before the tunnels would race the
	// forwarder goroutines into "use of closed network connection"
	// errors during shutdown.
	s.stopAllTunnels()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pc := range s.pool {
		_ = pc.client.Close()
	}
	s.pool = nil
}
