package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SessionRecord mirrors the Python schema:
//
//	{ "profile":      <pinned profile or null>,
//	  "cwds":         { profileName: cwd, ... },
//	  "guard":        <bool, optional>,
//	  "color_preset": <string, optional>,
//	  "started":      iso, "last_seen": iso }
//
// Guard, when true, makes the MCP server refuse high-risk operations
// (destructive `run`/`detach` patterns, `sync` with delete) unless the
// caller passes confirm=true. Default off so existing flows are
// unchanged. Toggled per-shell via `srv guard on|off`, or globally via
// the SRV_GUARD env var (which trumps the session record).
//
// ColorPreset names a shell snippet under ~/.srv/init/<name>.sh that
// `srv <cmd>` (CLI non-TTY) inlines before the user's command. Empty
// means use the platform default (forward local LS_COLORS on
// linux/mac, nothing on windows). Toggled via `srv color use <name>`
// / `srv color off`. MCP runs are NOT affected -- the model wants
// plain text, not ANSI escapes.
type SessionRecord struct {
	Profile     *string           `json:"profile"`
	Cwds        map[string]string `json:"cwds"`
	Guard       bool              `json:"guard,omitempty"`
	ColorPreset string            `json:"color_preset,omitempty"`
	Started     string            `json:"started"`
	LastSeen    string            `json:"last_seen"`
}

type sessionsFile struct {
	Version  int                       `json:"_version,omitempty"`
	Sessions map[string]*SessionRecord `json:"sessions"`
}

func loadSessionsFile() *sessionsFile {
	data, err := os.ReadFile(SessionsFile())
	s := &sessionsFile{Sessions: map[string]*SessionRecord{}}
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		fatal("error: %s is not valid JSON: %v", SessionsFile(), err)
	}
	if s.Sessions == nil {
		s.Sessions = map[string]*SessionRecord{}
	}
	warnIfNewerSchema(SessionsFile(), s.Version)
	return s
}

func writeSessionsFile(s *sessionsFile) error {
	s.Version = SchemaVersion
	return writeJSONFile(SessionsFile(), s)
}

// intermediateExes (Windows): exes that are transparent launcher
// layers, NOT the shell the user is typing into. Walking up through
// these yields the actual host shell's pid.
//
// cmd.exe is intentionally NOT in this set. Treating cmd.exe as the
// session boundary makes "every cmd window is its own session", which
// matches user mental model: open two cmd windows under the same
// Windows Terminal, expect two separate `srv color` / `srv guard`
// states. Same for powershell.exe, bash.exe, etc -- whichever shell
// lives at the bottom of the tree owns the session.
//
// python launchers stay in here so that `python -c "subprocess.run([srv,...])"`
// from a long-running python REPL keeps the SAME session across calls
// (parent = python = walked through; grandparent = the user's shell).
var intermediateExes = map[string]bool{
	"python.exe": true, "py.exe": true,
	"pythonw.exe": true, "python3.exe": true, "python3w.exe": true,
}

// SessionID returns a stable identifier for the calling shell.
//
// Override: $SRV_SESSION wins. Otherwise:
//   - Unix: os.Getppid() -- direct parent shell.
//   - Windows: walk up the process tree, skipping launcher wrappers
//     (python.exe et al.), return the first non-wrapper ancestor's
//     pid. This is normally the cmd.exe / powershell.exe / bash.exe
//     window the user is typing into, so each window has its own
//     session.
func SessionID() string {
	if v := os.Getenv("SRV_SESSION"); v != "" {
		return strings.TrimSpace(v)
	}
	return platformSessionID()
}

// TouchSession ensures the current session record exists, bumps last_seen,
// and persists. Caller can mutate the returned *SessionRecord and call
// saveSessionsWith.
func TouchSession() (string, *SessionRecord) {
	sid := SessionID()
	s := loadSessionsFile()
	rec, ok := s.Sessions[sid]
	now := time.Now().Format("2006-01-02T15:04:05")
	if !ok {
		rec = &SessionRecord{
			Cwds:     map[string]string{},
			Started:  now,
			LastSeen: now,
		}
		s.Sessions[sid] = rec
	} else {
		if rec.Cwds == nil {
			rec.Cwds = map[string]string{}
		}
		rec.LastSeen = now
	}
	_ = writeSessionsFile(s)
	return sid, rec
}

