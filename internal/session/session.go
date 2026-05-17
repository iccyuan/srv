// Package session owns the per-shell record file (~/.srv/sessions.json).
// One entry per shell, keyed by a stable id derived from the calling
// process tree. State stored per record:
//
//	{ "profile":      <pinned profile or null>,
//	  "cwds":         { profileName: cwd, ... },
//	  "guard":        <bool, optional>,
//	  "color_preset": <string, optional>,
//	  "started":      iso, "last_seen": iso }
//
// Public API: ID, Touch, SaveWith, GuardOn, SetGuard,
// GetColorPreset, SetColorPreset. The file format is read by no other
// package -- callers go through these helpers so the JSON schema can
// evolve in one place.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"srv/internal/srvutil"
)

// Record mirrors one entry in sessions.json.
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
type Record struct {
	Profile *string           `json:"profile"`
	Cwds    map[string]string `json:"cwds"`
	// PrevCwds tracks the immediately-previous cwd per profile so `srv
	// cd -` can swap to it the way shell `cd -` does. Maintained by
	// config.SetCwd; never written to directly outside that helper.
	PrevCwds    map[string]string `json:"prev_cwds,omitempty"`
	Guard       bool              `json:"guard,omitempty"`
	ColorPreset string            `json:"color_preset,omitempty"`
	Started     string            `json:"started"`
	LastSeen    string            `json:"last_seen"`
}

type file struct {
	Version  int                `json:"_version,omitempty"`
	Sessions map[string]*Record `json:"sessions"`
}

func loadFile() *file {
	data, err := os.ReadFile(srvutil.Sessions())
	s := &file{Sessions: map[string]*Record{}}
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		// Malformed sessions.json: surface a warning but return an
		// empty record rather than panicking -- losing the per-shell
		// state shouldn't block the CLI.
		fmt.Fprintf(os.Stderr, "srv: %s is not valid JSON: %v\n", srvutil.Sessions(), err)
		return &file{Sessions: map[string]*Record{}}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]*Record{}
	}
	srvutil.WarnIfNewerSchema(srvutil.Sessions(), s.Version)
	return s
}

func writeFile(s *file) error {
	s.Version = srvutil.SchemaVersion
	return srvutil.WriteJSONFile(srvutil.Sessions(), s)
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

// ID returns a stable identifier for the calling shell.
//
// Override: $SRV_SESSION wins. Otherwise:
//   - Unix: os.Getppid() -- direct parent shell.
//   - Windows: walk up the process tree, skipping launcher wrappers
//     (python.exe et al.), return the first non-wrapper ancestor's
//     pid. This is normally the cmd.exe / powershell.exe / bash.exe
//     window the user is typing into, so each window has its own
//     session.
func ID() string {
	if v := os.Getenv("SRV_SESSION"); v != "" {
		return strings.TrimSpace(v)
	}
	return platformID()
}

// Touch ensures the current session record exists, bumps last_seen,
// and persists. Caller can mutate the returned *Record and call
// SaveWith.
//
// Locks sessions.json for the duration of the read-modify-write so
// two shells doing `srv use` / `srv cd` at the same moment don't
// lose one update. Lock acquisition has a 1 s budget; on timeout
// we fall back to the un-locked path (last-writer-wins) rather
// than refusing the operation -- the user's cwd update is more
// important than perfect concurrency.
func Touch() (string, *Record) {
	sid := ID()
	release, _ := srvutil.FileLock(srvutil.Sessions())
	if release != nil {
		defer release()
	}
	s := loadFile()
	rec, ok := s.Sessions[sid]
	now := time.Now().Format("2006-01-02T15:04:05")
	if !ok {
		rec = &Record{
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
	_ = writeFile(s)
	return sid, rec
}

// SaveWith reloads from disk, replaces the rec for sid, and writes.
// Same locking story as Touch -- the read-modify-write needs a
// mutex to avoid losing concurrent updates from sibling shells.
func SaveWith(sid string, rec *Record) error {
	release, _ := srvutil.FileLock(srvutil.Sessions())
	if release != nil {
		defer release()
	}
	s := loadFile()
	s.Sessions[sid] = rec
	return writeFile(s)
}

// GuardOn reports whether the high-risk-op confirmation guard is
// active for the calling session.
//
// Precedence (high to low):
//  1. SRV_GUARD env: "1"/"true"/"on"/"yes" -> on; "0"/"false"/"off"/"no" -> off.
//     Use this in MCP server registrations so the guard travels with
//     the subprocess regardless of which shell session id it inherits.
//  2. Record.Guard, set via `srv guard on|off`.
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
	sid := ID()
	s := loadFile()
	rec, ok := s.Sessions[sid]
	return ok && rec.Guard
}

// SetGuard toggles the calling session's guard flag and persists.
// Returns the resolved session id so callers can echo it back to
// users (the `srv guard on` command does this so the user can see
// *which* session it bound to -- handy when SRV_SESSION is set).
func SetGuard(on bool) (string, error) {
	sid, rec := Touch()
	rec.Guard = on
	if err := SaveWith(sid, rec); err != nil {
		return sid, err
	}
	return sid, nil
}

// GetColorPreset returns the active colour preset name for the
// calling session, or "" if none. Used by cmdRun to decide which
// shell snippet (if any) to inline before the user's command.
func GetColorPreset() string {
	sid := ID()
	s := loadFile()
	rec, ok := s.Sessions[sid]
	if !ok {
		return ""
	}
	return rec.ColorPreset
}

// SetColorPreset persists the chosen preset name (or "" to clear) to
// the calling session. Returns the resolved session id so the CLI can
// echo it back.
func SetColorPreset(name string) (string, error) {
	sid, rec := Touch()
	rec.ColorPreset = name
	if err := SaveWith(sid, rec); err != nil {
		return sid, err
	}
	return sid, nil
}

// All returns every session record currently on disk, keyed by
// session id. Used by `srv sessions list` / `show` to enumerate.
func All() map[string]*Record {
	return loadFile().Sessions
}

// Clear removes one session by id. Returns true when an entry was
// actually deleted, false when no such id existed.
func Clear(id string) bool {
	s := loadFile()
	if _, ok := s.Sessions[id]; !ok {
		return false
	}
	delete(s.Sessions, id)
	_ = writeFile(s)
	return true
}

// PruneDead removes every session whose id is no longer the pid of a
// running process. Returns (removed, totalBefore). aliveProbe is the
// caller-supplied "is this id still alive" function -- supplied by
// the caller so this package doesn't have to depend on srvutil's
// PidAlive directly (kept the dependency direction one-way).
func PruneDead(aliveProbe func(id string) bool) (removed, totalBefore int) {
	s := loadFile()
	totalBefore = len(s.Sessions)
	for id := range s.Sessions {
		if !aliveProbe(id) {
			delete(s.Sessions, id)
			removed++
		}
	}
	_ = writeFile(s)
	return removed, totalBefore
}
