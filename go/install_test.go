package main

import (
	"strings"
	"testing"
)

func TestRemoveCodexMcpSection(t *testing.T) {
	input := `[plugins.foo]
enabled = true

[mcp_servers.srv]
command = "old"
args = ["mcp"]

[projects.bar]
trust_level = "trusted"
`
	got, removed := removeCodexMcpSection(input)
	if !removed {
		t.Fatal("removeCodexMcpSection did not report removal")
	}
	if strings.Contains(got, "[mcp_servers.srv]") || strings.Contains(got, `command = "old"`) {
		t.Fatalf("srv section was not removed:\n%s", got)
	}
	if !strings.Contains(got, "[plugins.foo]") || !strings.Contains(got, "[projects.bar]") {
		t.Fatalf("unrelated sections were not preserved:\n%s", got)
	}
}

func TestCodexMcpSectionAndCommand(t *testing.T) {
	input := `[mcp_servers.srv]
command = "D:\\WorkSpace\\server\\srv\\srv.exe"
args = ["mcp"]

[windows]
sandbox = "elevated"
`
	section := codexMcpSection(input)
	if section == "" {
		t.Fatal("codexMcpSection returned empty section")
	}
	got := parseCodexMcpCommand(section)
	want := `D:\WorkSpace\server\srv\srv.exe`
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}
