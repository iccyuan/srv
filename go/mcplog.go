package main

import (
	"fmt"
	"os"
	"srv/internal/srvpath"
	"sync"
	"time"
)

// MCP server diagnostic log. Appended to ~/.srv/mcp.log so users can
// look up *why* their MCP session disconnected after the fact -- the
// stdio MCP path discards stdout/stderr from Claude Code's perspective
// so without a side channel the only signal is "tools no longer
// available". Each line is human-readable RFC3339 + PID + event:
//
//	2026-05-10T15:30:00+08:00 [1234] start v=2.6.6
//	2026-05-10T15:30:01+08:00 [1234] tool=run dur=0.5s ok
//	2026-05-10T15:30:02+08:00 [1234] tool=push dur=12.3s ok
//	2026-05-10T15:32:15+08:00 [1234] exit reason=stdin-eof
//	2026-05-10T15:33:00+08:00 [1235] panic at tar producer: <message>
//
// PID lets users tell concurrent srv mcp instances apart (multiple
// Claude conversations / MCP clients on the same machine). The file is
// trimmed to its last ~256 KB on open if it grew past 1 MB so the log
// never balloons unbounded.

const (
	mcpLogMaxBytes  = 1 << 20 // 1 MB
	mcpLogKeepBytes = 256 << 10
)

var (
	mcpLogOnce sync.Once
	mcpLogFile *os.File
	mcpLogMu   sync.Mutex
	mcpLogPid  = os.Getpid()
)

func mcpLogPath() string { return srvpath.MCPLog() }

// mcpLogOpen lazily opens the log file on first write. Failure is
// silent: logging must never disrupt the MCP request loop.
func mcpLogOpen() *os.File {
	mcpLogOnce.Do(func() {
		_ = os.MkdirAll(srvpath.Dir(), 0o755)
		path := mcpLogPath()
		// Trim oversize log: read tail, rewrite. Cheap on disk because
		// 256 KB is small; matters only when the file is e.g. 100 MB.
		if st, err := os.Stat(path); err == nil && st.Size() > mcpLogMaxBytes {
			if data, err := os.ReadFile(path); err == nil && int64(len(data)) > mcpLogKeepBytes {
				_ = os.WriteFile(path, data[int64(len(data))-mcpLogKeepBytes:], 0o600)
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		mcpLogFile = f
	})
	return mcpLogFile
}

// mcpLogf writes one timestamped line. Safe under concurrent calls and
// failure-silent. The trailing newline is added; callers should not
// include one in format.
func mcpLogf(format string, args ...any) {
	f := mcpLogOpen()
	if f == nil {
		return
	}
	mcpLogMu.Lock()
	defer mcpLogMu.Unlock()
	stamp := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("%s [%d] %s\n", stamp, mcpLogPid, fmt.Sprintf(format, args...))
	_, _ = f.WriteString(line)
}
