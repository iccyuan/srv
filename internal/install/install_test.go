package install

import (
	"net/http"
	"net/http/httptest"
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

func TestInstallHTMLKeepsFunctionalHooks(t *testing.T) {
	html := string(installHTML)
	for _, want := range []string{
		"/api/status",
		"/api/apply",
		"/api/quit",
		"add_to_path",
		"remove_from_path",
		"register_claude_mcp",
		"unregister_claude_mcp",
		"register_codex_mcp",
		"unregister_codex_mcp",
		"init_profile",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("install HTML missing functional hook %q", want)
		}
	}
}

func TestAuthorizeAPI(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const listen = "127.0.0.1:43210"
	mk := func(method, tok, host, origin string) *http.Request {
		r := httptest.NewRequest(method, "http://"+listen+"/api/apply", nil)
		r.Host = host
		if tok != "" {
			r.Header.Set("X-Srv-Token", tok)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{"ok same-origin", mk("POST", token, listen, "http://"+listen), true},
		{"ok no origin header", mk("POST", token, listen, ""), true},
		{"ok localhost spelling", mk("POST", token, "localhost:43210", "http://localhost:43210"), true},
		{"missing token", mk("POST", "", listen, ""), false},
		{"wrong token", mk("POST", "ffff", listen, ""), false},
		{"wrong method", mk("GET", token, listen, ""), false},
		{"foreign origin", mk("POST", token, listen, "http://evil.example"), false},
		{"foreign host", mk("POST", token, "evil.example:80", ""), false},
		{"other port", mk("POST", token, "127.0.0.1:1", ""), false},
	}
	for _, tc := range cases {
		if got := authorizeAPI(tc.req, http.MethodPost, token, listen); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestInstallHTMLCarriesToken(t *testing.T) {
	html := string(installHTML)
	// The placeholder must appear exactly where Cmd substitutes it,
	// and every /api/* call must go through the api() helper that
	// attaches X-Srv-Token -- a bare fetch('/api/...') would be
	// rejected by authorizeAPI.
	if !strings.Contains(html, `<meta name="srv-token" content="`+tokenPlaceholder+`">`) {
		t.Fatal("install HTML missing srv-token meta placeholder")
	}
	if !strings.Contains(html, "'X-Srv-Token': SRV_TOKEN") {
		t.Fatal("install HTML api() helper must send X-Srv-Token")
	}
	if strings.Contains(html, "fetch('/api/") {
		t.Fatal("install HTML has a bare fetch('/api/...') that bypasses the token helper")
	}
	tok, err := newToken()
	if err != nil || len(tok) != 64 {
		t.Fatalf("newToken: %q %v", tok, err)
	}
}
