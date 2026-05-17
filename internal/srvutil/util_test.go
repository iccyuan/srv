package srvutil

import "testing"

func TestAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":    false,
		"123": true,
		"12a": false,
		"-1":  false,
	}
	for in, want := range cases {
		if got := AllDigits(in); got != want {
			t.Errorf("AllDigits(%q) = %v; want %v", in, got, want)
		}
	}
}
