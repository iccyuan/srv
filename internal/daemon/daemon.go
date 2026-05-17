package daemon

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
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strings"
	"sync"
	"sync/atomic"
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

type Request struct {
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

type Response struct {
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
	Tunnels []TunnelInfo `json:"tunnels,omitempty"`
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
	client   *sshx.Client
	lastUsed time.Time
	// inflight = currently-open SSH sessions running through this
	// connection. acquireClient picks the lowest-inflight connection
	// from the per-profile slot; release() decrements when the
	// caller's session is done. Atomic so the hot read inside
	// acquireClient doesn't have to grab a mutex.
	inflight atomic.Int32
}

type lsCacheEntry struct {
	entries []string
	cached  time.Time
}

type daemonState struct {
	mu sync.Mutex
	// pool now holds a slice per profile -- up to PoolSize SSH
	// connections share the work for one profile when concurrency
	// warrants it. acquireClient picks least-inflight; lazy growth
	// up to the cap, idle-GC trims back down.
	pool      map[string][]*pooledClient
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
	// runInflight implements single-flight for the `run` op: while one
	// goroutine is executing a (profile, cwd, command) tuple, every
	// other goroutine that asks for the SAME tuple waits on its
	// channel and reuses the result instead of opening its own SSH
	// session. The motivating case is parallel MCP tool calls: when
	// Claude fires 4 identical `srv ls /` requests in the same window
	// (which it does more often than you'd think), the daemon now
	// pays the cost once. Keyed by NUL-joined fields so empties on
	// either side don't collapse into ambiguities.
	runMu       sync.Mutex
	runInflight map[string]*runInflightEntry
}

// runInflightEntry is one in-flight `run` request other goroutines can
// hitch a ride on. `done` closes when the running goroutine has
// populated `resp`. Readers must wait on `done` before reading the
// other fields -- the fields are NOT protected by a mutex and the
// channel-close happens-before relationship is what makes the read
// safe afterward.
type runInflightEntry struct {
	done chan struct{}
	resp Response
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
	def       *config.TunnelDef
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
	return filepath.Join(srvutil.Dir(), "daemon.sock")
}

// daemonPortPath holds the TCP loopback port number when we couldn't
// bind a unix socket -- the old-Windows / Server 2016 fallback path
// described in daemonListen's comment.
func daemonPortPath() string {
	return filepath.Join(srvutil.Dir(), "daemon.port")
}

// daemonListen picks a transport for the daemon listener. Unix
// domain socket is the preferred path (cheaper, naturally
// user-private, no port allocation); TCP loopback is the fallback
// for hosts that don't support AF_UNIX. Windows 10 1803+ and Server
// 2019+ have native AF_UNIX, so the fast path covers everything
// modern; older Win10 / Server 2016 boxes are the audience for the
// fallback.
//
// Security note: TCP loopback is technically reachable by any local
// user on the same machine. We accept that exposure for the
// fallback case because it only fires when unix sockets are flat-out
// unavailable, and the alternative is "daemon can't run at all".
// Users on shared hosts that need AF_UNIX semantics should upgrade
// the OS.
func daemonListen() (net.Listener, transportInfo, error) {
	sockPath := daemonSocketPath()
	if l, err := net.Listen("unix", sockPath); err == nil {
		_ = os.Chmod(sockPath, 0o600)
		// Clean up any stale port file from a previous TCP-fallback
		// daemon run; readers prefer unix when present so a stale
		// port file is at best confusing, at worst hides a stale-
		// port issue behind a working unix socket.
		_ = os.Remove(daemonPortPath())
		return l, transportInfo{kind: "unix", addr: sockPath}, nil
	}
	// Unix socket failed -- fall back to TCP loopback with an
	// ephemeral port. ":0" lets the kernel pick; we write the
	// resolved port to a file so clients know where to dial.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, transportInfo{}, fmt.Errorf("listen tcp: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(daemonPortPath(), []byte(fmt.Sprintf("%d", port)), 0o600); err != nil {
		_ = l.Close()
		return nil, transportInfo{}, fmt.Errorf("write port file: %v", err)
	}
	return l, transportInfo{kind: "tcp", addr: fmt.Sprintf("127.0.0.1:%d", port)}, nil
}

