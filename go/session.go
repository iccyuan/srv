package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SessionRecord mirrors the Python schema:
//
//	{ "profile": <pinned profile or null>,
//	  "cwds":    { profileName: cwd, ... },
//	  "guard":   <bool, optional>,
//	  "started": iso, "last_seen": iso }
//
// Guard, when true, makes the MCP server refuse high-risk operations
// (destructive `run`/`detach` patterns, `sync` with delete) unless the
// caller passes confirm=true. Default off so existing flows are
// unchanged. Toggled per-shell via `srv guard on|off`, or globally via
// the SRV_GUARD env var (which trumps the session record).
type SessionRecord struct {
	Profile  *string           `json:"profile"`
	Cwds     map[string]string `json:"cwds"`
	Guard    bool              `json:"guard,omitempty"`
	Started  string            `json:"started"`
	LastSeen string            `json:"last_seen"`
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

// _INTERMEDIATE_EXES (Windows): exes that are transparent layers between the
// user's shell and our binary. Walking up through these yields a stable id.
var intermediateExes = map[string]bool{
	"cmd.exe": true, "python.exe": true, "py.exe": true,
	"pythonw.exe": true, "python3.exe": true, "python3w.exe": true,
}

// SessionID returns a stable identifier for the calling shell.
//
// Override: $SRV_SESSION wins. Otherwise:
//   - Unix: os.Getppid().
//   - Windows: walk up the process tree, skipping intermediate exes (cmd.exe
//     shim, python.exe launchers), return the first non-intermediate ancestor
//     pid -- usually the user's powershell.exe / bash.exe.
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
