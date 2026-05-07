package main

import "testing"

func TestParseHostSpec(t *testing.T) {
	cases := []struct {
		in       string
		dUser    string
		dPort    int
		wantUser string
		wantHost string
		wantPort int
	}{
		{"bastion", "ubuntu", 22, "ubuntu", "bastion", 22},
		{"root@bastion", "ubuntu", 22, "root", "bastion", 22},
		{"root@bastion:2222", "ubuntu", 22, "root", "bastion", 2222},
		{"bastion:2222", "ubuntu", 22, "ubuntu", "bastion", 2222},
		{"user@host:notnum", "ubuntu", 22, "user", "host:notnum", 22}, // bad port stays literal
	}
	for _, tc := range cases {
		u, h, p := parseHostSpec(tc.in, tc.dUser, tc.dPort)
		if u != tc.wantUser || h != tc.wantHost || p != tc.wantPort {
			t.Errorf("parseHostSpec(%q, %q, %d) = (%q, %q, %d); want (%q, %q, %d)",
				tc.in, tc.dUser, tc.dPort, u, h, p, tc.wantUser, tc.wantHost, tc.wantPort)
		}
	}
}
