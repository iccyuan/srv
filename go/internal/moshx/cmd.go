package moshx

import (
	"fmt"
	"os"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/srvtty"
	"strings"
	"time"
)

// ClientCmd implements `srv mosh [--cmd "..."] [args...]`. Bootstraps
// the server over SSH, parses the connect line, opens UDP and hands
// off to RunClient.
//
// Notes on scope (v1):
//
//   - The remote must have `srv` on PATH (same binary).
//   - Without ConPTY plumbing, a Windows client can connect and
//     stream data, but local SIGWINCH isn't wired; the remote shell
//     keeps its initial geometry.
//   - No predictive local echo (real mosh's headline feature) -- byte
//     latency is what the underlying network gives, not what mosh
//     would mask.
//   - Session resumption across client process restarts is NOT
//     supported. NAT rebind / Wi-Fi → cellular within a single
//     process IS supported (codec state survives).
func ClientCmd(args []string, cfg *config.Config, profileOverride string) error {
	// Parse a tiny flag surface: --cmd "..." overrides the default
	// shell-login; remaining positionals are appended to the cmd.
	cmd := ""
	idleStr := ""
	rest := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--cmd":
			if i+1 >= len(args) {
				return clierr.Errf(2, "--cmd requires a value")
			}
			cmd = args[i+1]
			i++
		case strings.HasPrefix(a, "--cmd="):
			cmd = strings.TrimPrefix(a, "--cmd=")
		case a == "--idle":
			if i+1 >= len(args) {
				return clierr.Errf(2, "--idle requires a duration")
			}
			idleStr = args[i+1]
			i++
		case strings.HasPrefix(a, "--idle="):
			idleStr = strings.TrimPrefix(a, "--idle=")
		default:
			rest = append(rest, a)
		}
	}
	_, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}

	// Build the remote bootstrap command.
	remoteInvocation := buildRemoteInvocation(cmd, idleStr, rest)

	// SSH bootstrap. We use a captured RunCapture rather than
	// streaming because we only need one line (the connect banner)
	// and want the SSH channel to close once we have it. Output up
	// to ReadBootstrapLine's 64 KiB cap is fine; the server prints
	// the line as its very first write.
	bootstrap, err := sshBootstrap(profile, remoteInvocation)
	if err != nil {
		return clierr.Errf(1, "ssh bootstrap: %v", err)
	}
	port, secret, err := ParseBootstrapLine(bootstrap)
	if err != nil {
		return clierr.Errf(1, "ssh bootstrap: %v", err)
	}

	w, h := srvtty.Size()
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}
	banner := []byte("client=srv-mosh")

	fmt.Fprintf(os.Stderr, "srv mosh: connected to %s:%d via UDP (key rotated per session)\n",
		profile.Host, port)
	return RunClient(ClientOptions{
		RemoteHost:    profile.Host,
		RemotePort:    port,
		Secret:        secret,
		InitialRows:   uint16(h),
		InitialCols:   uint16(w),
		InitialBanner: banner,
	})
}

// buildRemoteInvocation composes the shell command we ask the remote
// to run inside the SSH bootstrap session. Quoting goes through the
// srvtty helper so a user's command with spaces / quotes survives.
func buildRemoteInvocation(userCmd string, idle string, extra []string) string {
	parts := []string{"srv", "mosh-server"}
	if idle != "" {
		parts = append(parts, "--idle", idle)
	}
	if userCmd != "" {
		parts = append(parts, "--cmd", srvtty.ShQuote(userCmd))
	} else if len(extra) > 0 {
		parts = append(parts, "--")
		parts = append(parts, extra...)
	}
	return strings.Join(parts, " ")
}

// ServerCmd implements `srv mosh-server` -- the entrypoint the SSH
// bootstrap invokes on the remote. Parses --cmd / --idle / -- args,
// then hands off to RunServer.
func ServerCmd(args []string) error {
	opts := ServerOptions{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--cmd":
			if i+1 >= len(args) {
				return clierr.Errf(2, "--cmd needs a value")
			}
			opts.Command = []string{"sh", "-c", args[i+1]}
			i++
		case strings.HasPrefix(a, "--cmd="):
			opts.Command = []string{"sh", "-c", strings.TrimPrefix(a, "--cmd=")}
		case a == "--idle":
			if i+1 >= len(args) {
				return clierr.Errf(2, "--idle needs a duration")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return clierr.Errf(2, "bad --idle duration: %v", err)
			}
			opts.IdleTimeout = d
			i++
		case strings.HasPrefix(a, "--idle="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--idle="))
			if err != nil {
				return clierr.Errf(2, "bad --idle duration: %v", err)
			}
			opts.IdleTimeout = d
		case a == "--":
			opts.Command = args[i+1:]
			i = len(args)
		default:
			return clierr.Errf(2, "unexpected arg %q", a)
		}
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	return RunServer(opts)
}
