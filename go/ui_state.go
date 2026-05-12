package main

import (
	"encoding/json"
	"os"
	"sort"

	"srv/internal/srvpath"
)

// Tiny on-disk state for `srv ui` so the dashboard's notion of "the
// profile I'm currently looking at" survives across runs and stays
// independent of the shell's session-bound active profile. The shell's
// active profile (sessions.json) drives the CLI; the dashboard's
// view-only selection lives here.

type uiPersistedState struct {
	LastProfile string `json:"last_profile,omitempty"`
}

func uiStateFile() string { return srvpath.UIState() }

func loadUIPersistedState() *uiPersistedState {
	buf, err := os.ReadFile(uiStateFile())
	if err != nil {
		return &uiPersistedState{}
	}
	var s uiPersistedState
	if err := json.Unmarshal(buf, &s); err != nil {
		return &uiPersistedState{}
	}
	return &s
}

func saveUIPersistedState(s *uiPersistedState) error {
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(uiStateFile(), buf, 0o644)
}

// pickInitialUIProfile returns the profile name to seed the
// dashboard's selectedProfile field. Order:
//
//  1. Last selection from ui-state.json (if still present in cfg)
//  2. cfg.DefaultProfile (if present)
//  3. Alphabetically first profile in cfg
//  4. "" if no profiles exist at all
//
// We deliberately do NOT consult sessions.json here -- the dashboard
// is not bound to the calling shell. That's the entire point of this
// file.
func pickInitialUIProfile(cfg *Config) string {
	if cfg == nil || len(cfg.Profiles) == 0 {
		return ""
	}
	ps := loadUIPersistedState()
	if ps.LastProfile != "" {
		if _, ok := cfg.Profiles[ps.LastProfile]; ok {
			return ps.LastProfile
		}
	}
	if cfg.DefaultProfile != "" {
		if _, ok := cfg.Profiles[cfg.DefaultProfile]; ok {
			return cfg.DefaultProfile
		}
	}
	names := sortedProfileNames(cfg)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// cycleProfile returns the next profile name in sorted order after
// `current`, wrapping at the end. `dir` is +1 or -1.
func cycleProfile(cfg *Config, current string, dir int) string {
	names := sortedProfileNames(cfg)
	if len(names) == 0 {
		return ""
	}
	idx := 0
	for i, n := range names {
		if n == current {
			idx = i
			break
		}
	}
	idx = (idx + dir + len(names)) % len(names)
	return names[idx]
}

// sortedProfileNames returns the cfg.Profiles keys alphabetically.
// Used as the cycle order for profile switching and any other place
// that needs a deterministic enumeration.
func sortedProfileNames(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
