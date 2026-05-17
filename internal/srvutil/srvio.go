// Package srvio holds the small primitives srv uses for writing
// JSON state files atomically (config.json / sessions.json /
// jobs.json) and for warning when an on-disk file declares a schema
// version newer than this binary knows about.
//
// Each feature module (config / session / jobs) calls these instead
// of duplicating the marshal / temp-file / rename dance, and stays
// free of cross-package coupling on package main.
package srvutil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaVersion identifies the current on-disk JSON shape across all
// srv state files (config / sessions / jobs). Bumped when a breaking
// field rename / semantic change requires migration. Older srv
// reading a newer file logs via WarnIfNewerSchema and proceeds best-
// effort; newer srv reading an older file (or one without _version)
// treats it as version 0 and silently upgrades on next save.
const SchemaVersion = 1

// WarnIfNewerSchema emits one stderr line when an on-disk file
// declares a schema we don't know about yet. We still try to use it
// (forward compat), but the user should know they may need a srv
// upgrade.
func WarnIfNewerSchema(path string, version int) {
	if version > SchemaVersion {
		fmt.Fprintf(os.Stderr,
			"srv: %s is schema version %d; this srv knows %d. Upgrade srv to be safe.\n",
			path, version, SchemaVersion)
	}
}

// WriteJSONFile marshals v with two-space indent and atomically
// replaces `path`. The temp + rename dance is in WriteFileAtomic so a
// crash mid-write never leaves the user with a half-empty
// state file the next srv invocation can't parse.
func WriteJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o600)
}

// WriteFileAtomic writes data to a sibling temp file, then renames it
// over path. The temp name is unique per (pid, random suffix) so two
// srv instances can both write without colliding. Falls back to a
// remove-then-rename when the OS doesn't allow direct overwrite-via-
// rename (a real concern on Windows).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(
		filepath.Dir(path),
		fmt.Sprintf(".%s.%d.%s.tmp", filepath.Base(path), os.Getpid(), RandHex4()),
	)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return err2
		}
	}
	return nil
}
