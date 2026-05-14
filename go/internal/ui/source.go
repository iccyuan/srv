package ui

import (
	"fmt"
	"math"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"srv/internal/remote"
	"srv/internal/tunnel"
	"strconv"
	"strings"
	"time"
)

// statsProbe routes the Stats() shell command through the daemon
// when available, falling back to a direct dial when not. Returns
// (stdout, exitCode, ok). `ok=false` means we couldn't even get a
// reply -- caller surfaces "no remote" in the sparkline error slot.
func statsProbe(profile *config.Profile, cmd string) (string, int, bool) {
	if res, ok := daemon.TryRunCapture(profile.Name, "", cmd); ok && res != nil {
		return res.Stdout, res.ExitCode, true
	}
	res, err := remote.RunCapture(profile, "", cmd)
	if err != nil || res == nil {
		return "", 0, false
	}
	return res.Stdout, res.ExitCode, true
}

// Source is the dashboard's data + side-effect façade. Every piece
// of state the renderers care about (config, jobs, MCP status,
// daemon snapshot, tunnel status, liveness) flows through Snapshot
// and Liveness; every destructive action (kill, tunnel up/down/remove)
// flows through the four action methods. The live and demo paths are
// just two implementations that obey the same contract.
//
// This is the structural answer to "srv ui and srv ui demo can drift":
// previously the main loop branched on `if demo` to pick which API to
// dial; now both go through the same Source surface, so any divergence
// is necessarily in the data the source returned, not in the rendering
// or handling code that consumed it.
type Source interface {
	// Snapshot returns one atomic view of dashboard state. The main
	// loop calls this at most once per snapTTL. Implementations must
	// not return nil maps in TunnelActive / TunnelErrs -- empty maps
	// are fine; nil would panic at the lookup sites.
	Snapshot() Snapshot
	// Liveness probes "are these jobs still running" for the supplied
	// records. The map's keys are job IDs; missing entries mean
	// "unknown" (treated as alive by the renderer so we never hide a
	// row whose state we couldn't probe).
	Liveness(js []*jobs.Record) map[string]bool
	// JobLog fetches the tail of a detached job's remote log. Returns
	// lines (newest last) plus any error encountered. Callers wrap
	// this in a goroutine; Source impls are free to block.
	JobLog(j *jobs.Record) ([]string, error)
	// Action wrappers used by armConfirmFor. Each returns (msg, err)
	// where msg is shown on the dashboard status line on success.
	KillJob(j *jobs.Record) (string, error)
	TunnelUp(name string) (string, error)
	TunnelDown(name string) (string, error)
	TunnelRemove(name string, cfg *config.Config) (string, error)
	// IsDemo lets the header decide whether to print "DEMO simulated
	// data" instead of "live terminal view". The only intentional
	// place in the UI where demo and live are visibly distinguished;
	// everything else flows through the data surface above.
	IsDemo() bool
	// Stats returns a single CPU-load + memory-percent sample for the
	// remote, used by panelStats to draw a small sparkline of recent
	// trends. The dashboard calls this on its own cadence (every
	// statsInterval); the Source itself doesn't have to cache.
	Stats() StatsSample
}

// StatsSample is one snapshot of the remote's resource usage. CPULoad
// is the 1-minute load average (matches /proc/loadavg field 1).
// MemPercent is used/total*100. Err carries a one-line reason when
// sampling failed -- the renderer surfaces it under the sparkline.
type StatsSample struct {
	CPULoad    float64
	MemPercent float64
	When       time.Time
	Err        string
}

// Snapshot is the bundle Source.Snapshot returns. Fields are owned by
// the renderer after the call -- implementations should return fresh
// copies rather than handing back internal state.
type Snapshot struct {
	Cfg          *config.Config
	Jobs         []*jobs.Record
	MCP          mcplog.Status
	DaemonResp   *daemon.Response
	TunnelActive map[string]daemon.TunnelInfo
	TunnelErrs   map[string]string
	// CapturedAt records when this snapshot was assembled, so the
	// loop can decide whether to fold it into st.snapAt without
	// taking a second time.Now().
	CapturedAt time.Time
}

