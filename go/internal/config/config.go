package config

import (
	"encoding/json"
	"fmt"
	"os"
	"srv/internal/project"
	"srv/internal/session"
	"time"

	"srv/internal/i18n"
	"srv/internal/srvio"
	"srv/internal/srvpath"
)

// Path helpers (Dir / Config / Sessions / Jobs) live in
// srv/internal/srvpath now -- everything that needs to know where
// srv keeps its files imports srvpath directly rather than chaining
// through package main.

// Profile mirrors the Python profile dict. Keys with omitempty/zero-value
// semantics:
//   - Multiplex/Compression default to true if not present (handled by
//     accessor methods)
//   - Most ints/strings have sensible zero defaults applied at use time
type Profile struct {
	Host              string            `json:"host"`
	User              string            `json:"user,omitempty"`
	Port              int               `json:"port,omitempty"`
	IdentityFile      string            `json:"identity_file,omitempty"`
	DefaultCwd        string            `json:"default_cwd,omitempty"`
	Multiplex         *bool             `json:"multiplex,omitempty"`
	Compression       *bool             `json:"compression,omitempty"`
	ConnectTimeout    int               `json:"connect_timeout,omitempty"`
	KeepaliveInterval int               `json:"keepalive_interval,omitempty"`
	KeepaliveCount    int               `json:"keepalive_count,omitempty"`
	ControlPersist    string            `json:"control_persist,omitempty"`
	SyncRoot          string            `json:"sync_root,omitempty"`
	SyncExclude       []string          `json:"sync_exclude,omitempty"`
	SshOptions        []string          `json:"ssh_options,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	// Jump (ProxyJump) -- one or more bastion hops dialed in order before
	// the final target. Each entry: "[user@]host[:port]". Auth uses the
	// same agent + identity_file + default key chain as the profile.
	Jump []string `json:"jump,omitempty"`
	// CompressSync controls whether `srv sync` gzips the tar stream over
	// the wire. nil = default true. ~70% size reduction for code, single-
	// digit ms CPU on the hot path.
	CompressSync *bool `json:"compress_sync,omitempty"`
	// DialAttempts: how many times to attempt the initial TCP dial / SSH
	// handshake before giving up. Default 1 (no retry, current behavior).
	// Set to 3-5 on flaky networks where the first SYN sometimes drops.
	// Auth and host-key errors never retry -- another attempt won't change
	// the answer.
	DialAttempts int `json:"dial_attempts,omitempty"`
	// DialBackoff: initial wait between dial retries; doubles each attempt
	// up to a 30s cap. Default "500ms". Parsed via time.ParseDuration so
	// "1s" / "200ms" / "2s500ms" all work.
	DialBackoff string `json:"dial_backoff,omitempty"`
	// Free-form bag for unknown keys forwarded from older Python configs.
	Extra map[string]any `json:"-"`
	// Name is the profile's lookup key in Config.Profiles. Populated by
	// ResolveProfile so deeper layers can include it in diagnostics
	// without threading the name through every signature. NOT serialized.
	Name string `json:"-"`
}

func (p *Profile) GetPort() int {
	if p.Port == 0 {
		return 22
	}
	return p.Port
}

func (p *Profile) GetConnectTimeout() int {
	if p.ConnectTimeout == 0 {
		return 10
	}
	return p.ConnectTimeout
}

func (p *Profile) GetKeepaliveInterval() int {
	if p.KeepaliveInterval == 0 {
		return 30
	}
	return p.KeepaliveInterval
}

func (p *Profile) GetKeepaliveCount() int {
	if p.KeepaliveCount == 0 {
		return 3
	}
	return p.KeepaliveCount
}

func (p *Profile) GetCompression() bool {
	if p.Compression == nil {
		return true
	}
	return *p.Compression
}

func (p *Profile) GetCompressSync() bool {
	if p.CompressSync == nil {
		return true
	}
	return *p.CompressSync
}

func (p *Profile) GetDefaultCwd() string {
	if p.DefaultCwd == "" {
		return "~"
	}
	return p.DefaultCwd
}

func (p *Profile) GetDialAttempts() int {
	if p.DialAttempts < 1 {
		return 1
	}
	return p.DialAttempts
}

func (p *Profile) GetDialBackoff() time.Duration {
	if p.DialBackoff == "" {
		return 500 * time.Millisecond
	}
	d, err := time.ParseDuration(p.DialBackoff)
	if err != nil || d < 0 {
		return 500 * time.Millisecond
	}
	return d
}

// SchemaVersion moved to srv/internal/srvio.SchemaVersion. Existing
// references in this file (and config.json `_version` handling) read
// srvio.SchemaVersion directly.

// Config maps to ~/.srv/config.json.
//
// Top-level fields beyond DefaultProfile are user-tunable globals that
// don't belong on a single profile -- they affect srv's local behavior
// regardless of which server you're talking to. Set via
// `srv config global <key> <value>`. nil pointer fields distinguish
// "user hasn't said" from "user said false".
type Config struct {
	Version        int    `json:"_version,omitempty"`
	DefaultProfile string `json:"default_profile"`
	// Hints toggles the "did you mean / typo" command hint emitter.
	// nil means default-on; *bool false means explicitly disabled.
	// Env var SRV_HINTS=0 or the --no-hints flag also disables.
	Hints *bool `json:"hints,omitempty"`
	// Lang controls UI language for help text + high-traffic error
	// strings. "" or "auto" = environment detection (SRV_LANG,
	// LC_ALL, LC_MESSAGES, LANG; falls back to English). "en" / "zh"
	// pin explicitly. Unknown values fall back to English.
	Lang     string              `json:"lang,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
	// Groups names a list of profiles that fan-out commands (`srv -G
	// <group> <cmd>`) target in parallel. Keys are group names, values
	// are ordered profile names. Empty groups are valid in the on-disk
	// config but error at use time -- a half-defined group is easier to
	// debug than a silent no-op.
	Groups map[string][]string `json:"groups,omitempty"`
	// Tunnels is the catalog of named, persistable tunnels. Bring one up
	// with `srv tunnel up <name>` (runs inside the daemon, survives the
	// CLI exit) and tear down with `srv tunnel down`. Autostart entries
	// come up automatically when the daemon starts.
	Tunnels map[string]*TunnelDef `json:"tunnels,omitempty"`
}

