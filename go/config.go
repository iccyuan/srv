package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigDir is the on-disk location of all srv state.
// Honors $SRV_HOME; falls back to ~/.srv.
func ConfigDir() string {
	if v := os.Getenv("SRV_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".srv"
	}
	return filepath.Join(home, ".srv")
}

func ConfigFile() string   { return filepath.Join(ConfigDir(), "config.json") }
func SessionsFile() string { return filepath.Join(ConfigDir(), "sessions.json") }
func JobsFile() string     { return filepath.Join(ConfigDir(), "jobs.json") }

// Profile mirrors the Python profile dict. Keys with omitempty/zero-value
// semantics:
//   - Multiplex/Compression default to true if not present (handled by
//     accessor methods)
//   - Most ints/strings have sensible zero defaults applied at use time
type Profile struct {
	Host              string   `json:"host"`
	User              string   `json:"user,omitempty"`
	Port              int      `json:"port,omitempty"`
	IdentityFile      string   `json:"identity_file,omitempty"`
	DefaultCwd        string   `json:"default_cwd,omitempty"`
	Multiplex         *bool    `json:"multiplex,omitempty"`
	Compression       *bool    `json:"compression,omitempty"`
	ConnectTimeout    int      `json:"connect_timeout,omitempty"`
	KeepaliveInterval int      `json:"keepalive_interval,omitempty"`
	KeepaliveCount    int      `json:"keepalive_count,omitempty"`
	ControlPersist    string   `json:"control_persist,omitempty"`
	SyncRoot          string   `json:"sync_root,omitempty"`
	SyncExclude       []string `json:"sync_exclude,omitempty"`
	SshOptions        []string `json:"ssh_options,omitempty"`
	// Jump (ProxyJump) -- one or more bastion hops dialed in order before
	// the final target. Each entry: "[user@]host[:port]". Auth uses the
	// same agent + identity_file + default key chain as the profile.
	Jump              []string `json:"jump,omitempty"`
	// CompressSync controls whether `srv sync` gzips the tar stream over
	// the wire. nil = default true. ~70% size reduction for code, single-
	// digit ms CPU on the hot path.
	CompressSync *bool `json:"compress_sync,omitempty"`
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

// SchemaVersion identifies the current on-disk JSON shape. Bumped when a
// breaking field rename / semantic change requires migration. Older srv
// reading a newer file logs a warning and proceeds best-effort; newer srv
// reading an older file (or one without _version) treats it as version 0
// and silently upgrades on next save.
const SchemaVersion = 1

// Config maps to ~/.srv/config.json.
type Config struct {
	Version        int                 `json:"_version,omitempty"`
	DefaultProfile string              `json:"default_profile"`
	Profiles       map[string]*Profile `json:"profiles"`
}

func newConfig() *Config {
	return &Config{Profiles: map[string]*Profile{}}
}

// LoadConfig returns nil with nil error when the file doesn't exist yet.
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", ConfigFile(), err)
	}
	cfg := newConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigFile(), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	warnIfNewerSchema(ConfigFile(), cfg.Version)
	return cfg, nil
}

func SaveConfig(cfg *Config) error {
	cfg.Version = SchemaVersion
	return writeJSONFile(ConfigFile(), cfg)
}

// warnIfNewerSchema emits one stderr line when an on-disk file declares a
// schema we don't know about yet. We still try to use it (forward compat),
// but the user should know they may need a srv upgrade.
func warnIfNewerSchema(path string, version int) {
	if version > SchemaVersion {
		fmt.Fprintf(os.Stderr,
			"srv: %s is schema version %d; this srv knows %d. Upgrade srv to be safe.\n",
			path, version, SchemaVersion)
	}
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(
		filepath.Dir(path),
		fmt.Sprintf(".%s.%d.%s.tmp", filepath.Base(path), os.Getpid(), randHex4()),
	)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return err2
		}
	}
	return nil
}

// ResolveProfile picks the active profile by precedence:
// override > session pin > $SRV_PROFILE > config default.
func ResolveProfile(cfg *Config, override string) (string, *Profile, error) {
	name := override
	if name == "" {
		_, rec := TouchSession()
		if rec.Profile != nil {
			name = *rec.Profile
		}
	}
	if name == "" {
		name = os.Getenv("SRV_PROFILE")
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return "", nil, fmt.Errorf("error: no profile selected. Run `srv init`, then `srv use <profile>` to pin one for this shell.")
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return "", nil, fmt.Errorf("error: profile %q not found. Run `srv config list`.", name)
	}
	p.Name = name
	return name, p, nil
}

// GetCwd returns the persisted cwd for (current session, profile), falling
// back to profile.default_cwd.
func GetCwd(profileName string, profile *Profile) string {
	_, rec := TouchSession()
	if cwd, ok := rec.Cwds[profileName]; ok && cwd != "" {
		return cwd
	}
	return profile.GetDefaultCwd()
}

// SetCwd persists a new cwd for (current session, profile).
func SetCwd(profileName, cwd string) error {
	sid, rec := TouchSession()
	rec.Cwds[profileName] = cwd
	return saveSessionsWith(sid, rec)
}

// SetSessionProfile pins (or clears with empty string) the session profile.
func SetSessionProfile(name string) (string, error) {
	sid, rec := TouchSession()
	if name == "" {
		rec.Profile = nil
	} else {
		rec.Profile = &name
	}
	return sid, saveSessionsWith(sid, rec)
}
