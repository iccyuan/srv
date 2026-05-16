// Package remote is the "do X on this profile" layer that sits
// directly above sshx + daemon. Every feature module reaches for
// these helpers when it needs to run a command, capture output, or
// resolve a cwd against a named profile -- they handle the
// daemon-pooled fast path (warm SSH connection, no handshake) with
// a direct sshx.Dial fallback when no daemon is reachable.
//
// Living in its own package means feature packages (streams, sync,
// transfer, ui, mcp tool handlers) don't have to import the daemon
// client and the SSH client separately; one import does both.
package remote

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"srv/internal/jobs"
	"srv/internal/srvutil"
	"strings"

	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/session"
	"srv/internal/srvtty"
	"srv/internal/sshx"
)

// RunStream opens a connection, runs `cmd` interactively (streaming
// stdio), and closes. Returns remote exit code.
//
// Non-TTY runs go through the daemon when one is available -- the
// daemon's pooled SSH connection saves the ~2.7s handshake. The
// daemon streams stdout/stderr as base64 chunks (stream_run op) so
// commands like `tail -f` and `find /` produce real-time output,
// not buffered.
func RunStream(profile *config.Profile, cwd, cmd string, tty bool) int {
	if !tty {
		if rc, ok := daemon.TryStreamRun(profile.Name, cwd, cmd); ok {
			return rc
		}
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srv: ssh dial %s: %v\n", profile.Host, err)
		return 255
	}
	defer c.Close()
	rc, err := c.RunInteractive(cmd, cwd, tty)
	if err != nil {
		fmt.Fprintf(os.Stderr, "srv: %v\n", err)
		if rc == 0 {
			return 255
		}
	}
	return rc
}

// RunCapture opens a connection, runs `cmd` capturing output,
// closes.
//
// Tries the daemon first when the profile is named -- the pooled
// SSH connection reuses the handshake (~2.7s cold) and avoids
// spawning a fresh keepalive goroutine per call. Falls back to a
// direct dial when no daemon is reachable.
//
// Note: this is the path used by the MCP server (`run` tool, etc).
// We deliberately do NOT inject any shell prologue here -- MCP
// wants plain text the model can parse. CLI-only colour / init-file
// support lives in cmdRun, where it belongs.
func RunCapture(profile *config.Profile, cwd, cmd string) (*sshx.RunCaptureResult, error) {
	cmd = ApplyEnv(profile, cmd)
	if profile.Name != "" {
		if res, ok := daemon.TryRunCapture(profile.Name, cwd, cmd); ok {
			return res, nil
		}
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		return &sshx.RunCaptureResult{
			Stderr:   "ssh dial failed: " + err.Error(),
			ExitCode: 255,
			Cwd:      cwd,
		}, nil
	}
	defer c.Close()
	return c.RunCapture(cmd, cwd)
}

// ChangeCwd validates a target path on the remote and persists the
// absolute result for the current session+profile. Tries the daemon
// first (warm pooled SSH); falls back to a direct dial if no
// daemon. Returns (newCwd, error). On failure, returns ("", err).
func ChangeCwd(profileName string, profile *config.Profile, target string) (string, error) {
	if target == "" {
		target = "~"
	}
	current := config.GetCwd(profileName, profile)

	// Fast path via daemon.
	if newCwd, used, err := daemon.TryCd(profileName, current, target); used {
		if err != nil {
			return "", err
		}
		if perr := config.SetCwd(profileName, newCwd); perr != nil {
			return "", perr
		}
		return newCwd, nil
	}

	// Direct dial fallback.
	newCwd, err := ValidateCwd(profile, current, target)
	if err != nil {
		return "", err
	}
	if err := config.SetCwd(profileName, newCwd); err != nil {
		return "", err
	}
	return newCwd, nil
}

