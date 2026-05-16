package sshx

import (
	"strings"
	"testing"
)

// TestDetachSpawnCmd_PortableBase64Decode guards the macOS/BSD
// compatibility fix: the detached-job wrapper must decode its base64
// payload with a GNU->BSD fallback so `detach` / `run
// background=true` work whether the remote spells decode `-d`
// (coreutils/busybox) or `-D` (macOS/*BSD). Regression for the bug
// where a macOS remote silently ran an empty `bash -c` and never
// wrote the .exit marker.
func TestDetachSpawnCmd_PortableBase64Decode(t *testing.T) {
	cmd := detachSpawnCmd("QkFTRTY0", "~", "~/.srv-jobs/job1.log")
	if !strings.Contains(cmd, "base64 -d 2>/dev/null || base64 -D") {
		t.Errorf("decode is not a stderr-suppressed -d||-D fallback: %s", cmd)
	}
	// Spawn shape that echoes the pid. setsid (Linux-only util-linux)
	// gives the job its own process group so kill_job can signal the
	// whole tree; it MUST be gated on `command -v setsid` with a plain
	// `nohup` fallback so detach still works on macOS/*BSD (same
	// portability class as the base64 -d/-D split).
	for _, want := range []string{
		"mkdir -p ~/.srv-jobs",
		"command -v setsid",    // portability gate
		"setsid nohup bash -c", // Linux path: own process group
		"else nohup bash -c",   // macOS/BSD fallback: still detaches
		"QkFTRTY0",
		"echo $!",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("spawn line lost %q: %s", want, cmd)
		}
	}
}

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