// TunnelDef is the saved-on-disk description of one named tunnel. The
// runtime state (active listener, started time, etc.) lives in the
// daemon and is queried via the `tunnel_list` daemon op.
type TunnelDef struct {
	// Type is "local" (default; like `ssh -L`) or "remote" (`ssh -R`).
	Type string `json:"type"`
	// Spec uses the same forms as the one-shot CLI: "8080",
	// "8080:9090", or "8080:host:9090".
	Spec string `json:"spec"`
	// Profile name to dial. Empty falls back to ResolveProfile rules at
	// up-time so $SRV_PROFILE / `.srv-project` still apply.
	Profile string `json:"profile,omitempty"`
	// Autostart=true brings the tunnel up automatically when the daemon
	// starts (typical for "always-on" things like a db port-forward).
	Autostart bool `json:"autostart,omitempty"`
}

// HintsEnabled reports whether typo / post-failure hints should fire.
// Config wins over env-default (true), and the --no-hints flag plus
// SRV_HINTS=0 are checked separately by the caller.
func (c *Config) HintsEnabled() bool {
	if c == nil || c.Hints == nil {
		return true
	}
	return *c.Hints
}

func New() *Config {
	return &Config{Profiles: map[string]*Profile{}}
}

// Load returns nil with nil error when the file doesn't exist yet.
func Load() (*Config, error) {
	data, err := os.ReadFile(srvpath.Config())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", srvpath.Config(), err)
	}
	cfg := New()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", srvpath.Config(), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	srvio.WarnIfNewerSchema(srvpath.Config(), cfg.Version)
	return cfg, nil
}

func Save(cfg *Config) error {
	cfg.Version = srvio.SchemaVersion
	return srvio.WriteJSONFile(srvpath.Config(), cfg)
}

// srvio.WriteJSONFile / srvio.WriteFileAtomic / srvio.WarnIfNewerSchema / srvio.SchemaVersion
// live in srv/internal/srvio now. Use srvio.WriteJSONFile etc.

// Resolve picks the active profile by precedence:
// override > session pin > $SRV_PROFILE > .srv-project pin > config default.
//
// The `.srv-project` step slots in just before the global default so a
// repo-level pin wins over "whatever profile happens to be default
// today" but still respects the user's session-scoped or env-scoped
// override. See findProjectFile for the lookup rules.
func Resolve(cfg *Config, override string) (string, *Profile, error) {
	name := override
	if name == "" {
		_, rec := session.Touch()
		if rec.Profile != nil {
			name = *rec.Profile
		}
	}
	if name == "" {
		name = os.Getenv("SRV_PROFILE")
	}
	if name == "" {
		if pf := project.Resolve(); pf != nil && pf.Profile != "" {
			name = pf.Profile
		}
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return "", nil, fmt.Errorf("%s", i18n.T("err.no_profile"))
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return "", nil, fmt.Errorf("%s", i18n.T("err.profile_not_found", name))
	}
	p.Name = name
	return name, p, nil
}

// GetCwd returns the persisted cwd for (current session, profile),
// falling back through:
//
//	session-persisted cwd > $SRV_CWD > .srv-project cwd > profile.default_cwd
//
// $SRV_CWD predates the project file and stays higher: it's an explicit
// per-invocation override (e.g. set in an MCP server registration's env
// block), whereas .srv-project is a passive checked-in pin. The project
// file is a one-time setup that travels with the repo; the env override
// is what you reach for when one launch needs something different.
func GetCwd(profileName string, profile *Profile) string {
	_, rec := session.Touch()
	if cwd, ok := rec.Cwds[profileName]; ok && cwd != "" {
		return cwd
	}
	if env := os.Getenv("SRV_CWD"); env != "" {
		return env
	}
	if pf := project.Resolve(); pf != nil && pf.Cwd != "" {
		return pf.Cwd
	}
	return profile.GetDefaultCwd()
}

// SetCwd persists a new cwd for (current session, profile).
func SetCwd(profileName, cwd string) error {
	sid, rec := session.Touch()
	rec.Cwds[profileName] = cwd
	return session.SaveWith(sid, rec)
}

// SetSessionProfile pins (or clears with empty string) the session profile.
func SetSessionProfile(name string) (string, error) {
	sid, rec := session.Touch()
	if name == "" {
		rec.Profile = nil
	} else {
		rec.Profile = &name
	}
	return sid, session.SaveWith(sid, rec)
}
