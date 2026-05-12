package daemon

import (
	"encoding/base64"
	"testing"
	"time"
)

func newSudoTestState() *daemonState {
	return &daemonState{
		sudoCache: map[string]sudoCacheEntry{},
	}
}

func TestSudoCache_GetMiss(t *testing.T) {
	s := newSudoTestState()
	resp := s.handleSudoCacheGet(Request{Profile: "p"})
	if !resp.OK {
		t.Fatalf("get should succeed on miss; got err=%q", resp.Err)
	}
	if resp.PasswordB64 != "" {
		t.Errorf("miss should return empty PasswordB64, got %q", resp.PasswordB64)
	}
}

func TestSudoCache_SetThenGet(t *testing.T) {
	s := newSudoTestState()
	pw := "hunter2!"
	setResp := s.handleSudoCacheSet(Request{
		Profile:     "p",
		PasswordB64: base64.StdEncoding.EncodeToString([]byte(pw)),
		TTLSec:      60,
	})
	if !setResp.OK {
		t.Fatalf("set failed: %q", setResp.Err)
	}
	getResp := s.handleSudoCacheGet(Request{Profile: "p"})
	if !getResp.OK {
		t.Fatalf("get failed: %q", getResp.Err)
	}
	got, _ := base64.StdEncoding.DecodeString(getResp.PasswordB64)
	if string(got) != pw {
		t.Errorf("round-trip mismatch: %q vs %q", got, pw)
	}
}

func TestSudoCache_TTLExpires(t *testing.T) {
	s := newSudoTestState()
	// Inject directly with a past expiry to avoid waiting.
	s.sudoCache["p"] = sudoCacheEntry{
		password: []byte("stale"),
		expires:  time.Now().Add(-time.Second),
	}
	resp := s.handleSudoCacheGet(Request{Profile: "p"})
	if resp.PasswordB64 != "" {
		t.Errorf("expired entry should miss, got %q", resp.PasswordB64)
	}
	// Lazy eviction: get on expired entry should have removed it.
	if _, ok := s.sudoCache["p"]; ok {
		t.Error("expired entry should have been evicted on get")
	}
}

func TestSudoCache_Clear(t *testing.T) {
	s := newSudoTestState()
	s.sudoCache["p"] = sudoCacheEntry{
		password: []byte("x"),
		expires:  time.Now().Add(time.Hour),
	}
	resp := s.handleSudoCacheClear(Request{Profile: "p"})
	if !resp.OK {
		t.Fatalf("clear failed: %q", resp.Err)
	}
	if _, ok := s.sudoCache["p"]; ok {
		t.Error("entry not removed after clear")
	}
}

func TestSudoCache_TTLClampedToMax(t *testing.T) {
	s := newSudoTestState()
	resp := s.handleSudoCacheSet(Request{
		Profile:     "p",
		PasswordB64: base64.StdEncoding.EncodeToString([]byte("x")),
		TTLSec:      24 * 3600, // ask for 24h
	})
	if !resp.OK {
		t.Fatalf("set: %q", resp.Err)
	}
	entry := s.sudoCache["p"]
	limit := time.Now().Add(sudoCacheMaxTTL + time.Minute)
	if entry.expires.After(limit) {
		t.Errorf("TTL not clamped: expires=%v, max %v", entry.expires, limit)
	}
}

func TestSudoCache_RejectsBadBase64(t *testing.T) {
	s := newSudoTestState()
	resp := s.handleSudoCacheSet(Request{
		Profile:     "p",
		PasswordB64: "not-base64!!",
		TTLSec:      60,
	})
	if resp.OK {
		t.Error("expected set to fail on bad base64")
	}
}

func TestSudoCache_RejectsEmptyProfile(t *testing.T) {
	s := newSudoTestState()
	for _, op := range []func() Response{
		func() Response { return s.handleSudoCacheGet(Request{}) },
		func() Response { return s.handleSudoCacheSet(Request{PasswordB64: "eA=="}) },
		func() Response { return s.handleSudoCacheClear(Request{}) },
	} {
		resp := op()
		if resp.OK {
			t.Error("op with empty profile should fail")
		}
	}
}
