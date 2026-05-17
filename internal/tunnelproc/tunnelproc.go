// Package tunnelproc hosts the "one tunnel per process" lifecycle:
// spawning a detached `srv _tunnel_run <name>` subprocess, the
// runtime body that subprocess executes, and the on-disk status
// files (`~/.srv/tunnels/<name>.json`) the CLI uses to enumerate +
// stop independent tunnels.
//
// The motivating asymmetry: daemon-hosted tunnels die with the
// daemon. For tunnels that are critical infrastructure (long-lived
// db port-forwards, observability bridges), restarting the daemon
// for an unrelated config change is unacceptable downtime. The
// independent path trades resource sharing (every independent
// tunnel costs its own SSH connection + ~20MB Go runtime) for
// isolation from the daemon's lifecycle.
package tunnelproc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/platform"
	"srv/internal/srvpath"
)

// tunnelsDir is where status + pid files live. Created on demand
// with 0700 perms because the file content reveals which hosts /
// ports we're forwarding to.
func tunnelsDir() string {
	return filepath.Join(srvpath.Dir(), "tunnels")
}

// StatusPath returns the JSON status file path for a tunnel name.
func StatusPath(name string) string {
	return filepath.Join(tunnelsDir(), name+".json")
}

// Status mirrors daemon.TunnelInfo so callers that already speak
// that shape can union daemon-hosted + independent into one map
// without an extra translation layer.
//
// PIDStart is the OS-reported creation time of the subprocess in
// unix nanoseconds, used to detect PID reuse: if a host runs long
// enough for PIDs to wrap, a crashed tunnel's status file might
// otherwise report "alive" for some unrelated new process. With
// PIDStart we also verify the running process's creation time
// matches what we wrote, catching the reuse case. Zero means the
// platform-specific lookup failed (e.g. macOS without /proc) and
// we fall back to PID-only liveness, which is the historical
// behaviour.
type Status struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Spec     string `json:"spec"`
	Profile  string `json:"profile,omitempty"`
	Listen   string `json:"listen,omitempty"`
	Started  int64  `json:"started,omitempty"`
	OnDemand bool   `json:"on_demand,omitempty"`
	PID      int    `json:"pid"`
	PIDStart int64  `json:"pid_start,omitempty"`
}

// WriteStatus persists `st` to ~/.srv/tunnels/<name>.json. Created
// 0600 so the file is readable only by the owning user; the
// containing directory is similarly 0700. Caller is the tunnel
// subprocess; it writes once at boot after the listener has bound
// successfully and once more (atomic replace) any time Listen
// changes (lazy bind, port renegotiation).
func WriteStatus(st Status) error {
	dir := tunnelsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tunnelproc: mkdir %s: %v", dir, err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace via tmp + rename so a concurrent reader never
	// sees a half-written file.
	tmp := filepath.Join(dir, "."+st.Name+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, StatusPath(st.Name))
}

// ReadStatus loads the status file for `name`, or nil if it doesn't
// exist. Errors propagate for "exists but unreadable / unparseable"
// cases so callers (`srv tunnel list`) can surface them as a real
// problem rather than silently treating the tunnel as not running.
func ReadStatus(name string) (*Status, error) {
	b, err := os.ReadFile(StatusPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("tunnelproc: parse %s: %v", StatusPath(name), err)
	}
	return &st, nil
}

// RemoveStatus deletes the status file. Called by the tunnel
// subprocess during its clean-shutdown deferred sequence and by
// `srv tunnel down` after a kill confirms the PID is gone (so a
// stale file doesn't poison the next list).
func RemoveStatus(name string) {
	_ = os.Remove(StatusPath(name))
}

// ListStatuses returns every <name>.json found, filtered to entries
// whose PID is still alive. A status file whose PID has died (crash,
// host reboot) is auto-cleaned so successive `srv tunnel list`
// readings don't ghost-report it as running forever.
func ListStatuses() (map[string]*Status, error) {
	out := map[string]*Status{}
	entries, err := os.ReadDir(tunnelsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		st, rerr := ReadStatus(name)
		if rerr != nil || st == nil {
			continue
		}
		if !pidAliveMatch(st.PID, st.PIDStart) {
			// Stale file from a previous crash or a recycled PID.
			// Remove so future listings stay accurate; not fatal
			// if remove fails (e.g. perms).
			RemoveStatus(name)
			continue
		}
		out[name] = st
	}
	return out, nil
}

