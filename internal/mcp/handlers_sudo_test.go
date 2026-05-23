package mcp

import (
	"strings"
	"testing"

	"srv/internal/config"
)

// sudoTestCfg returns a minimal Config with one profile so
// resolveProfile doesn't bail out. resolveProfile reads from cfg
// directly, and dial-time errors only surface AFTER the elicitation
// + cache-miss branches the sudo handler tests focus on -- so we
// never need a live SSH endpoint for the boundary tests.
func sudoTestCfg() *config.Config {
	return &config.Config{
		DefaultProfile: "p",
		Profiles: map[string]*config.Profile{
			"p": {Name: "p", Host: "127.0.0.1", User: "nobody", Port: 22},
		},
	}
}

// TestHandleSudo_RejectsEmptyCommand: the required-arg gate is the
// first thing the handler should check, before any side effect
// (profile resolution, elicitation, daemon round-trip). Catches a
// regression where reordering would let an empty-string sudo slip
// through.
func TestHandleSudo_RejectsEmptyCommand(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\t\n"} {
		r := handleSudo(map[string]any{"command": cmd}, sudoTestCfg(), "")
		if !r.IsError {
			t.Errorf("empty command %q should error, got success", cmd)
		}
		if !strings.Contains(resultText(r), "command is required") {
			t.Errorf("expected 'command is required', got %q", resultText(r))
		}
	}
}

// TestHandleSudo_ElicitationUnavailableHardDenies: a client that
// didn't advertise the elicitation capability MUST hard-deny. This
// is the core boundary -- no automatic fallback that would let MCP
// sudo through without a human signing off.
func TestHandleSudo_ElicitationUnavailableHardDenies(t *testing.T) {
	// Default: clientElicitation = false, stdinReader = nil ->
	// elicitConfirm returns (false, false). Don't install
	// elicitFnForTests; this is the real "no peer to ask" path.
	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfg(), "")
	if !r.IsError {
		t.Fatal("no-elicitation client should hard-deny, got success")
	}
	if !strings.Contains(resultText(r), "does not support elicitation") {
		t.Errorf("expected 'does not support elicitation' diagnostic, got %q", resultText(r))
	}
}

// TestHandleSudo_HumanDeclines: when the human says No at the
// elicitation prompt, the handler returns guardDenied and NEVER
// touches the password cache / dials the remote. denied_by=user
// distinguishes from guardBlocked (couldn't ask).
func TestHandleSudo_HumanDeclines(t *testing.T) {
	elicitFnForTests = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { elicitFnForTests = nil })

	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfg(), "")
	if !r.IsError {
		t.Fatal("declined elicitation should be an error result")
	}
	if !strings.Contains(resultText(r), "declined") {
		t.Errorf("expected 'declined' in body, got %q", resultText(r))
	}
}

// TestHandleSudo_AcceptedButCacheEmpty: when the human accepts but
// no password is cached (no daemon / cache miss), we return a
// structured cached:false response with a CLI hint -- NEVER prompt
// for a password or fall through to running sudo without one. The
// test runs without a daemon socket, so sudo.CacheGet returns "".
func TestHandleSudo_AcceptedButCacheEmpty(t *testing.T) {
	// Point SRV_HOME at an empty temp dir so any daemon socket the
	// real ~/.srv would have is invisible. CacheGet then takes the
	// "DialSock returns nil" fast-fail path.
	t.Setenv("SRV_HOME", t.TempDir())
	elicitFnForTests = func(string) (bool, bool) { return true, true }
	t.Cleanup(func() { elicitFnForTests = nil })

	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfg(), "")
	if !r.IsError {
		t.Fatal("cache-empty should be an error result so model retries seeding flow")
	}
	body := resultText(r)
	if !strings.Contains(body, "no cached password") {
		t.Errorf("body missing 'no cached password': %q", body)
	}
	if !strings.Contains(body, "srv sudo") {
		t.Errorf("body should hint at seeding via `srv sudo`, got %q", body)
	}
	// StructuredContent carries the machine-readable signal so a
	// custom UI can render a guided "seed the cache" step.
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent should be map[string]any, got %T", r.StructuredContent)
	}
	if cached, _ := sc["cached"].(bool); cached {
		t.Errorf("StructuredContent.cached should be false, got %v", sc["cached"])
	}
	if hint, _ := sc["hint"].(string); !strings.Contains(hint, "srv sudo") {
		t.Errorf("StructuredContent.hint missing srv-sudo seed command: %q", hint)
	}
}

