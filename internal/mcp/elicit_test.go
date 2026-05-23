package mcp

import (
	"bufio"
	"os"
	"srv/internal/config"
	"strings"
	"testing"
)

// guardActiveForTest pins the guard ON and uses an empty config as the
// rule source (defaults, incl. "rm -rf", apply) so these tests don't
// depend on ~/.srv/config.json. Mirrors TestHandleRunGroup_GuardParity.
func guardActiveForTest(t *testing.T) {
	t.Helper()
	t.Setenv("SRV_GUARD", "1")
	SetGuardConfigForTests(&config.Config{})
	t.Cleanup(func() { SetGuardConfigForTests(nil) })
}

// TestGuardElicit_HumanAllows: when the human Accepts at the
// elicitation prompt, the risky command proceeds (guardCheckRisky
// returns nil) -- the model never had to set confirm=true.
func TestGuardElicit_HumanAllows(t *testing.T) {
	guardActiveForTest(t)
	elicitFnForTests = func(string) (bool, bool) { return true, true }
	t.Cleanup(func() { elicitFnForTests = nil })

	if r := guardCheckRisky("run", "rm -rf /tmp/x", false); r != nil {
		t.Fatalf("human Accept should proceed, got block: %q", resultText(*r))
	}
}

// TestGuardElicit_HumanDenies: a Decline/Cancel comes back as a
// guardDenied result -- guard_blocked stays true (envelope unchanged)
// and denied_by=user signals "a person said no, do not retry".
func TestGuardElicit_HumanDenies(t *testing.T) {
	guardActiveForTest(t)
	elicitFnForTests = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { elicitFnForTests = nil })

	r := guardCheckRisky("run", "rm -rf /tmp/x", false)
	if r == nil {
		t.Fatal("human Decline should block")
	}
	m, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent %T, want map", r.StructuredContent)
	}
	if m["guard_blocked"] != true || m["denied_by"] != "user" {
		t.Errorf("structured=%v, want guard_blocked=true denied_by=user", m)
	}
	if !strings.Contains(resultText(*r), "denied by the user") {
		t.Errorf("text should say denied by the user, got %q", resultText(*r))
	}
}

// TestGuardElicit_NoCapabilityFallsBackToHardDeny: with no elicitation
// seam and the client capability off (the default), the original
// hard-deny path is taken -- guardBlocked, no denied_by. This is the
// documented degrade behaviour and what every pre-existing test relies
// on.
func TestGuardElicit_NoCapabilityFallsBackToHardDeny(t *testing.T) {
	guardActiveForTest(t)
	elicitFnForTests = nil
	clientElicitation = false
	stdinReader = nil

	r := guardCheckRisky("run", "rm -rf /tmp/x", false)
	if r == nil {
		t.Fatal("no elicitation -> must hard-deny")
	}
	m, _ := r.StructuredContent.(map[string]any)
	if m["guard_blocked"] != true {
		t.Errorf("guard_blocked=%v, want true", m["guard_blocked"])
	}
	if _, present := m["denied_by"]; present {
		t.Errorf("hard-deny must not carry denied_by, got %v", m)
	}
	if !strings.Contains(resultText(*r), "confirm=true") {
		t.Errorf("hard-deny should still point at confirm=true, got %q", resultText(*r))
	}
}

// TestElicitConfirm_Transport drives the real round-trip: feed a
// canned client reply on a fake stdinReader, capture the outgoing
// elicitation/create frame on stdout, and assert action mapping.
// accept -> (true,true); decline -> (false,true).
func TestElicitConfirm_Transport(t *testing.T) {
	cases := []struct {
		action    string
		wantAllow bool
	}{
		{"accept", true},
		{"decline", false},
		{"cancel", false},
	}
	for _, tc := range cases {
		prevReader, prevCap, prevSeq := stdinReader, clientElicitation, elicitSeq
		prevStdout := os.Stdout
		t.Cleanup(func() {
			stdinReader, clientElicitation, elicitSeq = prevReader, prevCap, prevSeq
			os.Stdout = prevStdout
		})

		clientElicitation = true
		elicitSeq = 0 // -> first request id is "srv-elicit-1"
		reply := `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"` +
			tc.action + `"}}` + "\n"
		stdinReader = bufio.NewReader(strings.NewReader(reply))

		out, err := os.CreateTemp(t.TempDir(), "stdout-*")
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = out

		allow, asked := elicitConfirm("allow?")

		_ = out.Close()
		os.Stdout = prevStdout
		sent, _ := os.ReadFile(out.Name())

		if !asked {
			t.Fatalf("%s: asked=false, want true (reply was well-formed)", tc.action)
		}
		if allow != tc.wantAllow {
			t.Errorf("%s: allow=%v, want %v", tc.action, allow, tc.wantAllow)
		}
		if !strings.Contains(string(sent), `"method":"elicitation/create"`) {
			t.Errorf("%s: outgoing frame missing elicitation/create: %s", tc.action, sent)
		}
	}
}

