package hooks

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvIncludesOnlyPopulatedFields(t *testing.T) {
	got := Env(Event{Name: "pre-cd", Profile: "prod", Host: "h", Cwd: "/opt"})
	want := []string{
		"SRV_CWD=/opt",
		"SRV_HOOK=pre-cd",
		"SRV_HOST=h",
		"SRV_PROFILE=prod",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env minimal mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestEnvIncludesExitCodeForPostHooks(t *testing.T) {
	got := Env(Event{Name: "post-sync", Profile: "p", Exit: 2})
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "SRV_EXIT_CODE=2") {
		t.Errorf("post hook should include exit code: %v", got)
	}

	preGot := Env(Event{Name: "pre-sync", Profile: "p", Exit: 99})
	preJoined := strings.Join(preGot, ",")
	if strings.Contains(preJoined, "SRV_EXIT_CODE") {
		t.Errorf("pre hook should not include exit code: %v", preGot)
	}
}

func TestIsKnownEvent(t *testing.T) {
	if !IsKnownEvent("pre-cd") {
		t.Errorf("pre-cd should be known")
	}
	if IsKnownEvent("on-fire") {
		t.Errorf("on-fire should not be known")
	}
}
