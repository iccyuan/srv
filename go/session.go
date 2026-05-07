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
//	  "started": iso, "last_seen": iso }
type SessionRecord struct {
	Profile  *string           `json:"profile"`
	Cwds     map[string]string `json:"cwds"`
	Started  string            `json:"started"`
	LastSeen string            `json:"last_seen"`
}

type sessionsFile struct {
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
	return s
}

func writeSessionsFile(s *sessionsFile) error {
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