// Spawn launches `srv _tunnel_run <name>` as a detached subprocess.
// Caller is `srv tunnel up <name>` when def.Independent is true.
// Returns nil on successful start (no wait for ready); caller can
// poll StatusPath / ListStatuses to know when the listener has come
// up. The detach attrs match the daemon's so the subprocess survives
// the CLI parent exiting.
func Spawn(name string) error {
	if _, err := ReadStatus(name); err == nil {
		// Pre-existing status -- check liveness. If the prior PID
		// is dead (or reused since), ListStatuses would have
		// removed the file, but we're called per-name here so we
		// re-check explicitly.
		if st, _ := ReadStatus(name); st != nil && pidAliveMatch(st.PID, st.PIDStart) {
			return fmt.Errorf("tunnel %q already running (pid %d)", name, st.PID)
		}
		RemoveStatus(name)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "_tunnel_run", name)
	cmd.Stdin = nil
	// Log subprocess output to a per-tunnel log file so a busted
	// dial / listener leaves a paper trail even after the parent
	// process is gone. Best-effort: failing to open the log doesn't
	// stop the spawn.
	if err := os.MkdirAll(tunnelsDir(), 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(tunnelsDir(), name+".log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
	}
	platform.Proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn _tunnel_run: %v", err)
	}
	// Don't wait -- the OS reaps. With the platform-specific detach
	// attrs the child outlives this parent.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

// Stop reads the status file for `name`, kills the recorded PID,
// and waits up to ~2s for the status file to disappear (the
// subprocess removes it on graceful shutdown). Returns nil if the
// tunnel was running and successfully terminated; an error if no
// status file or PID exists, or the kill failed.
func Stop(name string) error {
	st, err := ReadStatus(name)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("tunnel %q not running as independent process", name)
	}
	if err := platform.Proc.SignalTerminate(st.PID); err != nil {
		// Kill / ctrl-break may fail if the pid is already gone.
		// Treat "process gone" as success and clean up the status
		// file rather than leaving the user with a confusing error.
		if !platform.Proc.PIDAlive(st.PID) {
			RemoveStatus(name)
			return nil
		}
		return fmt.Errorf("signal pid %d: %v", st.PID, err)
	}
	// Best-effort wait for the subprocess's deferred RemoveStatus to
	// fire so the user gets synchronous confirmation. Stale-file
	// cleanup at the end covers cases where shutdown was abrupt
	// (Kill fallback) and the subprocess didn't get to run defers.
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(StatusPath(name)); os.IsNotExist(err) {
			return nil
		}
		if !platform.Proc.PIDAlive(st.PID) {
			RemoveStatus(name)
			return nil
		}
		sleepMillis(50)
	}
	RemoveStatus(name)
	return nil
}

// pidAliveMatch combines platform.Proc's PIDAlive + PIDStartTime
// into a single "is this status file's recorded process really
// still ours?" check. When expectedStart is 0 (lookup failed at
// write time, or a pre-feature status file), it degrades to a
// PID-only liveness probe. When non-zero, the running PID's actual
// creation time must match within 2 seconds (clock-skew slack);
// any larger drift means the PID was recycled by an unrelated
// process and we report dead.
func pidAliveMatch(pid int, expectedStart int64) bool {
	if !platform.Proc.PIDAlive(pid) {
		return false
	}
	if expectedStart == 0 {
		return true
	}
	got, ok := platform.Proc.PIDStartTime(pid)
	if !ok {
		return true
	}
	diff := got - expectedStart
	if diff < 0 {
		diff = -diff
	}
	return diff < int64(2*1e9)
}

// DefFromConfig fetches the tunnel def from the user's config. Used
// by the _tunnel_run subprocess at startup; returns a clean error
// for missing tunnels / missing config so the subprocess's log file
// has a diagnosable line instead of a panic.
func DefFromConfig(name string) (*config.TunnelDef, error) {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return nil, fmt.Errorf("load config: %v", err)
	}
	def, ok := cfg.Tunnels[name]
	if !ok {
		return nil, fmt.Errorf("tunnel %q not defined", name)
	}
	return def, nil
}

// FormatStartedAgo renders a started-at unix timestamp as an
// English "5m ago" / "1h12m ago" string for `srv tunnel list`.
// Pulled out so the CLI side and the dashboard share one
// formatter; otherwise every call site renders it slightly
// differently and the user notices.
func FormatStartedAgo(started int64) string {
	return formatStartedAgo(started)
}