// ValidateCwd is the side-effect-free "cd ... && pwd" probe that
// returns the resolved absolute path. Used both by direct-dial cwd
// changes and by the daemon's cd handler.
func ValidateCwd(profile *config.Profile, current, target string) (string, error) {
	if current == "" {
		current = "~"
	}
	if target == "" {
		target = "~"
	}
	cmd := fmt.Sprintf(
		"cd %s 2>/dev/null || cd ~; cd %s && pwd",
		srvtty.ShQuotePath(current), srvtty.ShQuotePath(target),
	)
	res, err := RunCapture(profile, "", cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("cd failed (exit %d)", res.ExitCode)
		}
		return "", errors.New(stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		return "", errors.New("remote did not return a path")
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// ResolvePath anchors a remote path: absolute or ~-prefixed stays
// as-is, otherwise prepended with cwd.
func ResolvePath(remote, cwd string) string {
	if remote == "" {
		return cwd
	}
	if strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, "~") {
		return remote
	}
	return strings.TrimRight(cwd, "/") + "/" + remote
}

// ValidateRemotePath rejects remote targets that are really
// Windows-shaped local paths. ResolvePath anchors any non-/,
// non-~ string under cwd, so `srv push foo C:\tmp\x` (or a
// script/test that reuses a Windows local path as the remote
// arg) used to MkdirAll a literal `~/C:/.../x` garbage tree on
// the server instead of erroring. Remote paths are POSIX by
// contract: a drive-letter prefix or any backslash is never a
// legitimate POSIX target and is always a leaked Windows path.
// Empty stays valid -- callers read it as "use cwd".
func ValidateRemotePath(p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("remote path %q contains a backslash; remote paths must be POSIX (forward slashes, no Windows path)", p)
	}
	// Windows drive prefix: one ASCII letter, ':', then end / '/'
	// / '\'. Tight enough not to flag a legit POSIX relative name
	// that merely contains a colon (e.g. "a:b", "deploy:2024").
	if len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':' &&
		(len(p) == 2 || p[2] == '/' || p[2] == '\\') {
		return fmt.Errorf("remote path %q looks like a Windows drive path; remote paths must be POSIX (no %c: prefix)", p, p[0])
	}
	return nil
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ApplyEnv exports the profile's Env map into the shell scope before
// running the user's command: `export K1=v1 K2=v2; <cmd>`.
//
// It deliberately does NOT use the `K=v cmd` simple-command prefix.
// That form is a shell *syntax error* in front of a compound command
// (`for`/`while`/`if`/`{ }`/`case`) -- e.g. `K=v for i ...; do` --
// and, for `;`/`&&`-joined lines, only binds to the first command.
// `srv` runs arbitrary user commands (and the MCP wait_job poll is a
// `for` loop), so the prefix form broke every compound command the
// moment any env var was set. `export ...;` scopes the vars for the
// whole line regardless of its shape.
//
// Order is deterministic (sorted keys) so cache hits on the same cmd
// repeat byte-for-byte. Empty/no env is a pure pass-through so the
// command stays exactly what the user typed.
func ApplyEnv(profile *config.Profile, cmd string) string {
	if profile == nil || len(profile.Env) == 0 {
		return cmd
	}
	keys := make([]string, 0, len(profile.Env))
	for k := range profile.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		parts = append(parts, k+"="+srvtty.ShQuote(profile.Env[k]))
	}
	if len(parts) == 0 {
		return cmd
	}
	return "export " + strings.Join(parts, " ") + "; " + cmd
}

// silence unused import when session isn't directly referenced in
// some Go toolchains (session is reached transitively via config
// helpers GetCwd/SetCwd).
var _ = session.ID

// SpawnDetached runs `userCmd` on the remote with nohup, returns the
// newly-persisted jobs.Record. Used by `srv -d <cmd>` (CLI) and the
// `run` MCP tool's background=true mode.
func SpawnDetached(profileName string, profile *config.Profile, userCmd string) (*jobs.Record, error) {
	cwd := config.GetCwd(profileName, profile)

	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	jobID := srvutil.GenJobID()
	pid, err := c.RunDetached(ApplyEnv(profile, userCmd), cwd, jobID)
	if err != nil {
		return nil, err
	}
	rec := &jobs.Record{
		ID:      jobID,
		Profile: profileName,
		Cmd:     userCmd,
		Cwd:     cwd,
		Pid:     pid,
		Log:     "~/.srv-jobs/" + jobID + ".log",
		Started: srvutil.NowISO(),
	}
	jf := jobs.Load()
	jf.Jobs = append(jf.Jobs, rec)
	if err := jobs.Save(jf); err != nil {
		return rec, err
	}
	return rec, nil
}
