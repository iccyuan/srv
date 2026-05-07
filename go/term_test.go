package main

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
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShQuotePath(t *testing.T) {
	// shQuotePath must preserve a leading ~ or ~/ so the remote shell
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
		if got := shQuotePath(in); got != want {
			t.Errorf("shQuotePath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestBase64Encode(t *testing.T) {
	if got := base64Encode("hi"); got != "aGk=" {
		t.Errorf("base64Encode(hi) = %q; want aGk=", got)
	}
}

func TestAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":    false,
		"123": true,
		"12a": false,
		"-1":  false,
	}
	for in, want := range cases {
		if got := allDigits(in); got != want {
			t.Errorf("allDigits(%q) = %v; want %v", in, got, want)
		}
	}
}