// transportInfo carries the chosen listener's identity through to
// the daemon's logging + cleanup paths. kind is "unix" or "tcp";
// addr is the human-readable address we just bound.
type transportInfo struct {
	kind string
	addr string
}

// Cmd starts the daemon listener (foreground). Ctrl-C stops it
// cleanly and unlinks the socket file.
func Cmd(args []string) error {
	// Subcommands of `srv daemon` itself: status / stop.
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return srvutil.Code(daemonClientStatus(args[1:]))
		case "stop":
			return srvutil.Code(daemonClientStop())
		case "restart":
			if rc := daemonClientStop(); rc != 0 {
				return srvutil.Code(rc)
			}
			// Wait for the old daemon to actually exit before
			// spawning a new one. daemonClientStop only waits for
			// the shutdown RPC's response, not for the daemon
			// process to finish teardown. If we race ahead,
			// Ensure's ping might hit the still-listening
			// dying daemon, declare success, and skip the spawn --
			// at which point autostart tunnels never come back up
			// because no fresh daemon ever runs startAutostartTunnels.
			waitDaemonGone(3 * time.Second)
			if Ensure() {
				fmt.Println("daemon: restarted")
				return nil
			}
			return fmt.Errorf("daemon: restart failed")
		case "logs":
			return srvutil.Code(daemonClientLogs())
		}
	}
	sockPath := daemonSocketPath()
	_ = os.MkdirAll(filepath.Dir(sockPath), 0o755)
	// Best-effort cleanup of stale socket / port file. Ping covers
	// both transports because the client side also tries both.
	if _, err := os.Stat(sockPath); err == nil {
		if !Ping() {
			_ = os.Remove(sockPath)
		} else {
			fmt.Fprintln(os.Stderr, "daemon already running at", sockPath)
			return fmt.Errorf("")
		}
	}
	if _, err := os.Stat(daemonPortPath()); err == nil {
		if !Ping() {
			_ = os.Remove(daemonPortPath())
		} else {
			fmt.Fprintln(os.Stderr, "daemon already running (TCP fallback)")
			return fmt.Errorf("")
		}
	}

	listener, transport, err := daemonListen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon listen:", err)
		return fmt.Errorf("")
	}
	fmt.Fprintf(os.Stderr, "srv daemon listening at %s (%s)\n", transport.addr, transport.kind)

	state := &daemonState{
		pool:        map[string][]*pooledClient{},
		lsCache:     map[string]*lsCacheEntry{},
		listener:    listener,
		startedAt:   time.Now(),
		lastReq:     time.Now(),
		stopCh:      make(chan struct{}),
		tunnels:     map[string]*activeTunnel{},
		tunnelErr:   map[string]string{},
		sudoCache:   map[string]sudoCacheEntry{},
		runInflight: map[string]*runInflightEntry{},
	}

	// Background gc: close idle connections, exit if whole daemon idle.
	go state.runGC()

	// Bring up any tunnels flagged autostart=true. Failures are logged
	// but don't block daemon startup -- a misconfigured tunnel
	// shouldn't keep ls / cd / run from working.
	go state.startAutostartTunnels()

	// Pre-warm SSH connections for profiles flagged autoconnect=true.
	// The lazy-dial pattern means the first request on a fresh daemon
	// pays the ~200-800ms handshake before any user-visible work
	// happens; pre-warm flips that to 0-RTT for the profiles the user
	// flagged as "I live here". Failures are logged but never block
	// startup -- a down host shouldn't keep the rest of the daemon
	// idle.
	go state.startAutoconnectProfiles()

	// Background job-completion watcher: fires local toast + webhook
	// for detached jobs whose remote `.exit` marker just appeared.
	// Bails out cheaply when no JobNotify is configured.
	go state.runJobWatcher()

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
				// Clean up whichever transport we used. Best-effort
				// on each -- a missing file is fine, an unremovable
				// file is also non-fatal (next daemon's startup
				// cleans).
				_ = os.Remove(sockPath)
				_ = os.Remove(daemonPortPath())
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
		var req Request
		if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
			wrMu.Lock()
			s.write(wr, Response{OK: false, Err: "parse: " + jerr.Error()})
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

