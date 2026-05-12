package tunnel

import (
	"srv/internal/config"
	"strings"
	"testing"
)

// cmdAdd parsing: -L / -R / --autostart / -P / spec. We can drive
// the parser directly by constructing a minimal Config and faking the
// config.Save path via t.TempDir + SRV_HOME override.
func TestTunnelAdd_LocalDefault(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["prod"] = &config.Profile{Host: "h"}
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdAdd([]string{"db", "5432:db:5432", "-P", "prod"}, cfg, "")
	if err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	def := cfg.Tunnels["db"]
	if def == nil {
		t.Fatal("tunnel not saved")
	}
	if def.Type != "local" {
		t.Errorf("Type=%q, want local", def.Type)
	}
	if def.Spec != "5432:db:5432" {
		t.Errorf("Spec=%q", def.Spec)
	}
	if def.Profile != "prod" {
		t.Errorf("config.Profile=%q, want prod", def.Profile)
	}
	if def.Autostart {
		t.Error("Autostart should default false")
	}
}

func TestTunnelAdd_ReverseAutostart(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["x"] = &config.Profile{Host: "h"}
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdAdd([]string{"rev", "-R", "9000:3000", "-P", "x", "--autostart"}, cfg, "")
	if err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	def := cfg.Tunnels["rev"]
	if def == nil {
		t.Fatal("tunnel not saved")
	}
	if def.Type != "remote" {
		t.Errorf("Type=%q, want remote", def.Type)
	}
	if !def.Autostart {
		t.Error("Autostart should be true")
	}
}

func TestTunnelAdd_RejectsBadSpec(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["x"] = &config.Profile{Host: "h"}
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdAdd([]string{"bad", "not-a-port", "-P", "x"}, cfg, "")
	if err == nil || !strings.Contains(err.Error(), "bad spec") {
		t.Errorf("expected bad spec error, got %v", err)
	}
}

func TestTunnelAdd_RejectsUnknownProfile(t *testing.T) {
	cfg := config.New()
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdAdd([]string{"x", "8080", "-P", "ghost"}, cfg, "")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("expected ghost profile error, got %v", err)
	}
}

func TestTunnelAdd_RequiresSpec(t *testing.T) {
	cfg := config.New()
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdAdd([]string{"x"}, cfg, "")
	if err == nil {
		t.Fatal("expected error for missing spec")
	}
}

func TestTunnelRemove_Unknown(t *testing.T) {
	cfg := config.New()
	t.Setenv("SRV_HOME", t.TempDir())
	err := cmdRemove([]string{"nope"}, cfg)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found, got %v", err)
	}
}

func TestTunnelRemove_DeletesAndPersists(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["x"] = &config.Profile{Host: "h"}
	t.Setenv("SRV_HOME", t.TempDir())
	if err := cmdAdd([]string{"db", "5432", "-P", "x"}, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if err := cmdRemove([]string{"db"}, cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tunnels["db"]; ok {
		t.Error("tunnel still in cfg after remove")
	}
}