// --- live source -----------------------------------------------------

// liveSource talks to the real config / daemon / SSH / jobs.json on
// every Snapshot. The disk + daemon calls match what `srv ui` did
// before the refactor; nothing about timing or RPC shape changed.
type liveSource struct{}

// NewLiveSource is exported so tests / external embedders can build
// the live path explicitly. Cmd() picks it for non-demo invocations.
func NewLiveSource() Source { return &liveSource{} }

func (liveSource) IsDemo() bool { return false }

func (liveSource) Snapshot() Snapshot {
	cfg, _ := config.Load()
	mcp := mcplog.Read()
	if len(mcp.ActivePIDs) > 0 {
		active := map[int]struct{}{}
		for _, pid := range mcp.ActivePIDs {
			active[pid] = struct{}{}
		}
		kept := mcp.RecentTools[:0]
		for _, t := range mcp.RecentTools {
			if t.PID == 0 {
				kept = append(kept, t)
				continue
			}
			if _, ok := active[t.PID]; ok {
				kept = append(kept, t)
			}
		}
		mcp.RecentTools = kept
	}
	active, errs := tunnel.LoadStatuses()
	if active == nil {
		active = map[string]daemon.TunnelInfo{}
	}
	if errs == nil {
		errs = map[string]string{}
	}
	return Snapshot{
		Cfg:          cfg,
		Jobs:         currentJobs(),
		MCP:          mcp,
		DaemonResp:   fetchDaemonStatusForUI(),
		TunnelActive: active,
		TunnelErrs:   errs,
		CapturedAt:   time.Now(),
	}
}

func (liveSource) Liveness(js []*jobs.Record) map[string]bool {
	if len(js) == 0 {
		return map[string]bool{}
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return map[string]bool{}
	}
	lister := func(profName string) (map[string]bool, bool) {
		prof, ok := cfg.Profiles[profName]
		if !ok {
			return nil, false
		}
		capture := func(cmd string) (string, int, bool) {
			res, err := remote.RunCapture(prof, "", cmd)
			if err != nil || res == nil {
				return "", 0, false
			}
			return res.Stdout, res.ExitCode, true
		}
		markers := jobs.RemoteExitMarkers(capture)
		return markers, markers != nil
	}
	return jobs.CheckLiveness(js, lister)
}

func (liveSource) JobLog(j *jobs.Record) ([]string, error) {
	return fetchJobLogLines(j)
}

func (liveSource) KillJob(j *jobs.Record) (string, error) {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return "", fmt.Errorf("load config: %v", err)
	}
	return uiKillJob(j, cfg)
}

func (liveSource) TunnelUp(name string) (string, error)   { return uiTunnelUp(name) }
func (liveSource) TunnelDown(name string) (string, error) { return uiTunnelDown(name) }

func (liveSource) TunnelRemove(name string, cfg *config.Config) (string, error) {
	return uiTunnelRemove(name, cfg)
}

func (liveSource) Stats() StatsSample {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return StatsSample{When: time.Now(), Err: "no config"}
	}
	name, profile, err := config.Resolve(cfg, "")
	_ = name
	if err != nil {
		return StatsSample{When: time.Now(), Err: "no profile"}
	}
	// One-liner Linux probe: load avg + memory used%.
	cmd := `awk '{print $1}' /proc/loadavg && awk 'BEGIN{used=0; total=1} /^MemTotal:/{total=$2} /^MemAvailable:/{avail=$2} END{if(total>0) printf "%.2f", (total-avail)/total*100}' /proc/meminfo`
	stdout, exitCode, ok := statsProbe(profile, cmd)
	if !ok {
		return StatsSample{When: time.Now(), Err: "no remote"}
	}
	if exitCode != 0 {
		return StatsSample{When: time.Now(), Err: "remote err"}
	}
	out := strings.TrimSpace(stdout)
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return StatsSample{When: time.Now(), Err: "parse"}
	}
	cpu, _ := strconv.ParseFloat(parts[0], 64)
	mem, _ := strconv.ParseFloat(parts[1], 64)
	return StatsSample{CPULoad: cpu, MemPercent: mem, When: time.Now()}
}

