package moshx

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/sshx"
	"strings"
)

// sshBootstrap runs `srv mosh-server ...` on the remote and returns
// the first bootstrap line it prints. Tolerates pre-banner shell
// output (motd, profile sourcing) -- ReadBootstrapLine scans until
// it sees "SRV-MOSH-CONNECT".
//
// Captures the full session output rather than streaming because the
// connect banner is one line and the SSH channel can close immediately
// once we have it. The server detaches via SIGHUP-ignore so closing
// the channel doesn't take it down.
func sshBootstrap(profile *config.Profile, remoteCmd string) (string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return "", err
	}
	defer c.Close()

	// Wrap in a subshell so the user's RC file is sourced (and PATH
	// includes whichever directory srv lives in for this user). nohup
	// keeps the server alive when the channel closes; redirecting
	// stdin/stderr off the pipe means we only see clean stdout.
	wrapped := fmt.Sprintf("nohup %s 2>/dev/null < /dev/null", remoteCmd)
	res, err := c.RunCapture(wrapped, "")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stdout, "SRV-MOSH-CONNECT ") {
		return "", fmt.Errorf("remote srv mosh-server exit %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	// Find the line within stdout; the server prints it as its very
	// first write so it should be near the top.
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "SRV-MOSH-CONNECT ") {
			return line, nil
		}
	}
	return "", fmt.Errorf("remote did not emit a bootstrap line (got %d bytes of stdout)", len(res.Stdout))
}
