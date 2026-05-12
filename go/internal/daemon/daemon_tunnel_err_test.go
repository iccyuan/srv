package daemon

import (
	"errors"
	"testing"
)

func newTunnelErrTestState() *daemonState {
	return &daemonState{
		tunnels:   map[string]*activeTunnel{},
		tunnelErr: map[string]string{},
	}
}

func TestRecordTunnelErr_PersistsMsg(t *testing.T) {
	s := newTunnelErrTestState()
	s.recordTunnelErr("db", errors.New("dial timeout"))
	if got := s.tunnelErr["db"]; got != "dial timeout" {
		t.Errorf("tunnelErr[\"db\"] = %q, want %q", got, "dial timeout")
	}
}

func TestRecordTunnelErr_NilIsNoOp(t *testing.T) {
	s := newTunnelErrTestState()
	s.tunnelErr["existing"] = "old"
	s.recordTunnelErr("existing", nil)
	// nil shouldn't clobber a prior error; clearTunnelErr is the
	// explicit "we succeeded, drop the prior failure".
	if got := s.tunnelErr["existing"]; got != "old" {
		t.Errorf("nil err clobbered existing entry: got %q", got)
	}
}

func TestClearTunnelErr_RemovesEntry(t *testing.T) {
	s := newTunnelErrTestState()
	s.tunnelErr["db"] = "stale"
	s.clearTunnelErr("db")
	if _, ok := s.tunnelErr["db"]; ok {
		t.Error("clearTunnelErr should remove the entry")
	}
}

func TestClearTunnelErr_OnEmptyNoPanic(t *testing.T) {
	s := newTunnelErrTestState()
	s.clearTunnelErr("never-existed") // shouldn't panic
}

func TestHandleTunnelList_IncludesErrors(t *testing.T) {
	s := newTunnelErrTestState()
	s.tunnelErr["db"] = "profile prod not found"
	s.tunnelErr["stats"] = "dial timeout"

	resp := s.handleTunnelList(Request{Op: "tunnel_list"})
	if !resp.OK {
		t.Fatal("tunnel_list should succeed")
	}
	if len(resp.TunnelErrors) != 2 {
		t.Errorf("expected 2 error entries, got %d", len(resp.TunnelErrors))
	}
	if resp.TunnelErrors["db"] != "profile prod not found" {
		t.Errorf("db err: %q", resp.TunnelErrors["db"])
	}
	if resp.TunnelErrors["stats"] != "dial timeout" {
		t.Errorf("stats err: %q", resp.TunnelErrors["stats"])
	}
}

func TestHandleTunnelList_NoErrorsYieldsEmptyMap(t *testing.T) {
	s := newTunnelErrTestState()
	resp := s.handleTunnelList(Request{Op: "tunnel_list"})
	if !resp.OK {
		t.Fatal("tunnel_list should succeed on empty state")
	}
	if len(resp.TunnelErrors) != 0 {
		t.Errorf("expected empty errors map, got %v", resp.TunnelErrors)
	}
}
