package transfer

import (
	"strings"
	"testing"
)

func TestBuildDownloadCmd(t *testing.T) {
	url := "https://example.com/f.tgz"
	prog := buildDownloadCmd("/tmp/d", "f.tgz", url, true)
	quiet := buildDownloadCmd("/tmp/d", "f.tgz", url, false)

	// Progress variant draws a live bar; quiet variant stays parseable.
	if !strings.Contains(prog, "--progress-bar") || !strings.Contains(prog, "--progress=bar:force") {
		t.Errorf("progress cmd missing bar flags: %s", prog)
	}
	if strings.Contains(quiet, "--progress-bar") || strings.Contains(quiet, "bar:force") {
		t.Errorf("quiet cmd should not draw a bar: %s", quiet)
	}
	if !strings.Contains(quiet, "-fsSL") || !strings.Contains(quiet, "wget -nv") {
		t.Errorf("quiet cmd missing silent flags: %s", quiet)
	}
	// Both keep curl's -f (fail on HTTP errors) and fall back to wget.
	for _, c := range []string{prog, quiet} {
		if !strings.Contains(c, "command -v curl") || !strings.Contains(c, "command -v wget") {
			t.Errorf("cmd missing curl/wget fallback: %s", c)
		}
	}
}

func TestRelayFileName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://example.com/path/file.tar.gz", "file.tar.gz"},
		{"https://example.com/path/file.tar.gz?token=abc&x=1", "file.tar.gz"},
		{"http://host/a/b/c/installer.sh#section", "installer.sh"},
		{"https://host/file.zip", "file.zip"},
		{"https://host/", "download"},
		{"https://host", "download"},
		{"https://host/dir/", "dir"}, // path.Base trims the trailing slash
	}
	for _, c := range cases {
		if got := relayFileName(c.url); got != c.want {
			t.Errorf("relayFileName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
