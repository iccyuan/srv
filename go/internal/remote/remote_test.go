package remote

import "testing"

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