// TestElicitPassword_Transport_Accept: drive the real round-trip
// with an accept reply that carries a password in content. Asserts
// (a) the outgoing frame declares a `format:"password"` field so a
// capable client renders a masked input, and (b) the helper returns
// the password verbatim with asked=true.
func TestElicitPassword_Transport_Accept(t *testing.T) {
	prevReader, prevCap, prevSeq := stdinReader, clientElicitation, elicitSeq
	prevStdout := os.Stdout
	t.Cleanup(func() {
		stdinReader, clientElicitation, elicitSeq = prevReader, prevCap, prevSeq
		os.Stdout = prevStdout
	})

	clientElicitation = true
	elicitSeq = 0 // -> first request id is "srv-elicit-1"
	reply := `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"accept","content":{"password":"hunter2"}}}` + "\n"
	stdinReader = bufio.NewReader(strings.NewReader(reply))

	out, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = out

	pw, asked := elicitPassword("password?")

	_ = out.Close()
	os.Stdout = prevStdout
	sent, _ := os.ReadFile(out.Name())

	if !asked {
		t.Fatalf("asked=false, want true (reply carried a password)")
	}
	if pw != "hunter2" {
		t.Errorf("password=%q, want %q", pw, "hunter2")
	}
	if !strings.Contains(string(sent), `"method":"elicitation/create"`) {
		t.Errorf("outgoing frame missing elicitation/create: %s", sent)
	}
	// We deliberately do NOT send format:"password" -- Claude Code's
	// MCP client validates against a strict subset (only email/uri/
	// date/date-time accepted) and rejects the whole request with
	// -32602 otherwise. Assert the negative: if this string ever
	// re-appears, the schema is regressing back to the broken shape
	// captured during the 2026-05-24 live verification.
	if strings.Contains(string(sent), `"format":"password"`) {
		t.Errorf("schema must NOT set format:password (Claude Code rejects with -32602): %s", sent)
	}
	if !strings.Contains(string(sent), `"required":["password"]`) {
		t.Errorf("outgoing schema must mark password required, got: %s", sent)
	}
	// description is the user-visible signal that the field is
	// sensitive (since we can't ask the client to mask). Assert it's
	// present so we don't accidentally drop the warning text.
	if !strings.Contains(string(sent), `"description"`) {
		t.Errorf("password property should carry a description warning the user the input is plain text, got: %s", sent)
	}
}

// TestElicitPassword_DeclineAndEmpty: any of (action != accept) /
// (empty password content) / (no content at all) must come back as
// ("",false). Three sub-cases share the same wiring boilerplate.
func TestElicitPassword_DeclineAndEmpty(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"decline", `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"decline"}}`},
		{"cancel", `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"cancel"}}`},
		{"accept-empty", `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"accept","content":{"password":""}}}`},
		{"accept-nocontent", `{"jsonrpc":"2.0","id":"srv-elicit-1","result":{"action":"accept"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prevReader, prevCap, prevSeq := stdinReader, clientElicitation, elicitSeq
			prevStdout := os.Stdout
			t.Cleanup(func() {
				stdinReader, clientElicitation, elicitSeq = prevReader, prevCap, prevSeq
				os.Stdout = prevStdout
			})

			clientElicitation = true
			elicitSeq = 0
			stdinReader = bufio.NewReader(strings.NewReader(tc.reply + "\n"))
			out, _ := os.CreateTemp(t.TempDir(), "stdout-*")
			os.Stdout = out

			pw, asked := elicitPassword("password?")
			os.Stdout = prevStdout
			_ = out.Close()

			if pw != "" || asked {
				t.Errorf("got (pw=%q, asked=%v), want (\"\", false)", pw, asked)
			}
		})
	}
}

// TestElicitPassword_NoCapabilityDegrades: without the elicitation
// capability, the helper must return ("",false) without writing any
// frame to stdout -- the same hard-deny shape as elicitConfirm.
func TestElicitPassword_NoCapabilityDegrades(t *testing.T) {
	prevReader, prevCap := stdinReader, clientElicitation
	prevStdout := os.Stdout
	t.Cleanup(func() {
		stdinReader, clientElicitation = prevReader, prevCap
		os.Stdout = prevStdout
	})

	clientElicitation = false
	stdinReader = nil
	out, _ := os.CreateTemp(t.TempDir(), "stdout-*")
	os.Stdout = out

	pw, asked := elicitPassword("password?")
	os.Stdout = prevStdout
	_ = out.Close()
	sent, _ := os.ReadFile(out.Name())

	if pw != "" || asked {
		t.Errorf("no-capability: got (pw=%q, asked=%v), want (\"\", false)", pw, asked)
	}
	if len(strings.TrimSpace(string(sent))) > 0 {
		t.Errorf("no-capability path must not write any frame, got: %s", sent)
	}
}

// TestElicitConfirm_PipeClosedDegrades: stdin EOF mid-elicitation must
// degrade to asked=false (hard-deny), never fabricate an answer.
func TestElicitConfirm_PipeClosedDegrades(t *testing.T) {
	prevReader, prevCap := stdinReader, clientElicitation
	prevStdout := os.Stdout
	t.Cleanup(func() {
		stdinReader, clientElicitation = prevReader, prevCap
		os.Stdout = prevStdout
	})

	clientElicitation = true
	stdinReader = bufio.NewReader(strings.NewReader("")) // immediate EOF
	out, _ := os.CreateTemp(t.TempDir(), "stdout-*")
	os.Stdout = out

	allow, asked := elicitConfirm("allow?")
	os.Stdout = prevStdout
	_ = out.Close()

	if asked || allow {
		t.Errorf("EOF mid-elicitation: got (allow=%v, asked=%v), want (false,false)", allow, asked)
	}
}
