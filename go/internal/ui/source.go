package ui

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"srv/internal/remote"
	"srv/internal/tunnel"
	"time"
)

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
	return Snapshot{
		Cfg:        cfg,
		Jobs:       js,
		MCP:        mcp,
		DaemonResp: &daemon.Response{OK: true, Profiles: []string{"美国备用", "tokyo-demo"}, Uptime: int64(2 * time.Hour / time.Second)},
		// Empty maps -- the demo doesn't simulate running tunnels so
		// renderers see "stopped" for each saved tunnel.
		TunnelActive: map[string]daemon.TunnelInfo{},
		TunnelErrs:   map[string]string{},
		CapturedAt:   time.Now(),
	}
}

func (demoSource) Liveness(js []*jobs.Record) map[string]bool {
	// In the demo we want every job to render as alive so the panel
	// shows the full sample row set. A real probe would dial remotes.
	out := make(map[string]bool, len(js))
	for _, j := range js {
		out[j.ID] = true
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
