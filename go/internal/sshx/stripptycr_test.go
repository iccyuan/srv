package sshx

import "testing"

// PTY output maps \n -> \r\n; stripPTYCR must turn it back into the
// plain-pipe \n shape so the streaming-data-loss fix (RunStreamPTY,
// which line-buffers tail -F / journalctl -f on every platform)
// hands callers clean lines.
func TestStripPTYCR(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc\r\n", "abc\n"},       // typical PTY line
		{"\r\n", "\n"},             // empty PTY line
		{"abc\n", "abc\n"},         // already clean (no CR)
		{"abc\r", "abc"},           // partial EOF line w/ trailing CR
		{"abc", "abc"},             // partial EOF line, no newline
		{"", ""},                   // nothing
		{"a\r\nb\r\n", "a\r\nb\n"}, // only the final CRLF (reader is per-line)
	}
	for _, c := range cases {
		if got := stripPTYCR(c.in); got != c.want {
			t.Errorf("stripPTYCR(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
