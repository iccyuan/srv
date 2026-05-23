package config

import "testing"

// boolP is local to keep this file standalone (the existing
// guard_active_test.go has a similarly named helper but in a
// different test focus area).
func boolP(b bool) *bool { return &b }

// TestResolveInherits_ChildOverridesParentScalar: when both parent
// and child set a scalar field, child wins. This is the most common
// shape (parent declares defaults, child tweaks).
func TestResolveInherits_ChildOverridesParentScalar(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"base":  {Host: "base.example.com", User: "alice", Port: 22, PoolSize: 4},
		"child": {Host: "child.example.com", User: "bob", Inherits: "base"},
	}
	cfg.ResolveInherits()

	got := cfg.Profiles["child"]
	if got.Host != "child.example.com" {
		t.Errorf("child host should win: %q", got.Host)
	}
	if got.User != "bob" {
		t.Errorf("child user should win: %q", got.User)
	}
	if got.Port != 22 {
		t.Errorf("port should inherit from base (child unset): got %d", got.Port)
	}
	if got.PoolSize != 4 {
		t.Errorf("pool_size should inherit: got %d", got.PoolSize)
	}
}

// TestResolveInherits_BoolTriState: *bool fields use nil = inherit,
// *true / *false = explicit. Critical for fields like Multiplex
// where explicit-false must be distinguishable from "use default".
func TestResolveInherits_BoolTriState(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"base":     {Host: "base", Multiplex: boolP(true), Compression: boolP(true)},
		"explicit": {Host: "explicit", Multiplex: boolP(false), Inherits: "base"},
		"silent":   {Host: "silent", Inherits: "base"},
	}
	cfg.ResolveInherits()

	if got := cfg.Profiles["explicit"].Multiplex; got == nil || *got != false {
		t.Errorf("explicit child should keep *false, got %v", got)
	}
	if got := cfg.Profiles["silent"].Multiplex; got == nil || *got != true {
		t.Errorf("silent child should inherit *true, got %v", got)
	}
	if got := cfg.Profiles["explicit"].Compression; got == nil || *got != true {
		t.Errorf("inherited compression should be *true, got %v", got)
	}
}

// TestResolveInherits_SliceReplacesNotAppends: a child setting its
// own jump / ssh_options replaces the parent's entirely. The
// alternative (append) would make it impossible to override a
// parent's chain and is the surprising semantics path.
func TestResolveInherits_SliceReplacesNotAppends(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"base":  {Host: "base", SshOptions: []string{"StrictHostKeyChecking=no"}, Jump: []JumpHop{{Spec: "bastion"}}},
		"child": {Host: "child", SshOptions: []string{"PreferredAuthentications=publickey"}, Inherits: "base"},
		"empty": {Host: "empty", Inherits: "base"},
	}
	cfg.ResolveInherits()

	if got := cfg.Profiles["child"].SshOptions; len(got) != 1 || got[0] != "PreferredAuthentications=publickey" {
		t.Errorf("child SshOptions should replace, got %v", got)
	}
	if got := cfg.Profiles["child"].Jump; len(got) != 1 || got[0].Spec != "bastion" {
		t.Errorf("child without own Jump should inherit base's, got %v", got)
	}
	if got := cfg.Profiles["empty"].SshOptions; len(got) != 1 || got[0] != "StrictHostKeyChecking=no" {
		t.Errorf("empty child should inherit base's SshOptions, got %v", got)
	}
}

// TestResolveInherits_EnvMergesAdditively: the Env field is the one
// exception to "replace" semantics -- a child's env vars merge with
// parent's, child wins on key collisions. Locks in the documented
// "parent declares base env, child adds/overrides" pattern.
func TestResolveInherits_EnvMergesAdditively(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"base":  {Host: "base", Env: map[string]string{"LANG": "en_US.UTF-8", "TZ": "UTC"}},
		"child": {Host: "child", Env: map[string]string{"TZ": "Asia/Tokyo", "DEPLOY_ENV": "prod"}, Inherits: "base"},
	}
	cfg.ResolveInherits()

	got := cfg.Profiles["child"].Env
	wantTZ := "Asia/Tokyo"
	wantLANG := "en_US.UTF-8"
	wantDEPLOY := "prod"
	if got["TZ"] != wantTZ {
		t.Errorf("TZ should be child's %q, got %q", wantTZ, got["TZ"])
	}
	if got["LANG"] != wantLANG {
		t.Errorf("LANG should inherit %q, got %q", wantLANG, got["LANG"])
	}
	if got["DEPLOY_ENV"] != wantDEPLOY {
		t.Errorf("DEPLOY_ENV should stay %q, got %q", wantDEPLOY, got["DEPLOY_ENV"])
	}
}