func (s *daemonState) write(wr *bufio.Writer, resp Response) {
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

func daemonData(resp Response) any {
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

// opHandler is the per-request signature every daemon op satisfies.
// All return a single Response; the streaming op (stream_run) lives
// outside the registry because it writes multiple frames and is
// dispatched separately in handleConn.
type opHandler func(s *daemonState, req Request) Response

// opRegistry maps protocol-level op names to their method values.
// New ops register by appending an entry here -- there's no central
// switch statement to maintain anymore, so the cost of adding an op
// is exactly: write the method, add one map entry. Mirrors the
// registry pattern internal/mcp already uses for its tool surface.
var opRegistry = map[string]opHandler{
	"ping":             (*daemonState).handlePing,
	"status":           (*daemonState).handleStatus,
	"shutdown":         (*daemonState).handleShutdown,
	"ls":               (*daemonState).handleLs,
	"cd":               (*daemonState).handleCd,
	"pwd":              (*daemonState).handlePwd,
	"run":              (*daemonState).handleRun,
	"tunnel_up":        (*daemonState).handleTunnelUp,
	"tunnel_down":      (*daemonState).handleTunnelDown,
	"tunnel_list":      (*daemonState).handleTunnelList,
	"sudo_cache_get":   (*daemonState).handleSudoCacheGet,
	"sudo_cache_set":   (*daemonState).handleSudoCacheSet,
	"sudo_cache_clear": (*daemonState).handleSudoCacheClear,
	"disconnect":       (*daemonState).handleDisconnect,
	"disconnect_all":   (*daemonState).handleDisconnectAll,
}

func (s *daemonState) dispatch(req Request) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			resp = Response{OK: false, Err: fmt.Sprintf("daemon panic: %v", r)}
		}
	}()
	h, ok := opRegistry[req.Op]
	if !ok {
		return Response{OK: false, Err: "unknown op: " + req.Op}
	}
	return h(s, req)
}

// handlePing is the cheapest op -- callers use it to detect daemon
// liveness before deciding whether to spawn a fresh one. No state
// access; doesn't even need to hold s.mu.
func (s *daemonState) handlePing(req Request) Response {
	return Response{OK: true}
}

// handleStatus reports uptime + per-profile pool occupancy. Used by
// `srv daemon status` and the UI dashboard. Returns a list of
// profile names that have at least one pooled connection.
func (s *daemonState) handleStatus(req Request) Response {
	s.mu.Lock()
	profs := make([]string, 0, len(s.pool))
	for k := range s.pool {
		profs = append(profs, k)
	}
	s.mu.Unlock()
	return Response{
		OK:       true,
		Profiles: profs,
		Uptime:   int64(time.Since(s.startedAt).Seconds()),
	}
}

// handleShutdown returns the ack response. The actual stop-signal
// fires in handleConn after the response has been written, so the
// client gets a clean reply before the socket goes away. Kept as a
// no-op handler here so the registry has uniform coverage of all
// op names -- the special-case logic stays in handleConn.
func (s *daemonState) handleShutdown(req Request) Response {
	return Response{OK: true}
}

