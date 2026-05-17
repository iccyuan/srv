package prune

import (
	"strings"
	"testing"

	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/srvutil"
)

// These three guard the local-ledger semantics that moved here verbatim
// from the retired `srv jobs prune` (internal/jobcli). remoteSweep is
// false throughout so cfg can be nil -- config.Resolve is only reached
// on the --remote path, which has its own host-dependent coverage.

// TestPruneKeepsLiveJobs: a no-arg prune drops every finished row and
// keeps every still-running one. Regression guard for the bug that
// motivated keeping job records after completion.
func TestPruneKeepsLiveJobs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)

	ec := 0
	jf := &jobs.File{Jobs: []*jobs.Record{
		{ID: "live1", Profile: "p", Pid: 100, Log: "/x/live1.log"},
		{ID: "done1", Profile: "p", Pid: 200, Log: "/x/done1.log", Finished: "2026-05-15T10:00:00Z", ExitCode: &ec},
		{ID: "done2", Profile: "p", Pid: 201, Log: "/x/done2.log", Finished: "2026-05-15T11:00:00Z", Killed: true},
		{ID: "live2", Profile: "p", Pid: 101, Log: "/x/live2.log"},
	}}
	if err := jobs.Save(jf); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	if err := pruneJobs(nil, nil, "", false); err != nil {
		t.Fatalf("prune: %v", err)
	}
	loaded := jobs.Load()
	if loaded == nil || len(loaded.Jobs) != 2 {
		t.Fatalf("expected 2 live jobs after prune, got %d", len(loaded.Jobs))
	}
	gotIDs := []string{}
	for _, j := range loaded.Jobs {
		gotIDs = append(gotIDs, j.ID)
	}
	for _, want := range []string{"live1", "live2"} {
		found := false
		for _, g := range gotIDs {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("live job %q missing from pruned set: %v", want, gotIDs)
		}
	}
	for _, dont := range []string{"done1", "done2"} {
		for _, g := range gotIDs {
			if g == dont {
				t.Errorf("finished job %q should have been pruned: %v", dont, gotIDs)
			}
		}
	}
}

// TestPruneRefusesToDropLiveByID: pruning a specific id that's still
// running must fail loudly rather than silently orphan the remote pid.
func TestPruneRefusesToDropLiveByID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	jf := &jobs.File{Jobs: []*jobs.Record{
		{ID: "still-going", Profile: "p", Pid: 100, Log: "/x.log"},
	}}
	if err := jobs.Save(jf); err != nil {
		t.Fatal(err)
	}
	err := pruneJobs([]string{"still-going"}, nil, "", false)
	if err == nil {
		t.Fatal("expected prune of live job to error")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error %q should mention the job is still running", err)
	}
	if loaded := jobs.Load(); len(loaded.Jobs) != 1 {
		t.Errorf("live record was wrongly removed; have %d jobs", len(loaded.Jobs))
	}
}

// TestPruneTargetsOneByID: pruning a specific finished entry leaves
// siblings untouched.
func TestPruneTargetsOneByID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	ec := 0
	jf := &jobs.File{Jobs: []*jobs.Record{
		{ID: "done-a", Finished: "2026-05-15T10:00:00Z", ExitCode: &ec},
		{ID: "done-b", Finished: "2026-05-15T11:00:00Z", ExitCode: &ec},
	}}
	if err := jobs.Save(jf); err != nil {
		t.Fatal(err)
	}
	if err := pruneJobs([]string{"done-a"}, nil, "", false); err != nil {
		t.Fatalf("prune by id: %v", err)
	}
	loaded := jobs.Load()
	if len(loaded.Jobs) != 1 || loaded.Jobs[0].ID != "done-b" {
		t.Errorf("expected only done-b to survive, got %+v", loaded.Jobs)
	}
}

