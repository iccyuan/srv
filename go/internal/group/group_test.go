package group

import (
	"srv/internal/config"
	"strings"
	"testing"
)

// setCmd should reject members that aren't real profiles, dedupe
// while preserving the user-typed order, and persist via config.Save.
// The persist path is exercised indirectly: setCmd calls config.Save
// which we let do its thing (write to ~/.srv); the in-memory cfg
// mutation is what matters for the tests below.
func TestGroupSet_RejectsUnknownMember(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["a"] = &config.Profile{Host: "a"}
	// "b" is not a profile.
	err := setCmd(cfg, "myg", []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
	if !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name the missing profile: %v", err)
	}
	// Group must NOT be written when validation fails.
	if _, ok := cfg.Groups["myg"]; ok {
		t.Errorf("group was created despite validation failure")
	}
}

func TestRunGroup_RejectsUnknownGroup(t *testing.T) {
	cfg := config.New()
	_, err := Run(cfg, "nope", "uptime")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

func TestRunGroup_RejectsEmptyGroup(t *testing.T) {
	cfg := config.New()
	cfg.Groups = map[string][]string{"empty": {}}
	_, err := Run(cfg, "empty", "uptime")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' error, got %v", err)
	}
}

func TestRunGroup_RejectsGhostMember(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["real"] = &config.Profile{Host: "real"}
	cfg.Groups = map[string][]string{"g": {"real", "ghost"}}
	_, err := Run(cfg, "g", "uptime")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("expected 'ghost' in error, got %v", err)
	}
}

// RenderResults exits with the max non-zero exit code; dial
// failures (ExitCode=-1) surface as 255.
func TestRenderGroupResults_MaxExitCode(t *testing.T) {
	results := []Result{
		{Profile: "a", ExitCode: 0},
		{Profile: "b", ExitCode: 2},
		{Profile: "c", ExitCode: 5},
	}
	maxExit, failed := RenderResults(results)
	if maxExit != 5 {
		t.Errorf("maxExit=%d, want 5", maxExit)
	}
	if failed != 2 {
		t.Errorf("failed=%d, want 2", failed)
	}
}

func TestRenderGroupResults_DialFailureAs255(t *testing.T) {
	results := []Result{
		{Profile: "a", ExitCode: 0},
		{Profile: "b", ExitCode: -1, Error: "dial: timeout"},
	}
	maxExit, failed := RenderResults(results)
	if maxExit != 255 {
		t.Errorf("dial failure should surface as 255, got %d", maxExit)
	}
	if failed != 1 {
		t.Errorf("failed=%d, want 1", failed)
	}
}

func TestRenderGroupResults_AllSucceeded(t *testing.T) {
	results := []Result{
		{Profile: "a", ExitCode: 0},
		{Profile: "b", ExitCode: 0},
	}
	maxExit, failed := RenderResults(results)
	if maxExit != 0 || failed != 0 {
		t.Errorf("all-success: got maxExit=%d failed=%d, want 0/0", maxExit, failed)
	}
}

func TestGroupResultsJSON_Shape(t *testing.T) {
	rs := []Result{
		{Profile: "a", ExitCode: 0, Stdout: "ok\n", Duration: 1.5},
	}
	js := ResultsJSON(rs)
	for _, want := range []string{`"profile":"a"`, `"exit_code":0`, `"stdout":"ok\n"`, `"duration_seconds":1.5`} {
		if !strings.Contains(js, want) {
			t.Errorf("missing %q in JSON: %s", want, js)
		}
	}
}
