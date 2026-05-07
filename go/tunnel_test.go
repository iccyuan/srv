package main

import "testing"

func TestParseTunnelSpec(t *testing.T) {
	cases := []struct {
		in       string
		lp       int
		rh       string
		rp       int
		wantErr  bool
		errLabel string
	}{
		{in: "8080", lp: 8080, rh: "127.0.0.1", rp: 8080},
		{in: "8080:9090", lp: 8080, rh: "127.0.0.1", rp: 9090},
		{in: "8080:db:5432", lp: 8080, rh: "db", rp: 5432},
		{in: "8080:127.0.0.1:5432", lp: 8080, rh: "127.0.0.1", rp: 5432},

		{in: "", wantErr: true, errLabel: "empty"},
		{in: "abc", wantErr: true, errLabel: "non-numeric port"},
		{in: "0", wantErr: true, errLabel: "zero port"},
		{in: "70000", wantErr: true, errLabel: "out of range"},
		{in: "8080:abc", wantErr: true, errLabel: "non-numeric remote port"},
		{in: "8080::5432", wantErr: true, errLabel: "empty host"},
		{in: "a:b:c:d", wantErr: true, errLabel: "too many parts"},
	}
	for _, tc := range cases {
		lp, rh, rp, err := parseTunnelSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want error (%s), got nil", tc.in, tc.errLabel)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.in, err)
			continue
		}
		if lp != tc.lp || rh != tc.rh || rp != tc.rp {
			t.Errorf("%q: got (%d,%q,%d), want (%d,%q,%d)",
				tc.in, lp, rh, rp, tc.lp, tc.rh, tc.rp)
		}
	}
}
