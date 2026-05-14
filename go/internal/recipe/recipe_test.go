package recipe

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSteps(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"echo hi"}, []string{"echo hi"}},
		{[]string{"echo a ;; echo b"}, []string{"echo a", "echo b"}},
		{[]string{"echo a;; echo b ;;echo c"}, []string{"echo a", "echo b", "echo c"}},
		{[]string{"echo a;b"}, []string{"echo a;b"}}, // single `;` stays a shell-level sep
		{[]string{"  ;;  "}, nil},
	}
	for _, tc := range cases {
		got := parseSteps(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSteps(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSubstitutePositional(t *testing.T) {
	got := substitute("echo $1 $2 $3", []string{"a", "b"}, nil)
	if got != "echo a b " {
		t.Errorf("got %q", got)
	}
}

func TestSubstituteNamedBraced(t *testing.T) {
	got := substitute("deploy to ${HOST}:${PORT}", nil, map[string]string{"HOST": "h", "PORT": "22"})
	if got != "deploy to h:22" {
		t.Errorf("got %q", got)
	}
}

func TestSubstituteNamedBare(t *testing.T) {
	got := substitute("user=$USER", nil, map[string]string{"USER": "iccyuan"})
	if got != "user=iccyuan" {
		t.Errorf("got %q", got)
	}
}

func TestSubstituteUnsetCollapsesToEmpty(t *testing.T) {
	got := substitute("a=${X}b", nil, nil)
	if got != "a=b" {
		t.Errorf("got %q", got)
	}
}

func TestCollectVars(t *testing.T) {
	got := collectVars([]string{"echo $1 ${HOST}", "deploy $TAG to ${HOST}"})
	wantContains := []string{"$1", "HOST", "TAG"}
	joined := strings.Join(got, ",")
	for _, w := range wantContains {
		if !strings.Contains(joined, w) {
			t.Errorf("collectVars missing %q in %v", w, got)
		}
	}
}

func TestValidVarName(t *testing.T) {
	good := []string{"X", "_x", "X1", "FOO_BAR"}
	bad := []string{"", "1X", "FOO-BAR", "a b"}
	for _, s := range good {
		if !validVarName(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range bad {
		if validVarName(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}
