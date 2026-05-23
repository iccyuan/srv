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
