package jobs

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	jf := &File{Jobs: []*Record{
		{ID: "20260516-205020-26fa"},
		{ID: "20260516-205023-9b4a"},
		{ID: "20260516-205027-72c9"},
	}}

	// Exact id.
	if r, err := Resolve(jf, "20260516-205023-9b4a"); err != nil || r == nil || r.ID != "20260516-205023-9b4a" {
		t.Fatalf("exact match: r=%v err=%v", r, err)
	}
	// Unambiguous prefix (one job started at :205020).
	if r, err := Resolve(jf, "20260516-205020"); err != nil || r == nil || r.ID != "20260516-205020-26fa" {
		t.Fatalf("unique prefix: r=%v err=%v", r, err)
	}
	// Not found -> "no such job".
	if _, err := Resolve(jf, "deadbeef"); err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Fatalf("not-found: err=%v; want 'no such job'", err)
	}
	// Ambiguous prefix -> distinct 'ambiguous' error, NOT 'no such job'.
	_, err := Resolve(jf, "20260516-2050")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "3 jobs") {
		t.Fatalf("ambiguous prefix: err=%v; want an 'ambiguous ... 3 jobs' error", err)
	}
	if strings.Contains(err.Error(), "no such job") {
		t.Fatalf("ambiguous prefix must NOT report 'no such job': %v", err)
	}
}