// TestRemoteSweepScriptGatesOnExit is the regression guard for the bug
// where `srv prune jobs <id> --remote` deleted a still-running job's
// remote .log unconditionally. The targeted path is reachable with an
// id that is NOT in the local ledger (stale/pruned ledger, id from
// another machine, typo colliding with a live job), in which case
// pruneJobs never verified the job finished -- so the script ITSELF
// must gate the rm on the .exit marker. The invariant: every command
// that can remove a .log must first prove that job's .exit exists.
func TestRemoteSweepScriptGatesOnExit(t *testing.T) {
	got := remoteSweepScript("abc123")
	// The .log rm must not be reachable unless .exit was tested first.
	if !strings.Contains(got, "[ -e ~/.srv-jobs/abc123.exit ]") {
		t.Fatalf("targeted sweep does not gate on the .exit marker:\n%s", got)
	}
	gate := strings.Index(got, "[ -e ~/.srv-jobs/abc123.exit ]")
	rmLog := strings.Index(got, "abc123.log")
	if gate < 0 || rmLog < 0 || gate > rmLog {
		t.Fatalf("the .exit test must precede the .log rm:\n%s", got)
	}
	// A bare unconditional `rm -f ...log` (the old bug) must be gone:
	// the rm has to be chained behind the test with &&.
	if strings.Contains(got, "&& rm -f") == false {
		t.Errorf("targeted rm is not && -chained behind the .exit test:\n%s", got)
	}

	// The no-target sweep only ever touches files discovered by
	// iterating *.exit, so a log without an .exit is structurally
	// unreachable; assert it still does that (no `rm` of a bare *.log).
	all := remoteSweepScript("")
	if !strings.Contains(all, "for e in *.exit") {
		t.Errorf("no-target sweep must iterate *.exit only:\n%s", all)
	}
	if strings.Contains(all, "rm -f *.log") || strings.Contains(all, `rm -f ~/.srv-jobs/*.log`) {
		t.Errorf("no-target sweep must not blanket-rm logs:\n%s", all)
	}
}

// TestCmdDispatch covers the Cmd argument parser scenarios: a bare
// prune is usage (exit 2) not a silent no-op, an unknown target is
// exit 2, and --remote is positional-independent. The --remote success
// paths need a live host and are exercised by the manual/E2E pass, not
// here (config.Resolve would need real config); these assert the
// pre-resolve dispatch only.
func TestCmdDispatch(t *testing.T) {
	t.Run("bare prune is usage exit 2", func(t *testing.T) {
		err := Cmd(nil, nil, "")
		if err == nil || srvutil.CodeOf(err) != 2 {
			t.Fatalf("bare prune: want exit-2 usage error, got %v", err)
		}
		if !strings.Contains(err.Error(), "usage:") {
			t.Errorf("bare prune error should be usage text, got %q", err)
		}
	})
	t.Run("unknown target is exit 2", func(t *testing.T) {
		err := Cmd([]string{"bogus"}, nil, "")
		if err == nil || srvutil.CodeOf(err) != 2 {
			t.Fatalf("unknown target: want exit-2, got %v", err)
		}
		if !strings.Contains(err.Error(), "unknown prune target") {
			t.Errorf("want unknown-target message, got %q", err)
		}
	})
	t.Run("--remote before target still parses target", func(t *testing.T) {
		// Reaches sweepRemoteJobs -> config.Resolve, which with an
		// empty (profile-less) config returns a clean error. The
		// point: --remote leading did not get consumed as the target;
		// "jobs" was, so we went down the remote path. Hermetic:
		// tempdir SRV_HOME + cleared SRV_PROFILE so a developer's real
		// profile can't satisfy Resolve and skip the error.
		dir := t.TempDir()
		t.Setenv("SRV_HOME", dir)
		t.Setenv("SRV_PROFILE", "")
		err := Cmd([]string{"--remote", "jobs"}, &config.Config{}, "")
		if err == nil || !strings.Contains(err.Error(), "--remote") {
			t.Fatalf("expected --remote resolve error proving target parsed, got %v", err)
		}
	})
}
