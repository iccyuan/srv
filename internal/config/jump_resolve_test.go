package config

import "testing"

// TestResolveJumps_NameRef confirms that a bare profile-name entry in
// B.Jump expands to a concrete hop carrying A's host/user/port/identity
// after Config.ResolveJumps runs. This is the headline UX: typing
// `jump: ["A"]` should be enough to reuse everything from profile A
// without restating A's connection info.
func TestResolveJumps_NameRef(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a.example.com", User: "alice", Port: 2200, IdentityFile: "~/.ssh/keyA"},
		"B": {Host: "10.10.0.5", User: "bob", Jump: []JumpHop{{Spec: "A"}}},
	}
	cfg.ResolveJumps()

	got := cfg.Profiles["B"].JumpResolved
	if len(got) != 1 {
		t.Fatalf("len(JumpResolved) = %d, want 1; got=%+v", len(got), got)
	}
	if got[0].Spec != "alice@a.example.com:2200" {
		t.Errorf("Spec = %q, want %q", got[0].Spec, "alice@a.example.com:2200")
	}
	if got[0].IdentityFile != "~/.ssh/keyA" {
		t.Errorf("IdentityFile = %q, want inherited %q", got[0].IdentityFile, "~/.ssh/keyA")
	}
}

// TestResolveJumps_DefaultPortOmitted: when A uses the default port 22,
// the resolved Spec omits ":22" so it stays clean (matches what users
// would type by hand).
func TestResolveJumps_DefaultPortOmitted(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a.example.com", User: "alice", Port: 22},
		"B": {Host: "10.10.0.5", Jump: []JumpHop{{Spec: "A"}}},
	}
	cfg.ResolveJumps()

	if got := cfg.Profiles["B"].JumpResolved[0].Spec; got != "alice@a.example.com" {
		t.Errorf("Spec = %q, want %q (no :22)", got, "alice@a.example.com")
	}
}

// TestResolveJumps_RecursiveChain: if A itself has a jump through C,
// dialing B (which jumps via A) should expand to C → A in order. This
// is what lets users compose profile hierarchies without hand-flattening.
func TestResolveJumps_RecursiveChain(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"C": {Host: "c.example.com", User: "carol"},
		"A": {Host: "a.example.com", User: "alice", Jump: []JumpHop{{Spec: "C"}}},
		"B": {Host: "10.10.0.5", User: "bob", Jump: []JumpHop{{Spec: "A"}}},
	}
	cfg.ResolveJumps()

	got := cfg.Profiles["B"].JumpResolved
	if len(got) != 2 {
		t.Fatalf("len(JumpResolved) = %d, want 2 (C then A); got=%+v", len(got), got)
	}
	if got[0].Spec != "carol@c.example.com" {
		t.Errorf("hop[0] Spec = %q, want %q", got[0].Spec, "carol@c.example.com")
	}
	if got[1].Spec != "alice@a.example.com" {
		t.Errorf("hop[1] Spec = %q, want %q", got[1].Spec, "alice@a.example.com")
	}
}

// TestResolveJumps_PerHopKeyOverridesProfile: if the jump entry sets
// its own identity_file, it wins over the referenced profile's key.
// This is the documented escape hatch (`srv config set B jump A+key`).
func TestResolveJumps_PerHopKeyOverridesProfile(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a.example.com", User: "alice", IdentityFile: "~/.ssh/keyA"},
		"B": {Host: "10.10.0.5", Jump: []JumpHop{{Spec: "A", IdentityFile: "~/.ssh/override"}}},
	}
	cfg.ResolveJumps()

	if got := cfg.Profiles["B"].JumpResolved[0].IdentityFile; got != "~/.ssh/override" {
		t.Errorf("IdentityFile = %q, want override %q", got, "~/.ssh/override")
	}
}

// TestResolveJumps_LiteralSpecPassthrough: an SSH-host literal (contains
// @ or :) is left alone, even if the substring before "@" happens to
// equal a profile name. Profile-name expansion is restricted to bare
// alphanumerics on purpose so it can't shadow legitimate user@host
// strings.
func TestResolveJumps_LiteralSpecPassthrough(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a.example.com", User: "alice"},
		"B": {Host: "10.10.0.5", Jump: []JumpHop{{Spec: "root@10.0.0.9:2222"}}},
	}
	cfg.ResolveJumps()

	got := cfg.Profiles["B"].JumpResolved
	if len(got) != 1 || got[0].Spec != "root@10.0.0.9:2222" {
		t.Errorf("expected literal passthrough, got %+v", got)
	}
}

// TestResolveJumps_UnknownNameStaysLiteral: a name that doesn't match
// any profile is preserved verbatim so dial reports a clean "lookup
// failed" rather than silently dropping the hop. (Dial then treats it
// as a bare hostname.)
func TestResolveJumps_UnknownNameStaysLiteral(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"B": {Host: "10.10.0.5", Jump: []JumpHop{{Spec: "ghost"}}},
	}
	cfg.ResolveJumps()

	got := cfg.Profiles["B"].JumpResolved
	if len(got) != 1 || got[0].Spec != "ghost" {
		t.Errorf("expected unknown name preserved, got %+v", got)
	}
}

// TestResolveJumps_CycleBroken: A → B → A would otherwise recurse
// forever. Resolution short-circuits the second time it visits a name
// and leaves the literal in place, so dial surfaces the loop as a
// hostname-resolution failure rather than hanging the process.
func TestResolveJumps_CycleBroken(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a.example.com", User: "alice", Jump: []JumpHop{{Spec: "B"}}},
		"B": {Host: "10.10.0.5", User: "bob", Jump: []JumpHop{{Spec: "A"}}},
	}
	// Must not hang or stack-overflow.
	cfg.ResolveJumps()

	// Both profiles should have a JumpResolved that terminates; the
	// exact shape is less important than the absence of explosion.
	if cfg.Profiles["A"].JumpResolved == nil || cfg.Profiles["B"].JumpResolved == nil {
		t.Error("ResolveJumps left a profile with nil JumpResolved under a cycle")
	}
}