// acquireClient checks out one SSH connection from the per-profile
// pool, returning the client + profile + a release closure that the
// caller MUST defer-call when the work is done. Inflight tracking
// drives "least-loaded" selection across the slice of connections we
// hold per profile -- when one connection is busy with N open
// channels and another is idle, future acquirers route to the idle
// one.
//
// Growth: if every existing connection is in use (inflight > 0) and
// the pool size is below the profile's GetPoolSize() cap, we dial a
// fresh one. Below the cap with idle conns we just reuse. At the cap
// and all busy, the lowest-inflight one is picked anyway (SSH
// multiplexes channels on a single TCP/encrypted connection, so this
// is degraded throughput, not blocking).
//
// Shrinkage is the GC's job -- this function never closes anything.
func (s *daemonState) acquireClient(profileName string) (*sshx.Client, *config.Profile, func(), error) {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil, nil, nil, fmt.Errorf("load config: %v", err)
	}
	if profileName == "" {
		profileName = cfg.DefaultProfile
	}
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return nil, nil, nil, fmt.Errorf("profile %q not found", profileName)
	}
	profile.Name = profileName
	poolSize := profile.GetPoolSize()

	// Fast path: reuse an existing pooled conn. Pick the lowest-
	// inflight one; if multiple tie, prefer the most recently used so
	// a known-good connection wins over a long-idle one that might
	// have NAT-timed-out under us.
	s.mu.Lock()
	pcs := s.pool[profileName]
	var best *pooledClient
	for _, pc := range pcs {
		if pc.client == nil || pc.client.Conn == nil {
			continue
		}
		if best == nil ||
			pc.inflight.Load() < best.inflight.Load() ||
			(pc.inflight.Load() == best.inflight.Load() && pc.lastUsed.After(best.lastUsed)) {
			best = pc
		}
	}
	canGrow := len(pcs) < poolSize
	// Idle-conn health check: if we'd hand out a conn that's been
	// idle past poolHealthThreshold, ping it first. Same logic as
	// before the multi-conn refactor, just lifted out so each pool
	// slot can be re-validated independently.
	if best != nil && best.inflight.Load() == 0 && time.Since(best.lastUsed) > poolHealthThreshold {
		s.mu.Unlock()
		if _, _, err := best.client.Conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			// Stale -- drop from pool and fall through to dial.
			s.mu.Lock()
			s.evictFromSlot(profileName, best)
			pcs = s.pool[profileName]
			canGrow = len(pcs) < poolSize
			s.mu.Unlock()
			_ = best.client.Close()
			best = nil
			// Re-scan for a healthy alternative.
			s.mu.Lock()
			for _, pc := range s.pool[profileName] {
				if pc.client == nil || pc.client.Conn == nil {
					continue
				}
				if best == nil || pc.inflight.Load() < best.inflight.Load() {
					best = pc
				}
			}
		} else {
			s.mu.Lock()
		}
	}
	// Decision matrix:
	//   1. We have a healthy conn AND (it's idle OR pool is at cap) -> reuse
	//   2. We have a healthy conn AND pool can grow AND all conns are busy -> dial new
	//   3. Pool empty / no healthy conn -> dial new
	if best != nil && (best.inflight.Load() == 0 || !canGrow) {
		best.inflight.Add(1)
		best.lastUsed = time.Now()
		s.mu.Unlock()
		return s.leaseRelease(best, profile)
	}
	s.mu.Unlock()

	// Slow path: dial without holding the mutex. Other requests
	// (status, ls on a different profile, disconnect) stay responsive
	// during a slow handshake.
	c, derr := sshx.Dial(profile)
	if derr != nil {
		return nil, profile, nil, derr
	}

	// Install the new conn. Re-check the cap because peer goroutines
	// could have grown the pool while we were dialing; if we lost
	// the race, drop the surplus dial and reuse whatever's there.
	s.mu.Lock()
	if cur := s.pool[profileName]; len(cur) >= poolSize {
		s.mu.Unlock()
		_ = c.Close()
		// Recurse once: someone else just grew the pool, so the
		// fast path will find a usable conn.
		return s.acquireClient(profileName)
	}
	pc := &pooledClient{client: c, lastUsed: time.Now()}
	pc.inflight.Add(1)
	s.pool[profileName] = append(s.pool[profileName], pc)
	s.mu.Unlock()
	return s.leaseRelease(pc, profile)
}

// leaseRelease wraps a checked-out pooledClient with the release
// closure callers defer. Decrementing the counter is atomic; touching
// lastUsed needs the mutex because the GC reads it while holding mu.
func (s *daemonState) leaseRelease(pc *pooledClient, profile *config.Profile) (*sshx.Client, *config.Profile, func(), error) {
	return pc.client, profile, func() {
		pc.inflight.Add(-1)
		s.mu.Lock()
		pc.lastUsed = time.Now()
		s.mu.Unlock()
	}, nil
}

