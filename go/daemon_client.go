package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// daemonDial connects to the daemon socket with a short timeout. Returns
// nil + nil error if no daemon is reachable (the caller falls back to
// direct dial). Returns an error for unexpected failures.
func daemonDial(timeout time.Duration) net.Conn {
	conn, err := net.DialTimeout("unix", daemonSocketPath(), timeout)
	if err != nil {
		return nil
	}
	return conn
}

// daemonPing returns true if a daemon is alive at the socket path.
func daemonPing() bool {
	conn := daemonDial(500 * time.Millisecond)
	if conn == nil {
		return false
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "ping"}, time.Second)
	return err == nil && resp.OK
}

// daemonCall writes a request and reads one response. Caller owns the conn.
func daemonCall(conn net.Conn, req daemonRequest, deadline time.Duration) (*daemonResponse, error) {
	if deadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(deadline))
		defer conn.SetDeadline(time.Time{})
	}
	if req.V == 0 {
		req.V = DaemonProtoVersion
	}
	if req.ID == 0 {
		req.ID = int(time.Now().UnixNano() & 0x7fffffff)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp daemonResponse
	if jerr := json.Unmarshal([]byte(line), &resp); jerr != nil {
		return nil, jerr
	}
	if resp.V > DaemonProtoVersion {
		fmt.Fprintf(os.Stderr,
			"srv: daemon protocol v%d, this srv knows v%d. Restart the daemon (`srv daemon stop`) or upgrade srv.\n",
			resp.V, DaemonProtoVersion)
	}
	return &resp, nil
}

// tryDaemonLs short-circuits the cold SSH handshake when a daemon is
// running and has the profile already pooled. Returns (entries, true) on
// success; (nil, false) when the caller should fall back to direct ssh.
func tryDaemonLs(profileName, cwd, prefix string) ([]string, bool) {
	conn := daemonDial(200 * time.Millisecond)
	if conn == nil {
		return nil, false
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{
		Op: "ls", Profile: profileName, Cwd: cwd, Prefix: prefix,
	}, 5*time.Second)
	if err != nil || resp == nil || !resp.OK {
		return nil, false
	}
	return resp.Entries, true
}

// tryDaemonStreamRun runs `command` via the daemon's pooled SSH connection
// and forwards the remote stdout/stderr to the local terminal in real
// time as chunks arrive (no buffering at the daemon, so `tail -f` and
// long-running commands behave naturally). Returns (exitCode, true) on
// success; (0, false) when the caller should fall back to direct dial.
func tryDaemonStreamRun(profileName, cwd, command string) (int, bool) {
	conn := daemonDial(200 * time.Millisecond)
	if conn == nil {
		if !ensureDaemon() {
			return 0, false
		}
		conn = daemonDial(200 * time.Millisecond)
		if conn == nil {
			return 0, false
		}
	}
	defer conn.Close()

	req := daemonRequest{
		V:       DaemonProtoVersion,
		ID:      int(time.Now().UnixNano() & 0x7fffffff),
		Op:      "stream_run",
		Profile: profileName,
		Cwd:     cwd,
		Command: command,
	}
	b, err := json.Marshal(req)
	if err != nil {
		return 0, false
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return 0, false
	}

	rd := bufio.NewReader(conn)
	versionChecked := false
	for {
		line, rerr := rd.ReadString('\n')
		if rerr != nil {
			// Daemon disconnected mid-stream -- something went wrong;
			// fall back to direct dial only if no output has been written.
			// (We can't unwrite local stdout, so just report failure.)
			return 0, false
		}
		var ch streamChunk
		if jerr := json.Unmarshal([]byte(line), &ch); jerr != nil {
			continue
		}
		if !versionChecked {
			versionChecked = true
			if ch.V > DaemonProtoVersion {
				fmt.Fprintf(os.Stderr,
					"srv: daemon protocol v%d, this srv knows v%d. Restart the daemon or upgrade srv.\n",
					ch.V, DaemonProtoVersion)
			}
		}
		switch ch.K {
		case "out":
			if data, derr := base64.StdEncoding.DecodeString(ch.B); derr == nil {
				_, _ = os.Stdout.Write(data)
			}
		case "err":
			if data, derr := base64.StdEncoding.DecodeString(ch.B); derr == nil {
				_, _ = os.Stderr.Write(data)
			}
		case "end":
			return ch.C, true
		case "fail":
			// Daemon couldn't even start the command. Fall back.
			return 0, false
		}
	}
}

