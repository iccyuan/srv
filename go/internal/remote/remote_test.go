package remote

import (
	"strings"
	"testing"

	"srv/internal/config"
)

// ApplyEnv is the contract handleRun/handleDetach now depend on for
// CLI/MCP parity: profile env vars are prepended to the command,
// sorted, and it is a pure no-op when there is no env to apply.
func TestApplyEnv(t *testing.T) {
	const cmd = "echo hi"

	if got := ApplyEnv(nil, cmd); got != cmd {
		t.Errorf("ApplyEnv(nil) = %q; want unchanged %q", got, cmd)
	}
	if got := ApplyEnv(&config.Profile{}, cmd); got != cmd {
		t.Errorf("ApplyEnv(empty env) = %q; want unchanged %q", got, cmd)
	}

	p := &config.Profile{Env: map[string]string{"BVAR": "2", "AVAR": "1"}}
	got := ApplyEnv(p, cmd)
	if !strings.HasSuffix(got, " "+cmd) {
		t.Errorf("ApplyEnv result %q must end with the original command", got)
	}
	if !strings.Contains(got, "AVAR=") || !strings.Contains(got, "BVAR=") {
		t.Errorf("ApplyEnv result %q must carry both env keys", got)
	}
	if strings.Index(got, "AVAR=") > strings.Index(got, "BVAR=") {
		t.Errorf("ApplyEnv result %q must order keys deterministically (AVAR before BVAR)", got)
	}
	// Deterministic: same inputs -> byte-identical output (cache safety).
	if again := ApplyEnv(p, cmd); again != got {
		t.Errorf("ApplyEnv not deterministic: %q vs %q", got, again)
	}
}

func TestValidateRemotePath_Valid(t *testing.T) {
	valid := []string{
		"",                // empty == "use cwd"
		"/opt/app",        // absolute POSIX
		"/var/log/syslog", // ditto
		"~",               // home
		"~/logs/app.log",  // home-relative
		"foo/bar.txt",     // cwd-relative
		".srv-jobs/x.log", // dotted relative
		"a:b",             // colon mid-name, NOT a drive prefix
		"deploy:2024/out", // ditto -- must not false-positive
	}
	for _, p := range valid {
		if err := ValidateRemotePath(p); err != nil {
			t.Errorf("ValidateRemotePath(%q) = %v; want nil", p, err)
		}
	}
}

func TestValidateRemotePath_Invalid(t *testing.T) {
	invalid := []string{
		`C:\Users\admin\AppData\Local\Temp\srv_resume_test.bin`,
		`C:/Users/admin/AppData/Local/Temp/srv_progress_test_cli.bin`,
		`C:/Program Files/Git/mnt/test`,
		`c:\tmp`,         // lowercase drive
		`D:\data`,        // other drive letter
		`C:`,             // bare drive
		`Z:/share`,       // forward-slash drive
		`foo\bar`,        // stray backslash anywhere
		`~/sub\dir`,      // backslash after a valid prefix
		`\\server\share`, // UNC
	}
	for _, p := range invalid {
		if err := ValidateRemotePath(p); err == nil {
			t.Errorf("ValidateRemotePath(%q) = nil; want error", p)
		}
	}
}

// TestValidateRemotePath_LeakRegression pins the exact inputs that
// created the `/root/C:/...` garbage tree on the server. They must
// be rejected by the guard, AND ResolvePath alone must still anchor
// them under cwd -- proving the guard (not ResolvePath) is what stops
// the leak, so a future ResolvePath refactor can't silently regress it.
func TestValidateRemotePath_LeakRegression(t *testing.T) {
	leaked := []string{
		`C:\Users\admin\AppData\Local\Temp\srv_resume_test.bin`,
		`C:\Users\admin\AppData\Local\Temp\srv_progress_test_cli.bin`,
		`C:\Program Files\Git\mnt\test`,
	}
	for _, p := range leaked {
		if ValidateRemotePath(p) == nil {
			t.Fatalf("leak input %q passed validation; would still MkdirAll ~/C:/ tree", p)
		}
		if got := ResolvePath(p, "~"); got == p {
			t.Fatalf("ResolvePath(%q, \"~\") = %q unchanged; expected a cwd-anchored join "+
				"(if this changes, the leak path changed -- re-audit the guard)", p, got)
		}
	}
}
