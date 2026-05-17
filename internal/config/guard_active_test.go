package config

import "testing"

// boolPtr is a tiny helper so table rows can express the tri-state
// GlobalOff field (nil / *true / *false) inline.
func boolPtr(b bool) *bool { return &b }

// TestGuardActive_Precedence locks the resolution order:
//
//	SRV_GUARD env > per-session > global config (GlobalOff) > default ON
//
// The session layer is forced to "unset" by pointing SRV_HOME at an
// empty temp dir (no sessions.json) and SRV_SESSION at an id that has
// no record, so the test exercises the env + global-config + default
// layers deterministically without touching the user's real ~/.srv.
func TestGuardActive_Precedence(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())
	t.Setenv("SRV_SESSION", "guardactive-unit-test")

	cases := []struct {
		name      string
		env       string // SRV_GUARD value; "" = unset
		globalOff *bool  // cfg.Guard.GlobalOff
		want      bool
	}{
		{"default: no env, no session, no global -> ON", "", nil, true},
		{"global off -> OFF", "", boolPtr(true), false},
		{"global explicitly on -> ON", "", boolPtr(false), true},
		{"env off beats default", "off", nil, false},
		{"env on beats default", "on", nil, true},
		{"env off beats global-on", "off", boolPtr(false), false},
		{"env on beats global-off", "1", boolPtr(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SRV_GUARD", tc.env)
			cfg := &Config{Guard: &GuardConfig{GlobalOff: tc.globalOff}}
			if got := cfg.GuardActive(); got != tc.want {
				t.Errorf("GuardActive() = %v, want %v", got, tc.want)
			}
		})
	}

	// Nil receiver and nil Guard must both fall through to default ON
	// (no panic on the nil-pointer method call).
	t.Setenv("SRV_GUARD", "")
	if !(*Config)(nil).GuardActive() {
		t.Error("nil *Config GuardActive() = false, want true (default ON)")
	}
	if !(&Config{}).GuardActive() {
		t.Error("Config{} (nil Guard) GuardActive() = false, want true (default ON)")
	}
}