// tryDaemonRunCapture runs `command` via the daemon's pooled SSH and
// returns the captured stdout/stderr/exit_code. Returns (nil, false) when
// no daemon is reachable or it answered with a non-OK -- caller should
// fall back to a direct dial in either case.
//
// Unlike tryDaemonStreamRun this is for the MCP `run` tool path: the
// caller wants a single buffered result, not real-time streaming. Reusing
// the daemon's pooled SSH avoids the ~2.7s cold handshake every call.
func tryDaemonRunCapture(profileName, cwd, command string) (*RunCaptureResult, bool) {
	conn := daemonDial(200 * time.Millisecond)
	if conn == nil {
		if !ensureDaemon() {
			return nil, false
		}
		conn = daemonDial(200 * time.Millisecond)
		if conn == nil {
			return nil, false
		}
	}
	defer conn.Close()
	// deadline=0: arbitrary-duration commands shouldn't get cut off mid-run.
	resp, err := daemonCall(conn, daemonRequest{
		Op: "run", Profile: profileName, Cwd: cwd, Command: command,
	}, 0)
	if err != nil || resp == nil || !resp.OK {
		return nil, false
	}
	return &RunCaptureResult{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
		Cwd:      cwd,
	}, true
}

// tryDaemonCd validates the target cwd via the daemon.
//
// Returns:
//   - used=false: no daemon reachable; caller should fall back to direct dial.
//   - used=true, err=nil:   success; newCwd is the validated absolute path.
//   - used=true, err!=nil:  daemon answered with a definitive failure
//     (e.g. "no such directory"). Caller should propagate this error
//     instead of retrying direct, since direct would just re-fail.
//
// Caller is responsible for persisting newCwd to its session store -- the
// daemon doesn't touch sessions.json.
func tryDaemonCd(profileName, currentCwd, target string) (newCwd string, used bool, err error) {
	conn := daemonDial(200 * time.Millisecond)
	if conn == nil {
		if !ensureDaemon() {
			return "", false, nil
		}
		conn = daemonDial(200 * time.Millisecond)
		if conn == nil {
			return "", false, nil
		}
	}
	defer conn.Close()
	resp, callErr := daemonCall(conn, daemonRequest{
		Op: "cd", Profile: profileName, Cwd: currentCwd, Path: target,
	}, 30*time.Second)
	if callErr != nil || resp == nil {
		// Network/protocol issue, treat as no-daemon -- direct dial may work.
		return "", false, nil
	}
	if !resp.OK {
		// Daemon definitively said no. Don't retry direct.
		return "", true, fmt.Errorf("%s", resp.Err)
	}
	return resp.Cwd, true, nil
}

func daemonClientStatus(args []string) int {
	asJSON := len(args) > 0 && args[0] == "--json"
	conn := daemonDial(time.Second)
	if conn == nil {
		if asJSON {
			fmt.Println(`{"running":false}`)
			return 1
		}
		fmt.Println("daemon: not running")
		return 1
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, 2*time.Second)
	if err != nil || resp == nil {
		fmt.Fprintln(os.Stderr, "daemon: status failed:", err)
		return 1
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"running":         true,
			"uptime_sec":      resp.Uptime,
			"profiles_pooled": resp.Profiles,
			"protocol":        resp.V,
		}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("running, uptime %ds\n", resp.Uptime)
	if len(resp.Profiles) == 0 {
		fmt.Println("pooled connections: (none)")
	} else {
		fmt.Println("pooled connections:")
		for _, p := range resp.Profiles {
			fmt.Println(" -", p)
		}
	}
	return 0
}

func daemonClientLogs() int {
	data, err := os.ReadFile(daemonLogPath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no daemon log)")
			return 0
		}
		fmt.Fprintln(os.Stderr, "daemon logs:", err)
		return 1
	}
	fmt.Print(string(data))
	return 0
}

func daemonClientPruneCache() int {
	dir := filepath.Join(ConfigDir(), "cache")
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "daemon prune-cache:", err)
		return 1
	}
	_ = os.MkdirAll(dir, 0o755)
	fmt.Println("cache pruned:", strings.TrimSpace(dir))
	return 0
}

func daemonClientStop() int {
	conn := daemonDial(time.Second)
	if conn == nil {
		fmt.Println("daemon: not running")
		return 0
	}
	defer conn.Close()
	resp, _ := daemonCall(conn, daemonRequest{Op: "shutdown"}, 2*time.Second)
	if resp != nil && resp.OK {
		fmt.Println("daemon: stopped")
		return 0
	}
	fmt.Fprintln(os.Stderr, "daemon: shutdown returned without ok")
	return 1
}
