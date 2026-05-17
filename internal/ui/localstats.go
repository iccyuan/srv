package ui

import (
	"srv/internal/platform"
	"time"
)

// localStats samples the host's current CPU load + memory-used% via
// the platform package's SystemStats interface. The dispatch to the
// right OS implementation (Linux /proc, darwin sysctl, Windows
// PowerShell) happens at the platform layer; this thin adapter just
// stitches the result into the StatsSample shape the UI rings
// consume.
//
// Why so thin: the per-OS sampling code used to live here behind a
// runtime.GOOS switch, but that pattern compiled non-target code
// into every binary AND violated the "extension lives in its own
// file" rule we adopted for the platform package. Migration moved
// the implementations to platform/platform_<goos>_stats.go where
// build tags exclude the unused ones cleanly.
func localStats() StatsSample {
	now := time.Now()
	s := platform.Stats.Sample()
	return StatsSample{
		CPULoad:    s.CPULoad,
		MemPercent: s.MemPercent,
		When:       now,
		Err:        s.Err,
	}
}
