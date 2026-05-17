package prune

import (
	"strings"
	"testing"

	"srv/internal/jobs"
)

func ids(jf *jobs.File) []string {
	out := []string{}
	for _, j := range jf.Jobs {
		out = append(out, j.ID)
	}
	return out
}

func TestPruneLedger(t *testing.T) {
	ec := 0
	mk := func() *jobs.File {
		return &jobs.File{Jobs: []*jobs.Record{
			{ID: "live1"},
			{ID: "done1", Finished: "2026-05-15T10:00:00Z", ExitCode: &ec},
			{ID: "done2", Finished: "2026-05-15T11:00:00Z", Killed: true},
			{ID: "live2"},
		}}
	}

	// No id: drop every finished row, keep both live ones.
	jf := mk()
	n, err := PruneLedger(jf, "")
	if err != nil || n != 2 {
		t.Fatalf("prune all finished: n=%d err=%v; want 2,nil", n, err)
	}
	if got := ids(jf); len(got) != 2 || got[0] != "live1" || got[1] != "live2" {
		t.Fatalf("kept set = %v; want [live1 live2]", got)
	}

	// By id, finished: drop just that one.
	jf = mk()
	if n, err := PruneLedger(jf, "done1"); err != nil || n != 1 {
		t.Fatalf("prune done1: n=%d err=%v; want 1,nil", n, err)
	}
	if got := ids(jf); len(got) != 3 {
		t.Fatalf("after prune done1 want 3 records, got %v", got)
	}

	// By id, still running: must error AND leave the ledger untouched
	// (no half-rewrite -- the long-lived MCP process must not orphan
	// the live remote pid).
	jf = mk()
	n, err = PruneLedger(jf, "live1")
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("prune live by id: err=%v; want a 'still running' error", err)
	}
	if n != 0 || len(jf.Jobs) != 4 {
		t.Fatalf("running-by-id must not mutate the ledger; n=%d len=%d", n, len(jf.Jobs))
	}

	// Nothing finished: clean no-op.
	jf = &jobs.File{Jobs: []*jobs.Record{{ID: "a"}, {ID: "b"}}}
	if n, err := PruneLedger(jf, ""); err != nil || n != 0 || len(jf.Jobs) != 2 {
		t.Fatalf("no-finished prune: n=%d err=%v len=%d; want 0,nil,2", n, err, len(jf.Jobs))
	}
}
