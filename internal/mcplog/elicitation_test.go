package mcplog

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SRV_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "mcp.log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// No mcp.log -> known=false (caller must render "unknown", never "off").
func TestLastElicitation_NoLog(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir()) // dir exists, file does not
	if known, _, _, _ := LastElicitation(); known {
		t.Fatalf("no log: known=true, want false")
	}
}

// A log with sessions but no initialize line -> still unknown.
func TestLastElicitation_NoHandshakeLine(t *testing.T) {
	writeLog(t, "2026-05-18T10:00:00+08:00 [1234] start v=2.7.0\n"+
		"2026-05-18T10:00:01+08:00 [1234] tool=run dur=0.5s ok\n")
	if known, _, _, _ := LastElicitation(); known {
		t.Fatalf("no initialize line: known=true, want false")
	}
}

// Last match wins across multiple sessions, and the pid is the one
// from that most-recent handshake (not an earlier session's).
func TestLastElicitation_LastWins(t *testing.T) {
	writeLog(t, ""+
		"2026-05-18T10:00:00+08:00 [1111] initialize elicitation=false\n"+
		"2026-05-18T10:00:01+08:00 [1111] tool=run dur=0.5s ok\n"+
		"2026-05-18T11:00:00+08:00 [2222] initialize elicitation=true\n")
	known, on, when, pid := LastElicitation()
	if !known || !on {
		t.Fatalf("got known=%v on=%v, want true/true", known, on)
	}
	if pid != 2222 {
		t.Errorf("pid=%d, want 2222 (most recent handshake)", pid)
	}
	if when.IsZero() {
		t.Errorf("when is zero, want the handshake timestamp")
	}
}

// elicitation=false parses as on=false (not just "absent").
func TestLastElicitation_OffParsed(t *testing.T) {
	writeLog(t, "2026-05-18T10:00:00+08:00 [3333] initialize elicitation=false\n")
	known, on, _, pid := LastElicitation()
	if !known || on || pid != 3333 {
		t.Fatalf("got known=%v on=%v pid=%d, want true/false/3333", known, on, pid)
	}
}
