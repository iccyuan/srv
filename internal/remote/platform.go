package remote

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvutil"
	"srv/internal/sshx"
)

// Platform is a coarse identifier of the remote operating system kind.
// Coarse on purpose: callers branch on "is this a systemd Linux or
// the macOS unified-logging world" and rarely care beyond that.
type Platform string

const (
	PlatformLinux   Platform = "linux"
	PlatformDarwin  Platform = "darwin"
	PlatformOther   Platform = "other"   // BSD / illumos / WSL / etc -- recognized but no special-case
	PlatformUnknown Platform = "unknown" // probe failed or never ran
)

// platformCacheTTL is how long a cached platform value is trusted
// before we re-probe. Operating systems don't shape-shift, so 24h is
// generous; the real point of the TTL is "if a profile gets retargeted
// to a different host without changing its name, we'll notice within a
// day." Re-probes are cheap (one `uname -s` on the warm pool).
const platformCacheTTL = 24 * time.Hour

// GetPlatform resolves a profile's remote platform. Resolution order:
//
//  1. Profile.Platform set explicitly to one of linux/darwin/other ->
//     use it verbatim. This is the escape hatch for stripped-down
//     remotes where `uname -s` lies or isn't available, and the
//     "always treat this profile as X" guarantee callers may need.
//  2. Fresh on-disk cache at ~/.srv/cache/platform-<profile>.txt ->
//     parse and return.
//  3. Probe the remote (`uname -s`), normalize the result, write
//     cache, return.
//  4. Probe failed -> return PlatformUnknown. Cache is NOT written
//     for the failure case so the next call retries instead of
//     pinning "unknown" for 24h. Callers must treat unknown as "no
//     information, behave as the cross-platform default" -- never as
//     a green light to make platform-specific assumptions.
//
// Profile.Name MUST be set (config.Resolve populates it); a blank
// name short-circuits to PlatformUnknown to avoid writing a
// cache-file collision under `platform-.txt`.
func GetPlatform(profile *config.Profile) Platform {
	if profile == nil || profile.Name == "" {
		return PlatformUnknown
	}
	// Step 1: explicit profile override wins absolutely.
	if p := normalizePlatform(profile.Platform); p != "" && p != PlatformUnknown {
		return p
	}
	// Step 2: fresh cache.
	cachePath := platformCacheFile(profile.Name)
	if p, ok := readPlatformCache(cachePath); ok {
		return p
	}
	// Step 3: probe.
	raw, ok := probePlatform(profile)
	if !ok {
		return PlatformUnknown
	}
	p := normalizePlatform(raw)
	if p == "" {
		p = PlatformOther
	}
	_ = writePlatformCache(cachePath, p)
	return p
}

// InvalidatePlatformCache deletes the cached platform value for a
// profile so the next GetPlatform call re-probes. Exposed so config
// edits (`srv config set ... host X.Y.Z` retargeting to a new box)
// and CLI commands that explicitly want a fresh detection can force
// it without waiting out the 24h TTL.
func InvalidatePlatformCache(profileName string) {
	if profileName == "" {
		return
	}
	_ = os.Remove(platformCacheFile(profileName))
}

// normalizePlatform takes a raw string (Profile.Platform field, or
// the output of `uname -s`) and folds it into the Platform enum.
// Case-insensitive; whitespace-tolerant; unknown values map to "".
func normalizePlatform(s string) Platform {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linux":
		return PlatformLinux
	case "darwin":
		return PlatformDarwin
	case "freebsd", "openbsd", "netbsd", "dragonfly", "sunos", "illumos", "aix":
		return PlatformOther
	case "":
		return ""
	case "unknown":
		return PlatformUnknown
	default:
		return ""
	}
}

func platformCacheFile(profileName string) string {
	return filepath.Join(srvutil.Dir(), "cache", "platform-"+sanitizePlatformProfileName(profileName)+".txt")
}

// sanitizePlatformProfileName replaces filesystem-hostile characters
// in a profile name so the cache filename is always safe. Duplicates
// the equivalent helper in internal/completion to avoid pulling that
// package into the remote dependency graph.
func sanitizePlatformProfileName(s string) string {
	if s == "" {
		return "_"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func readPlatformCache(path string) (Platform, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if time.Since(st.ModTime()) > platformCacheTTL {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	p := normalizePlatform(string(data))
	if p == "" {
		// A corrupted/edited cache file is a "no info" signal --
		// re-probe rather than trust the noise.
		return "", false
	}
	return p, true
}

func writePlatformCache(path string, p Platform) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(string(p)+"\n"), 0o644)
}

// probePlatform runs a single `uname -s` over the warm daemon pool
// (or a fresh dial as fallback) and returns the trimmed stdout. The
// command is universally available on Linux/macOS/BSD/illumos -- the
// one class of remote where it isn't is bare BusyBox sh on certain
// stripped containers, and those rarely have profiles pointing at
// them. A non-zero exit code is treated as "couldn't determine" so
// callers get PlatformUnknown.
func probePlatform(profile *config.Profile) (string, bool) {
	const cmd = "uname -s"
	// Daemon fast path mirrors completion.fetchRemotePath: a warm
	// pool gives sub-100ms; cold dial gives ~200-800ms (single ssh
	// handshake). Either way the result gets cached, so callers in
	// the same 24h window pay zero further latency.
	if res, ok := daemon.TryRunCapture(profile.Name, "", cmd); ok {
		if res.ExitCode == 0 {
			return strings.TrimSpace(res.Stdout), true
		}
		return "", false
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		return "", false
	}
	defer c.Close()
	res, err := c.RunCapture("", cmd)
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(res.Stdout), true
}