// evictFromSlot removes one pooledClient from the per-profile slice.
// Caller MUST hold s.mu. Does NOT close the client -- caller decides
// whether to close it (e.g. health-check failure closes; disconnect-
// all closes; GC closes).
func (s *daemonState) evictFromSlot(profileName string, target *pooledClient) {
	pcs := s.pool[profileName]
	for i, pc := range pcs {
		if pc == target {
			s.pool[profileName] = append(pcs[:i], pcs[i+1:]...)
			break
		}
	}
	if len(s.pool[profileName]) == 0 {
		delete(s.pool, profileName)
	}
}

// getClient is the back-compat shim for callers that hold a client
// for an indefinite duration (tunnel forwarders, autoconnect dial-
// probes). It acquires a lease and immediately releases it -- the
// caller gets the bare client but inflight isn't tracked, so multi-
// conn routing won't avoid the same conn for new sessions.
//
// New callers should use acquireClient with `defer release()`. This
// shim exists so the multi-conn refactor was incremental rather than
// rewriting every old callsite at once.
func (s *daemonState) getClient(profileName string) (*sshx.Client, *config.Profile, error) {
	c, p, release, err := s.acquireClient(profileName)
	if err != nil {
		return nil, p, err
	}
	release()
	return c, p, nil
}

func (s *daemonState) handleLs(req Request) Response {
	c, profile, release, err := s.acquireClient(req.Profile)
	if err != nil {
		return Response{OK: false, Err: err.Error()}
	}
	defer release()
	// Use the caller's cwd (sent in the request); fall back to default for
	// safety only.
	cwd := req.Cwd
	if cwd == "" {
		cwd = profile.GetDefaultCwd()
	}
	dirPart, basePart := sshx.SplitRemotePrefix(req.Prefix)
	target := sshx.RemoteListTarget(dirPart, cwd)

	// In-memory cache: same target for the same profile within 5s -> instant.
	listing, fromCache := s.cachedListing(profile.Name, target)
	if !fromCache {
		listing, err = s.runLs(c, target)
		if err != nil {
			return Response{OK: false, Err: err.Error()}
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
	return Response{OK: true, Entries: out}
}

// runLs runs `ls -1Ap` on the remote and returns the raw entries (one
// per line; dirs carry trailing "/").
func (s *daemonState) runLs(c *sshx.Client, target string) ([]string, error) {
	cmd := fmt.Sprintf("ls -1Ap -- %s", srvtty.ShQuotePath(target))
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, err
	}
	return parseLsOutput(res)
}

// parseLsOutput turns a `ls -1Ap` capture into entries. A non-zero
// exit (typically "No such file or directory" on a missing target)
// is surfaced as an error rather than swallowed as an empty success:
// otherwise list_dir on a bogus path is indistinguishable from an
// empty directory and the caller silently believes the dir exists.
// handleLs already maps this error to Response{OK:false}, which makes
// TryLs fall back to the direct-dial path that errors the same way,
// so the failure reaches the user instead of vanishing.
func parseLsOutput(res *sshx.RunCaptureResult) ([]string, error) {
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("ls failed (exit %d)", res.ExitCode)
		}
		return nil, fmt.Errorf("%s", msg)
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
	if time.Since(e.cached) > sshx.LsCacheTTL {
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
	c, _, release, err := s.acquireClient(profileName)
	if err != nil {
		return
	}
	defer release()
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

// sshx.LsCacheTTL was already defined in completion_remote.go for the file
// cache; reuse it for the daemon's in-memory cache too. (Defined there.)

func (s *daemonState) handleCd(req Request) Response {
	c, _, release, err := s.acquireClient(req.Profile)
	if err != nil {
		return Response{OK: false, Err: err.Error()}
	}
	defer release()
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
		srvtty.ShQuotePath(current), srvtty.ShQuotePath(target),
	)
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return Response{OK: false, Err: err.Error()}
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("cd failed (exit %d)", res.ExitCode)
		}
		return Response{OK: false, Err: stderr}
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		return Response{OK: false, Err: "remote did not return a path"}
	}
	return Response{OK: true, Cwd: strings.TrimSpace(lines[len(lines)-1])}
}

