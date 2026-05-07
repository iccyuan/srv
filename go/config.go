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
	// Free-form bag for unknown keys forwarded from older Python configs.
	Extra map[string]any `json:"-"`
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

func (p *Profile) GetDefaultCwd() string {
	if p.DefaultCwd == "" {
		return "~"
	}
	return p.DefaultCwd
}

// Config maps to ~/.srv/config.json.
type Config struct {
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
	return cfg, nil
}

func SaveConfig(cfg *Config) error {
	return writeJSONFile(ConfigFile(), cfg)
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
