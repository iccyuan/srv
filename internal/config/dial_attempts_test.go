package config

import "testing"

// TestGetDialAttempts_DefaultsByJump locks the auto-bump rule: a
// profile WITHOUT a jump chain keeps the historical default of 1
// (no retry), but a profile WITH a jump chain auto-defaults to 2.
// Rationale lives in the GetDialAttempts doc comment -- each extra
// hop is one more chance for sshd MaxStartups / NAT blip / mid-path
// timeout to trip a single dial. One retry costs ~500ms-2s for the
// happy path's tail-of-tail probability case.
func TestGetDialAttempts_DefaultsByJump(t *testing.T) {
	cases := []struct {
		name string
		p    Profile
		want int
	}{
		{"direct profile, unset -> 1", Profile{}, 1},
		{"jumped profile, unset -> 2", Profile{Jump: []JumpHop{{Spec: "bastion"}}}, 2},
		{"explicit 3 wins over jump auto-bump", Profile{DialAttempts: 3, Jump: []JumpHop{{Spec: "bastion"}}}, 3},
		{"explicit 1 wins over jump auto-bump (user said so)", Profile{DialAttempts: 1, Jump: []JumpHop{{Spec: "bastion"}}}, 1},
		{"explicit 5 on direct profile", Profile{DialAttempts: 5}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.GetDialAttempts(); got != tc.want {
				t.Errorf("GetDialAttempts() = %d, want %d (profile=%+v)", got, tc.want, tc.p)
			}
		})
	}
}
