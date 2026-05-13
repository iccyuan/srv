package daemon

import (
	"fmt"
	"os"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"strings"
)

// DisconnectCmd implements `srv disconnect [profile] [-P prof] [--all]`.
//
// Behavior:
//
//	srv disconnect              disconnect the currently active profile
//	srv disconnect <name>       disconnect that specific profile
//	srv disconnect --all        close every pooled SSH client + wipe ls cache
//
// "Disconnect" means: close the daemon's pooled SSH client and drop
// the matching ls-cache rows. The next call referencing the profile
// will re-dial cold. Tunnels are NOT torn down here -- their state
// is managed by `srv tunnel up|down|remove`.
//
// If no daemon is running there's nothing to do; we say so and exit
// 0 (success, idempotent).
func DisconnectCmd(args []string, cfg *config.Config, profileOverride string) error {
	all := false
	var target string
	for _, a := range args {
		switch {
		case a == "--all":
			all = true
		case a == "-h" || a == "--help":
			fmt.Println(disconnectHelp)
			return nil
		case strings.HasPrefix(a, "-"):
			return clierr.Errf(2, "unknown flag %q (try --help)", a)
		default:
			if target != "" {
				return clierr.Errf(2, "too many arguments (expected one profile name or --all)")
			}
			target = a
		}
	}
	if all && target != "" {
		return clierr.Errf(2, "--all and an explicit profile are mutually exclusive")
	}

	if !Ping() {
		// No daemon = nothing pooled = nothing to disconnect. This
		// is an expected steady state for users who don't keep srv
		// hot, so phrase it as info, not failure.
		fmt.Println("(no daemon running; nothing to disconnect)")
		return nil
	}

	if all {
		res := DisconnectAll()
		if !res.OK {
			return clierr.Errf(1, "daemon disconnect_all call failed")
		}
		if len(res.Freed) == 0 {
			fmt.Println("(no profiles connected)")
			return nil
		}
		sort.Strings(res.Freed)
		fmt.Printf("disconnected %d profile(s): %s\n", len(res.Freed), strings.Join(res.Freed, ", "))
		return nil
	}

	// Resolve the target profile. Explicit arg wins; otherwise fall
	// back to the global -P flag's resolution path, which honours
	// the session pin / default.
	if target == "" {
		name, _, err := config.Resolve(cfg, profileOverride)
		if err != nil {
			return clierr.Errf(1, "%v", err)
		}
		target = name
	} else {
		// Validate the name exists in the config so a typo doesn't
		// silently succeed (the daemon's pool might not have the
		// typo'd name, which would render as "(not connected)" --
		// indistinguishable from a real miss without this check).
		if _, ok := cfg.Profiles[target]; !ok {
			fmt.Fprintf(os.Stderr, "srv disconnect: profile %q not in config; nothing matched in pool either.\n", target)
		}
	}
	res := Disconnect(target)
	if !res.OK {
		return clierr.Errf(1, "daemon disconnect call failed")
	}
	if len(res.Freed) == 0 {
		fmt.Printf("(%s: not connected)\n", target)
		return nil
	}
	fmt.Printf("disconnected: %s\n", target)
	return nil
}

const disconnectHelp = `srv disconnect -- close the daemon's pooled SSH client for a profile

USAGE:
  srv disconnect [profile]       drop the pool entry for one profile
                                 (defaults to the active profile)
  srv disconnect --all           drop every pooled connection +
                                 wipe the ls cache

What this does:
  Closes the daemon's cached SSH client so the next call referencing
  that profile re-dials. Useful when a connection has gone weird
  (NAT timeout, network changed under it, server-side restart) and
  you want to force a fresh handshake without restarting the daemon.

What this does NOT do:
  Running tunnels are not torn down. Use ` + "`srv tunnel down <name>`" + ` for
  those -- a tunnel may share its underlying SSH session with the
  pool but its lifecycle is managed separately.

If no daemon is running, this is a no-op (exits 0 with a notice).`