// --- demo source -----------------------------------------------------

// demoSource serves the fixed demo dataset. Actions return canned
// "demo X acknowledged" messages without touching anything real. The
// data is rebuilt on each Snapshot so relative times ("8m ago") stay
// fresh.
type demoSource struct {
	base *config.Config // honoured for Lang / Hints only; everything else demo
}

// NewDemoSource builds the demo Source. `base` is the user's actual
// config; the demo carries its Lang / Hints over so the screenshot
// matches the user's locale, but otherwise ignores it.
func NewDemoSource(base *config.Config) Source { return &demoSource{base: base} }

func (demoSource) IsDemo() bool { return true }

func (d *demoSource) Snapshot() Snapshot {
	cfg, js, mcp := demoDashboardData(d.base)
	// Exercise the renderers' state branches so the demo doubles as a
	// screenshot fixture for every visual state, not just the happy
	// path. Each entry below maps to a live-only branch the
	// pre-refactor demo never showed:
	//   - 数据库 running          -> "listen=..." extra
	//   - proxy errored           -> red "failed: ..." row
	//   - 监控 / api-lazy stopped -> default state
	active := map[string]daemon.TunnelInfo{
		"数据库": {Name: "数据库", Type: "local", Spec: "15432:db.internal:5432",
			Profile: "美国备用", Listen: "127.0.0.1:15432",
			Started: time.Now().Add(-37 * time.Minute).Unix()},
	}
	errs := map[string]string{
		"proxy": "ssh connection closed: EOF after 3 retries",
	}
	return Snapshot{
		Cfg:          cfg,
		Jobs:         js,
		MCP:          mcp,
		DaemonResp:   &daemon.Response{OK: true, Profiles: []string{"美国备用", "tokyo-demo"}, Uptime: int64(2 * time.Hour / time.Second)},
		TunnelActive: active,
		TunnelErrs:   errs,
		CapturedAt:   time.Now(),
	}
}

func (demoSource) Liveness(js []*jobs.Record) map[string]bool {
	// Most demo jobs are alive so the panel shows a full row set,
	// but mark the oldest one as exited (it ran `npm run build` 42m
	// ago -- realistic for a tail-end build) so the JOBS title's
	// "+ N hidden" branch is exercised in the screenshot.
	out := make(map[string]bool, len(js))
	for i, j := range js {
		out[j.ID] = i != 0 // first record (demo-job-0001) is exited
	}
	return out
}

func (demoSource) JobLog(j *jobs.Record) ([]string, error) {
	return []string{
		"[demo] " + j.Cmd,
		"[demo] starting...",
		"[demo] ok 1/5",
		"[demo] ok 2/5",
		"[demo] (live fetch suppressed in demo mode)",
	}, nil
}

func (demoSource) KillJob(j *jobs.Record) (string, error) {
	return "demo kill acknowledged; no process changed", nil
}
func (demoSource) TunnelUp(name string) (string, error) {
	return "demo tunnel started; no daemon changed", nil
}
func (demoSource) TunnelDown(name string) (string, error) {
	return "demo tunnel stopped; no daemon changed", nil
}
func (demoSource) TunnelRemove(name string, _ *config.Config) (string, error) {
	return "demo tunnel removed; no config changed", nil
}

func (demoSource) Stats() StatsSample {
	// Deterministic-but-lively sample so the demo screenshot shows a
	// non-trivial sparkline. Two slow sinusoids offset by π/3 so the
	// CPU and MEM rows aren't identical -- helps users see the panel
	// has two independent series.
	now := time.Now()
	phase := float64(now.Unix()%180) / 180.0 // 3-minute period
	cpu := 0.6 + 0.8*math.Sin(phase*2*math.Pi)
	mem := 50 + 25*math.Sin(phase*2*math.Pi+math.Pi/3)
	if cpu < 0 {
		cpu = 0
	}
	if mem < 0 {
		mem = 0
	}
	if mem > 100 {
		mem = 100
	}
	return StatsSample{CPULoad: cpu, MemPercent: mem, When: now}
}