// saveSessionsWith reloads from disk, replaces the rec for sid, and writes.
// This avoids losing concurrent updates from other srv invocations between
// TouchSession() and the save.
func saveSessionsWith(sid string, rec *SessionRecord) error {
	s := loadSessionsFile()
	s.Sessions[sid] = rec
	return writeSessionsFile(s)
}

// GuardOn reports whether the high-risk-op confirmation guard is
// active for the calling session.
//
// Precedence (high to low):
//  1. SRV_GUARD env: "1"/"true"/"on"/"yes" -> on; "0"/"false"/"off"/"no" -> off.
//     Use this in MCP server registrations so the guard travels with the
//     subprocess regardless of which shell session id it inherits.
//  2. SessionRecord.Guard, set via `srv guard on|off`.
//  3. Default: off.
func GuardOn() bool {
	if v := os.Getenv("SRV_GUARD"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			return true
		case "0", "false", "off", "no":
			return false
		}
	}
	sid := SessionID()
	s := loadSessionsFile()
	rec, ok := s.Sessions[sid]
	return ok && rec.Guard
}

// SetGuard toggles the calling session's guard flag and persists.
// Returns the resolved session id so callers can echo it back to users
// (the `srv guard on` command does this so the user can see *which*
// session it bound to -- handy when SRV_SESSION is set).
func SetGuard(on bool) (string, error) {
	sid, rec := TouchSession()
	rec.Guard = on
	if err := saveSessionsWith(sid, rec); err != nil {
		return sid, err
	}
	return sid, nil
}

// GetColorPreset returns the active colour preset name for the
// calling session, or "" if none. Used by cmdRun to decide which
// shell snippet (if any) to inline before the user's command.
func GetColorPreset() string {
	sid := SessionID()
	s := loadSessionsFile()
	rec, ok := s.Sessions[sid]
	if !ok {
		return ""
	}
	return rec.ColorPreset
}

// SetColorPreset persists the chosen preset name (or "" to clear)
// to the calling session. Returns the resolved session id so the CLI
// can echo it back.
func SetColorPreset(name string) (string, error) {
	sid, rec := TouchSession()
	rec.ColorPreset = name
	if err := saveSessionsWith(sid, rec); err != nil {
		return sid, err
	}
	return sid, nil
}

// ColorPresetsDir returns ~/.srv/init/, the directory the user drops
// custom shell snippets into. Each *.sh file is one preset; the
// filename without extension is what `srv color use <name>` accepts.
func ColorPresetsDir() string {
	return filepath.Join(ConfigDir(), "init")
}

// ColorPresetPath resolves a preset name to its absolute file path
// under ColorPresetsDir(). Doesn't check existence; callers test
// with os.Stat so missing files surface a clear error.
func ColorPresetPath(name string) string {
	return filepath.Join(ColorPresetsDir(), name+".sh")
}

// ListColorPresets enumerates the *.sh files in ColorPresetsDir(),
// returning their names without the extension. Returns nil + nil
// when the dir doesn't exist (treated as "no presets configured",
// not an error -- the directory is created on demand by the user).
func ListColorPresets() ([]string, error) {
	dir := ColorPresetsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sh") {
			out = append(out, strings.TrimSuffix(name, ".sh"))
		}
	}
	return out, nil
}

// PidAlive returns true if a process with the given pid (as string) exists.
func PidAlive(pidStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	return platformPidAlive(pid)
}

// nowISO returns an ISO-8601 truncated-to-seconds timestamp.
func nowISO() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

// genJobID mirrors the Python format: YYYYMMDD-HHMMSS-xxxx where xxxx is 4
// random hex chars.
func genJobID() string {
	t := time.Now().Format("20060102-150405")
	return fmt.Sprintf("%s-%s", t, randHex4())
}
