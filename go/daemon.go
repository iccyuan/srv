package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
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

type daemonRequest struct {
	ID      int    `json:"id"`
	Op      string `json:"op"`
	Profile string `json:"profile,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
}

type daemonResponse struct {
	ID       int      `json:"id"`
	OK       bool     `json:"ok"`
	Err      string   `json:"err,omitempty"`
	Entries  []string `json:"entries,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Profiles []string `json:"profiles_pooled,omitempty"`
	Uptime   int64    `json:"uptime_sec,omitempty"`
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
	startedAt time.Time
	// lastReq updated on every request; idle-shutdown checks this.
	lastReq time.Time
	stopCh  chan struct{}
}

const (
	// How long an unused per-profile SSH connection stays open. Closed
	// connections are re-dialed on next use.
	connIdleTTL = 10 * time.Minute
	// Whole-daemon idle shutdown threshold.
	daemonIdleTTL = 30 * time.Minute
	// Cleanup tick.
	gcInterval = 60 * time.Second
)

func daemonSocketPath() string {
	return filepath.Join(ConfigDir(), "daemon.sock")
}

// cmdDaemon starts the daemon listener (foreground). Ctrl-C stops it
// cleanly and unlinks the socket file.
func cmdDaemon(args []string) int {
	// Subcommands of `srv daemon` itself: status / stop.
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return daemonClientStatus()
		case "stop":
			return daemonClientStop()
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
			return 1
		}
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon listen:", err)
		return 1
	}
	_ = os.Chmod(sockPath, 0o600)
	fmt.Fprintln(os.Stderr, "srv daemon listening at", sockPath)

	state := &daemonState{
		pool:      map[string]*pooledClient{},
		lsCache:   map[string]*lsCacheEntry{},
		startedAt: time.Now(),
		lastReq:   time.Now(),
		stopCh:    make(chan struct{}),
	}

	// Background gc: close idle connections, exit if whole daemon idle.
	go state.runGC(listener)

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nsrv daemon: stopping.")
		close(state.stopCh)
		_ = listener.Close()
	}()

	// Accept loop.
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-state.stopCh:
				state.closeAll()
				_ = os.Remove(sockPath)
				return 0
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
			s.write(wr, daemonResponse{OK: false, Err: "parse: " + jerr.Error()})
			continue
		}
		s.mu.Lock()
		s.lastReq = time.Now()
		s.mu.Unlock()
		resp := s.dispatch(req)
		resp.ID = req.ID
		s.write(wr, resp)
		if req.Op == "shutdown" {
			close(s.stopCh)
			return
		}
	}
}

func (s *daemonState) write(wr *bufio.Writer, resp daemonResponse) {
	b, _ := json.Marshal(resp)
	wr.Write(b)
	wr.WriteByte('\n')
	wr.Flush()
}

func (s *daemonState) dispatch(req daemonRequest) daemonResponse {
	defer func() {
		if r := recover(); r != nil {
			// Final fallback; shouldn't be hit normally.
			fmt.Fprintln(os.Stderr, "daemon panic:", r)
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
	}
	return daemonResponse{OK: false, Err: "unknown op: " + req.Op}
}

// getClient returns a pooled (or freshly dialed) Client for the named
// profile. Errors propagate to the caller; the dialed client stays in the
// pool until idle-collected.
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if pc, ok := s.pool[profileName]; ok && pc.client != nil && pc.client.Conn != nil {
		pc.lastUsed = time.Now()
		return pc.client, profile, nil
	}
	c, err := Dial(profile)
	if err != nil {
		return nil, profile, err
	}
	s.pool[profileName] = &pooledClient{client: c, lastUsed: time.Now()}
	return c, profile, nil
}

func (s *daemonState) handleLs(req daemonRequest) daemonResponse {
	c, profile, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	cwd := GetCwd(profile.Name, profile)
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
	defer func() { _ = recover() }()
	c, _, err := s.getClient(profileName)
	if err != nil {
		return
	}
	parent = strings.TrimRight(parent, "/") + "/"
	for _, e := range entries {
		if !strings.HasSuffix(e, "/") {
			continue
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
	}
}

// lsCacheTTL was already defined in completion_remote.go for the file
// cache; reuse it for the daemon's in-memory cache too. (Defined there.)

func (s *daemonState) handleCd(req daemonRequest) daemonResponse {
	_, profile, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	newCwd, err := changeRemoteCwd(profile.Name, profile, req.Path)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	return daemonResponse{OK: true, Cwd: newCwd}
}

func (s *daemonState) handlePwd(req daemonRequest) daemonResponse {
	_, profile, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	return daemonResponse{OK: true, Cwd: GetCwd(profile.Name, profile)}
}

func (s *daemonState) handleRun(req daemonRequest) daemonResponse {
	c, profile, err := s.getClient(req.Profile)
	if err != nil {
		return daemonResponse{OK: false, Err: err.Error()}
	}
	cwd := GetCwd(profile.Name, profile)
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

func (s *daemonState) runGC(listener net.Listener) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.gc(listener)
		}
	}
}

func (s *daemonState) gc(listener net.Listener) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, pc := range s.pool {
		if now.Sub(pc.lastUsed) > connIdleTTL {
			_ = pc.client.Close()
			delete(s.pool, name)
		}
	}
	if now.Sub(s.lastReq) > daemonIdleTTL {
		fmt.Fprintln(os.Stderr, "srv daemon: idle for",
			daemonIdleTTL, "-- shutting down.")
		_ = listener.Close()
	}
}

func (s *daemonState) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pc := range s.pool {
		_ = pc.client.Close()
	}
	s.pool = nil
}
