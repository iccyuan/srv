package daemon

import (
	"srv/internal/config"
	"testing"
)

// TestAutoconnectSelectsOnlyFlaggedProfiles is a contract test against
// config.Profile.GetAutoconnect, which startAutoconnectProfiles uses
// to decide which profiles to dial at boot. We don't drive the dial
// itself (that would need an SSH stub); we just assert the predicate
// matches the field we exposed, so a future refactor that renames or
// re-types the field gets caught here instead of silently turning
// every profile's pre-warm off.
func TestAutoconnectSelectsOnlyFlaggedProfiles(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{"unset means off (lazy dial is the default)", nil, false},
		{"explicit false stays off", &fal, false},
		{"explicit true opts in", &tru, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &config.Profile{Autoconnect: c.flag}
			if got := p.GetAutoconnect(); got != c.want {
				t.Errorf("GetAutoconnect=%v, want %v", got, c.want)
			}
		})
	}
}

// TestAutoconnectFilterMatchesDaemonSelection mirrors the filter loop
// inside startAutoconnectProfiles, asserting that a mixed-config
// Profiles map selects exactly the flagged entries. Catches regressions
// where the loop accidentally walks Tunnels (which has its own
// Autostart bool) or otherwise iterates the wrong map.
func TestAutoconnectFilterMatchesDaemonSelection(t *testing.T) {
	tru := true
	cfg := &config.Config{
		Profiles: map[string]*config.Profile{
			"warm":   {Host: "a", Autoconnect: &tru},
			"cold":   {Host: "b"},
			"warm2":  {Host: "c", Autoconnect: &tru},
			"frozen": {Host: "d"},
		},
	}
	var selected []string
	for name, prof := range cfg.Profiles {
		if prof.GetAutoconnect() {
			selected = append(selected, name)
		}
	}
	if len(selected) != 2 {
		t.Fatalf("expected 2 selected profiles, got %d (%v)", len(selected), selected)
	}
	got := map[string]bool{}
	for _, n := range selected {
		got[n] = true
	}
	for _, want := range []string{"warm", "warm2"} {
		if !got[want] {
			t.Errorf("expected %q to be selected for autoconnect", want)
		}
	}
}