// TestHandleSudo_NoConfirmArgBypassesGate: the handler must NOT
// honor a `confirm: true` arg as a way around elicitation. This
// guards against a tempting "let me skip the human" refactor; the
// privilege boundary is non-negotiable. Asserting it via a no-
// elicitation client + confirm: true still hard-denies.
func TestHandleSudo_NoConfirmArgBypassesGate(t *testing.T) {
	r := handleSudo(map[string]any{"command": "whoami", "confirm": true}, sudoTestCfg(), "")
	if !r.IsError {
		t.Fatal("confirm: true must NOT bypass elicitation for sudo")
	}
	if !strings.Contains(resultText(r), "does not support elicitation") {
		t.Errorf("confirm-bypass attempt should still hit the elicit-unavailable path, got %q", resultText(r))
	}
}

// sudoTestCfgOptIn returns the same minimal profile as sudoTestCfg
// but with EnableMCPPasswordPrompt = true. Used by the opt-in path
// tests to assert that the password-elicit branch is taken only when
// the profile explicitly opts in.
func sudoTestCfgOptIn() *config.Config {
	return &config.Config{
		DefaultProfile: "p",
		Profiles: map[string]*config.Profile{
			"p": {Name: "p", Host: "127.0.0.1", User: "nobody", Port: 22, EnableMCPPasswordPrompt: true},
		},
	}
}

// TestHandleSudo_OptInOff_CacheMissStillReturnsHint: when the toggle
// is off (the default and what sudoTestCfg uses), a cache miss must
// behave exactly like the pre-opt-in implementation -- structured
// cached:false plus a hint to seed from a terminal. The hint text
// also gains a sentence advertising the opt-in path, so we assert
// both bits are present.
func TestHandleSudo_OptInOff_CacheMissStillReturnsHint(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())
	elicitFnForTests = func(string) (bool, bool) { return true, true }
	// Sanity check: even if a password-elicit seam were installed,
	// the opt-in-off path must not consult it. Set the seam to a
	// fataling fn so any unexpected call surfaces immediately.
	elicitPasswordFnForTests = func(string) (string, bool) {
		t.Fatal("opt-in OFF must not invoke elicitPassword")
		return "", false
	}
	t.Cleanup(func() {
		elicitFnForTests = nil
		elicitPasswordFnForTests = nil
	})

	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfg(), "")
	if !r.IsError {
		t.Fatal("opt-in OFF + cache miss must error")
	}
	body := resultText(r)
	if !strings.Contains(body, "no cached password") {
		t.Errorf("body missing 'no cached password': %q", body)
	}
	if !strings.Contains(body, "enable_mcp_password_prompt") {
		t.Errorf("body should advertise the opt-in alternative, got %q", body)
	}
}

// TestHandleSudo_OptInOn_DeclineReturnsHint: the human Allow-ed the
// command, opt-in is on, but they cancel / decline the subsequent
// password prompt (or the client returns empty). The handler must
// fall back to the same cached:false hint -- no half-state where we
// dial without a password. The hint here should NOT include the
// "set enable_mcp_password_prompt: true" line (already true).
func TestHandleSudo_OptInOn_DeclineReturnsHint(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())
	elicitFnForTests = func(string) (bool, bool) { return true, true }
	elicitPasswordFnForTests = func(string) (string, bool) { return "", false }
	t.Cleanup(func() {
		elicitFnForTests = nil
		elicitPasswordFnForTests = nil
	})

	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfgOptIn(), "")
	if !r.IsError {
		t.Fatal("opt-in ON + password decline must error")
	}
	body := resultText(r)
	if !strings.Contains(body, "no cached password") {
		t.Errorf("body missing 'no cached password': %q", body)
	}
	if strings.Contains(body, "enable_mcp_password_prompt") {
		t.Errorf("opt-in already on; should not advertise the toggle, got %q", body)
	}
}

// TestHandleSudo_OptInOn_AcceptProceedsPastCacheCheck: the human
// supplies a password via the elicit-password seam. The handler must
// move past the cache-miss branch and into the dial+run code. We
// can't reach a real SSH endpoint from a test, so the success
// signal is "no longer the cached:false hint" -- the error must be
// a downstream dial failure, not the cache-miss diagnostic.
func TestHandleSudo_OptInOn_AcceptProceedsPastCacheCheck(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())
	elicitFnForTests = func(string) (bool, bool) { return true, true }
	elicitPasswordFnForTests = func(string) (string, bool) { return "secret", true }
	t.Cleanup(func() {
		elicitFnForTests = nil
		elicitPasswordFnForTests = nil
	})

	r := handleSudo(map[string]any{"command": "whoami"}, sudoTestCfgOptIn(), "")
	if !r.IsError {
		// We don't actually expect success -- the test profile points
		// at 127.0.0.1:22 with bogus creds, so the dial will fail. A
		// non-error here would mean we somehow short-circuited; treat
		// as a regression worth flagging.
		t.Fatal("expected dial-stage error against unreachable test endpoint")
	}
	body := resultText(r)
	if strings.Contains(body, "no cached password") {
		t.Errorf("opt-in accept path took the cache-miss branch despite a seeded password: %q", body)
	}
}
