// Package mcplog owns the MCP server's diagnostic log at ~/.srv/mcp.log
// and the helpers that read it back. The stdio MCP path swallows
// stdout/stderr from Claude Code's perspective, so this is the only
// post-mortem signal users have when an MCP session dies. Format is
// human-readable RFC3339 + bracketed pid + event:
//
//	2026-05-10T15:30:00+08:00 [1234] start v=2.6.6
//	2026-05-10T15:30:01+08:00 [1234] tool=run dur=0.5s ok
//	2026-05-10T15:30:02+08:00 [1234] tool=push dur=12.3s ok
//	2026-05-10T15:32:15+08:00 [1234] exit reason=stdin-eof
//
// The pid lets users tell concurrent srv mcp instances apart (multiple
// Claude conversations on the same machine). The file is trimmed to
// its last ~256 KB on open if it grew past 1 MB so the log never
// balloons unbounded.
package mcplog

import (
	"fmt"
	"os"
	"sync"
	"time"

	"srv/internal/srvpath"
)

const (
	maxBytes  = 1 << 20 // 1 MB
	keepBytes = 256 << 10
)

var (
	logOnce sync.Once
	logFile *os.File
	logMu   sync.Mutex
	logPid  = os.Getpid()
)

// Path returns the absolute path of the active mcp.log.
func Path() string { return srvpath.MCPLog() }

// open lazily opens the log file on first write. Failure is silent:
// logging must never disrupt the MCP request loop.
func open() *os.File {
	logOnce.Do(func() {
		_ = os.MkdirAll(srvpath.Dir(), 0o755)
		path := Path()
		// Trim oversize log: read tail, rewrite. Cheap on disk because
		// 256 KB is small; matters only when the file is e.g. 100 MB.
		if st, err := os.Stat(path); err == nil && st.Size() > maxBytes {
			if data, err := os.ReadFile(path); err == nil && int64(len(data)) > keepBytes {
				_ = os.WriteFile(path, data[int64(len(data))-keepBytes:], 0o600)
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		logFile = f
	})
	return logFile
}

// Logf writes one timestamped line. Safe under concurrent calls and
// failure-silent. The trailing newline is added; callers should not
// include one in format.
func Logf(format string, args ...any) {
	f := open()
	if f == nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	stamp := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("%s [%d] %s\n", stamp, logPid, fmt.Sprintf(format, args...))
	_, _ = f.WriteString(line)
}