// TestResolveInherits_TransitiveChain: A inherits B inherits C. The
// final shape of A should pick up C's fields through the chain. This
// is the natural extension of single-level inheritance and the test
// pins it so a future refactor doesn't accidentally make inheritance
// one-level-deep.
func TestResolveInherits_TransitiveChain(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"C": {Host: "c", PoolSize: 8, IdentityFile: "~/.ssh/c_key"},
		"B": {Inherits: "C", User: "bob"},
		"A": {Inherits: "B", Host: "a"},
	}
	cfg.ResolveInherits()

	a := cfg.Profiles["A"]
	if a.Host != "a" {
		t.Errorf("A.Host child wins: %q", a.Host)
	}
	if a.User != "bob" {
		t.Errorf("A.User should inherit from B: %q", a.User)
	}
	if a.PoolSize != 8 {
		t.Errorf("A.PoolSize should inherit from C through B: %d", a.PoolSize)
	}
	if a.IdentityFile != "~/.ssh/c_key" {
		t.Errorf("A.IdentityFile should inherit from C through B: %q", a.IdentityFile)
	}
}

// TestResolveInherits_CycleHaltsCleanly: A inherits B inherits A.
// Resolution must not recurse forever. The exact final shape is
// less important than the absence of stack overflow / hang.
func TestResolveInherits_CycleHaltsCleanly(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"A": {Host: "a", Inherits: "B"},
		"B": {Host: "b", Inherits: "A"},
	}
	// Must terminate. If we hung here, the test framework would
	// time out the package run -- 30s default budget.
	cfg.ResolveInherits()

	// Both profiles should at minimum still have their explicit
	// Host fields (cycle broke before stomping them).
	if cfg.Profiles["A"].Host == "" || cfg.Profiles["B"].Host == "" {
		t.Error("cycle resolution lost explicit Host fields")
	}
}

// TestResolveInherits_MissingParentSilentlySkips: a typo in
// `inherits` shouldn't make Load() refuse to start; the missing
// reference is harmless until the affected field is actually read at
// dial time. Dial-time errors are clearer (e.g. "no identity_file")
// than a load-time "parent xyz not found" stack trace from deep in
// a config refresh.
func TestResolveInherits_MissingParentSilentlySkips(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"orphan": {Host: "orphan", Inherits: "doesnotexist"},
	}
	cfg.ResolveInherits()

	if cfg.Profiles["orphan"].Host != "orphan" {
		t.Errorf("missing-parent profile should still resolve its own fields, got %q", cfg.Profiles["orphan"].Host)
	}
}

// TestResolveInherits_BookkeepingFieldsDoNotLeak: parent's Name,
// Inherits, and JumpResolved must NOT propagate to the child --
// those are per-profile identity / cache fields, not user-facing
// config. A leak here would make every child report parent's name
// and have a phantom inheritance link to parent's grandparent.
func TestResolveInherits_BookkeepingFieldsDoNotLeak(t *testing.T) {
	cfg := New()
	cfg.Profiles = map[string]*Profile{
		"base":  {Host: "base", Inherits: "elder", JumpResolved: []JumpHop{{Spec: "x"}}},
		"elder": {Host: "elder"},
		"child": {Host: "child", Inherits: "base"},
	}
	cfg.ResolveInherits()

	c := cfg.Profiles["child"]
	if c.Name != "child" {
		t.Errorf("child.Name leaked to %q (Name is identity, not inheritable)", c.Name)
	}
	if c.Inherits != "base" {
		t.Errorf("child.Inherits should stay %q, got %q (inheritance chain not transitively rewritten)", "base", c.Inherits)
	}
	if c.JumpResolved != nil {
		t.Errorf("child.JumpResolved should NOT inherit (separately populated by ResolveJumps), got %+v", c.JumpResolved)
	}
}
