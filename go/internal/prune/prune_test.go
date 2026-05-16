package prune

import (
	"srv/internal/jobs"
	"strings"
	"testing"
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