func (s *daemonState) handlePwd(req Request) Response {
	// pwd is purely local in the new model -- the CLI reads its session
	// directly. Kept for protocol completeness; returns the request's cwd.
	cwd := req.Cwd
	if cwd == "" {
		cwd = "~"
	}
	return Response{OK: true, Cwd: cwd}
}

// handleStreamRun runs `req.Command` on the remote and forwards stdout
// and stderr back to the client as base64-encoded chunks, line by line.
// Final frame {"k":"end","c":<exit>}. If the daemon's writer fails (the
// CLI disconnected, e.g. user hit Ctrl+C), the ssh session is closed so
// the remote process gets a SIGHUP and we don't leak it.
func (s *daemonState) handleStreamRun(req Request, wr *bufio.Writer, wrMu *sync.Mutex) {
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

	c, _, release, err := s.acquireClient(req.Profile)
	if err != nil {
		fail(err.Error())
		return
	}
	defer release()
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
		full = fmt.Sprintf("cd %s && (%s)", srvtty.ShQuotePath(cwd), req.Command)
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

func (s *daemonState) handleRun(req Request) Response {
	// Single-flight: collapse concurrent identical (profile, cwd,
	// command) calls onto one SSH round-trip. Read-write commands
	// merge too -- if Claude fires `rm /tmp/x` in parallel twice,
	// the second waits for the first instead of producing two
	// independent failures. The semantics ("the call ran once,
	// everyone sees the same result") match what the caller would
	// have got from a local mutex around the same code.
	key := s.runInflightKey(req.Profile, req.Cwd, req.Command)
	s.runMu.Lock()
	if existing, ok := s.runInflight[key]; ok {
		s.runMu.Unlock()
		<-existing.done
		return existing.resp
	}
	entry := &runInflightEntry{done: make(chan struct{})}
	s.runInflight[key] = entry
	s.runMu.Unlock()

	defer func() {
		s.runMu.Lock()
		// Only evict if we're still the owner -- a paranoid guard for
		// the unlikely case where the key recycles before close(done).
		if cur, ok := s.runInflight[key]; ok && cur == entry {
			delete(s.runInflight, key)
		}
		s.runMu.Unlock()
		close(entry.done)
	}()

	c, _, release, err := s.acquireClient(req.Profile)
	if err != nil {
		entry.resp = Response{OK: false, Err: err.Error()}
		return entry.resp
	}
	defer release()
	cwd := req.Cwd
	res, err := c.RunCapture(req.Command, cwd)
	if err != nil {
		entry.resp = Response{OK: false, Err: err.Error()}
		return entry.resp
	}
	entry.resp = Response{
		OK:       true,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		Cwd:      cwd,
	}
	return entry.resp
}

// runInflightKey builds the single-flight identity used by handleRun.
// NUL separators keep the boundaries unambiguous: "a", "b", "cd" and
// "a", "bc", "d" both join to "a\x00b\x00cd" / "a\x00bc\x00d" without
// colliding. profile MUST be the post-resolution name (not "") so
// empty-string defaults don't share a single inflight slot.
func (s *daemonState) runInflightKey(profile, cwd, cmd string) string {
	return profile + "\x00" + cwd + "\x00" + cmd
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
	var toClose []*sshx.Client
	s.mu.Lock()
	for name, pcs := range s.pool {
		// Keep busy conns; close idle ones individually. An idle slot
		// inside a 4-conn pool can be GC'd without taking the other 3
		// down with it.
		kept := pcs[:0]
		for _, pc := range pcs {
			if pc.inflight.Load() == 0 && now.Sub(pc.lastUsed) > connIdleTTL {
				toClose = append(toClose, pc.client)
				continue
			}
			kept = append(kept, pc)
		}
		if len(kept) == 0 {
			delete(s.pool, name)
		} else {
			s.pool[name] = kept
		}
	}
	// Drop expired ls cache entries. Without this the map's keys grow
	// without bound across a long-running daemon (every distinct directory
	// ever tab-completed sticks around forever, even though the TTL check
	// in cachedListing makes them functionally dead).
	for k, e := range s.lsCache {
		if now.Sub(e.cached) > sshx.LsCacheTTL {
			delete(s.lsCache, k)
		}
	}
	idle := now.Sub(s.lastReq) > daemonIdleTTL
	s.mu.Unlock()
	// Close the evicted conns outside the mutex -- Close() can block
	// on a slow socket teardown and we shouldn't hold up new
	// acquires for that. Nil-guarded so test fixtures that build
	// bare pooledClient values (no real SSH) don't panic.
	for _, c := range toClose {
		if c != nil {
			_ = c.Close()
		}
	}
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

// waitDaemonGone polls until Ping() reports no daemon, or the
// deadline expires. Used after a shutdown RPC so a follow-up
// Ensure() definitely sees a clean slate instead of racing
// with the old daemon's teardown.
func waitDaemonGone(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if !Ping() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// evictPooledClient drops every pooled SSH connection for
// `profileName` without waiting for the idle-TTL GC. Used by the
// tunnel forwarder after a transport crash so the next dial of the
// same profile builds a fresh connection instead of reusing the
// dead one. With multi-conn pools, all slots for the profile are
// closed; a transport crash typically takes the whole connection
// down anyway, but if one slot survived the next acquire will dial
// fresh capacity into the empty pool.
func (s *daemonState) evictPooledClient(profileName string) {
	if profileName == "" {
		return
	}
	s.mu.Lock()
	pcs := s.pool[profileName]
	delete(s.pool, profileName)
	s.mu.Unlock()
	for _, pc := range pcs {
		if pc.client != nil {
			_ = pc.client.Close()
		}
	}
}

// handleDisconnect drops the pooled SSH client for req.Profile and
// evicts every ls-cache row keyed on that profile. The next call
// referencing the profile will re-dial cold. Tunnels are NOT torn
// down -- they hold their own *sshx.Client handed out by getClient,
// and stopping them is `srv tunnel down <name>`'s job.
//
// Response.OK is true iff a pooled client was actually present and
// closed. The CLI uses that to render "disconnected" vs "(not
// connected)" without an extra round-trip.
func (s *daemonState) handleDisconnect(req Request) Response {
	if req.Profile == "" {
		return Response{OK: false, Err: "profile required"}
	}
	s.mu.Lock()
	pcs := s.pool[req.Profile]
	delete(s.pool, req.Profile)
	cachePrefix := req.Profile + "\x00"
	for k := range s.lsCache {
		if strings.HasPrefix(k, cachePrefix) {
			delete(s.lsCache, k)
		}
	}
	s.mu.Unlock()
	hadAny := len(pcs) > 0
	for _, pc := range pcs {
		if pc.client != nil {
			_ = pc.client.Close()
		}
	}
	return Response{
		OK:  hadAny,
		Err: "", // ok=false here just means "wasn't connected"; not a failure
	}
}

// handleDisconnectAll iterates the pool, closes every client, and
// wipes the ls cache. Used by `srv disconnect --all`. Returns the
// list of profiles whose connections were closed so the CLI can
// list them.
func (s *daemonState) handleDisconnectAll(req Request) Response {
	s.mu.Lock()
	freed := make([]string, 0, len(s.pool))
	pooled := make([]*pooledClient, 0, len(s.pool))
	for name, pcs := range s.pool {
		freed = append(freed, name)
		pooled = append(pooled, pcs...)
	}
	s.pool = map[string][]*pooledClient{}
	// Wipe the full ls cache -- it was keyed on the profiles we
	// just disconnected (plus possibly cold ones that haven't been
	// pooled lately). Rebuild on demand.
	s.lsCache = map[string]*lsCacheEntry{}
	s.mu.Unlock()
	for _, pc := range pooled {
		if pc.client != nil {
			_ = pc.client.Close()
		}
	}
	return Response{OK: true, Profiles: freed}
}

func (s *daemonState) closeAll() {
	// Stop tunnels first: their listeners hold references to pooled
	// clients, so closing the pool before the tunnels would race the
	// forwarder goroutines into "use of closed network connection"
	// errors during shutdown.
	s.stopAllTunnels()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pcs := range s.pool {
		for _, pc := range pcs {
			if pc.client != nil {
				_ = pc.client.Close()
			}
		}
	}
	s.pool = nil
}
