package srvtty

import "testing"

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"":             "''",
		"foo":          "foo",
		"hello world":  "'hello world'",
		"a'b":          `'a'\''b'`,
		"$HOME":        "'$HOME'",
		"already-safe": "already-safe",
	}
	for in, want := range cases {
		if got := ShQuote(in); got != want {
			t.Errorf("ShQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShQuotePath(t *testing.T) {
	// ShQuotePath must preserve a leading ~ or ~/ so the remote shell
	// expands it. This is the bug we hit in v2.0.1: cwd="~" got quoted
	// as 'srv', breaking every subsequent run.
	cases := map[string]string{
		"~":            "~",
		"~/foo":        "~/foo",
		"~/some path":  "~/'some path'",
		"/opt/app":     "/opt/app",
		"/opt/with sp": "'/opt/with sp'",
		"plain":        "plain",
	}
	for in, want := range cases {
		if got := ShQuotePath(in); got != want {
			t.Errorf("ShQuotePath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestBase64Encode(t *testing.T) {
	if got := Base64Encode("hi"); got != "aGk=" {
		t.Errorf("Base64Encode(hi) = %q; want aGk=", got)
	}
}
