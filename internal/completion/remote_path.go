package completion

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvpath"
	"srv/internal/sshx"
	"strings"
	"time"
)

// remotePathTTL governs how stale the cached remote-$PATH listing can
// be before we go fetch a fresh copy. One hour is long enough that
// repeated Tab completion never blocks on an SSH dial, short enough
// that newly-installed remote tools show up the same workday they
// were installed.
const remotePathTTL = time.Hour

// remotePathMax caps the number of cached entries so the completion
// menu stays usable. A typical Linux box has ~1500 entries in /usr/bin
// + /usr/local/bin; we trim to 4000 to leave headroom without piping
// truly absurd lists into bash's completion buffer.
const remotePathMax = 4000

// RemotePathCmd is the hidden `srv _remote_path` subcommand. The shell
// completion scripts call it to learn which remote binaries to offer
// alongside srv's own subcommands at the first positional slot.
//
// Failures are silent (Println nothing): completion mustn't blow up on
// errors. The cache layer covers cold connections so a fresh shell's
// first Tab still feels snappy after the first warm-up.
func RemotePathCmd(args []string, cfg *config.Config, profileOverride string) error {
	entries, err := ListRemotePath(cfg, profileOverride)
	if err != nil {
		// Surface nothing to the shell -- a noisy "could not dial"
		// would corrupt completion output. The caller already has
		// srv's own subcommand list to fall back on.
		return nil
	}
	for _, e := range entries {
		fmt.Println(e)
	}
	return nil
}

// ListRemotePath returns the active profile's remote-PATH executables,
// reading from cache when fresh and refreshing in-band when stale.
// Entries are sorted, deduplicated, capped at remotePathMax.
func ListRemotePath(cfg *config.Config, profileOverride string) ([]string, error) {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return nil, err
	}
	cachePath := pathCacheFile(name)
	if entries, ok := readPathCache(cachePath); ok {
		return entries, nil
	}
	entries, err := fetchRemotePath(profile)
	if err != nil {
		return nil, err
	}
	_ = writePathCache(cachePath, entries)
	return entries, nil
}

// InvalidatePathCache deletes the cached PATH listing for `profile`,
// forcing the next completion to re-fetch. Called when the user
// changes profile or runs `srv config set ... remote-path` (future).
func InvalidatePathCache(profile string) {
	_ = os.Remove(pathCacheFile(profile))
}

func pathCacheFile(profile string) string {
	return filepath.Join(srvpath.Dir(), "cache", "remote-path-"+sanitizeProfile(profile)+".txt")
}

// sanitizeProfile replaces filesystem-hostile chars in a profile name
// so the cache filename is always safe. Profiles typically match
// [A-Za-z0-9_.-]+ already; this is belt-and-braces.
func sanitizeProfile(s string) string {
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

func readPathCache(path string) ([]string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(st.ModTime()) > remotePathTTL {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out, true
}

func writePathCache(path string, entries []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(entries) > remotePathMax {
		entries = entries[:remotePathMax]
	}
	return os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o644)
}

// fetchRemotePath enumerates executables on every directory of the
// remote $PATH. Implemented in POSIX sh (no bashisms) so it works on
// minimal images. Daemon-pooled when available; cold direct-dial
// fallback only fires when no daemon is reachable.
func fetchRemotePath(profile *config.Profile) ([]string, error) {
	// POSIX sh: split PATH on `:`, list each dir, dedupe.
	script := `IFS=:; for d in $PATH; do [ -d "$d" ] && ls -1 "$d" 2>/dev/null; done | sort -u`

	// Daemon fast path (warm pooled SSH).
	if res, ok := daemon.TryRunCapture(profile.Name, "", script); ok && res.ExitCode == 0 {
		return parsePathOutput(res.Stdout), nil
	}

	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.RunCapture(script, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("remote path enumerate exit %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parsePathOutput(res.Stdout), nil
}

func parsePathOutput(stdout string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, ln := range strings.Split(stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		// Skip entries with shell-meta chars -- those break compgen's
		// word splitter on the bash side and would surface as broken
		// completion candidates.
		if strings.ContainsAny(ln, " \t\\\"'$`|&;<>(){}[]") {
			continue
		}
		if _, ok := seen[ln]; ok {
			continue
		}
		seen[ln] = struct{}{}
		out = append(out, ln)
	}
	sort.Strings(out)
	return out
}
