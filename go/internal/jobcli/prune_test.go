package jobcli

import (
	"srv/internal/jobs"
	"strings"
	"testing"
)

// TestPruneKeepsLiveJobs is a regression guard for the bug that
// motivated keeping job records after completion: pruning must
// distinguish finished from still-running rows, never silently
// dropping a live job we're tracking the PID + log path for.
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

	if err := cmdJobsPrune(nil); err != nil {
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

// TestPruneRefusesToDropLiveByID is the safety check on the
// targeted-prune path: pruning a specific id that's still running
// must fail loudly rather than silently orphan the remote PID.
func TestPruneRefusesToDropLiveByID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	jf := &jobs.File{Jobs: []*jobs.Record{
		{ID: "still-going", Profile: "p", Pid: 100, Log: "/x.log"},
	}}
	if err := jobs.Save(jf); err != nil {
		t.Fatal(err)
	}
	err := cmdJobsPrune([]string{"still-going"})
	if err == nil {
		t.Fatal("expected prune of live job to error")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error %q should mention the job is still running", err)
	}
	// Record must remain.
	if loaded := jobs.Load(); len(loaded.Jobs) != 1 {
		t.Errorf("live record was wrongly removed; have %d jobs", len(loaded.Jobs))
	}
}

// TestPruneTargetsOneByID checks the happy-path of pruning a
// specific finished entry without touching siblings.
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
	if err := cmdJobsPrune([]string{"done-a"}); err != nil {
		t.Fatalf("prune by id: %v", err)
	}
	loaded := jobs.Load()
	if len(loaded.Jobs) != 1 || loaded.Jobs[0].ID != "done-b" {
		t.Errorf("expected only done-b to survive, got %+v", loaded.Jobs)
	}
}
