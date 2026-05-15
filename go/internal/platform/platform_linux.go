//go:build linux

package platform

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linux-specific implementations of the platform interfaces. Stats
// and Notifier live in their respective platform_linux_*.go siblings
// so each "feature" can be reviewed independently; this file owns
// the Process bits that vary from the unix base.

func init() {
	Proc = linuxProcess{}
	Term = unixConsole{}
	Sec = unixCrypto{}
	Stats = linuxStats{}
	Notif = linuxNotifier{}
	Open = xdgOpener{}
	Sh = unixShell{}
}

// linuxProcess extends unixProcessBase with /proc-based start-time
// lookups. Embedding lets us reuse Detach / SignalTerminate /
// PIDAlive from the base unchanged; we only add the one method
// that's Linux-only.
type linuxProcess struct {
	unixProcessBase
}

// PIDStartTime reads /proc/<pid>/stat field 22 (starttime in
// clock ticks since boot) and combines it with the boot time
// derived from /proc/uptime to produce a monotonic identifier for
// the process. The unit is "approximate wall-clock nanoseconds"
// using a probed HZ; the value is used for equality-with-slack
// comparison ONLY (not against real wall-clock), so even when HZ
// detection misses, the record-side and check-side both apply the
// same transformation and the comparison still works.
//
// Linux compatibility:
//   - /proc has been required and field 22 stable since Linux 2.0.
//   - HZ varies by distro: 100 (default), 250 (Debian historically),
//     1000 (some RT / embedded kernels). detectHZ probes our own
//     /proc/self/stat against time.Now() at first call and caches
//     the result, so all three common values are handled correctly.
//   - Containers: as long as /proc is mounted (it almost always is),
//     this works. /proc/uptime in a container shows the host's uptime
//     unless `--proc-hidepid` style flags are set; that's fine for
//     our purposes -- we're computing a same-host monotonic stamp.
func (linuxProcess) PIDStartTime(pid int) (int64, bool) {
	startJiffies, ok := readStarttimeJiffies(pid)
	if !ok {
		return 0, false
	}
	boot, ok := bootTime()
	if !ok {
		return 0, false
	}
	hz := detectHZ()
	startSec := float64(startJiffies) / float64(hz)
	return boot + int64(startSec*1e9), true
}

// readStarttimeJiffies parses /proc/<pid>/stat field 22. Format:
// "<pid> (<comm>) <state> <ppid> ... <starttime> ...". <comm> can
// carry spaces and parens (a process can rename itself to anything),
// so we anchor on the LAST ')' before field-splitting -- fields 1
// and 2 sit before that ')' and don't enter the split.
func readStarttimeJiffies(pid int) (int64, bool) {
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	b, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	close := strings.LastIndexByte(string(b), ')')
	if close < 0 || close+2 >= len(b) {
		return 0, false
	}
	rest := strings.Fields(string(b[close+2:]))
	if len(rest) < 20 {
		return 0, false
	}
	// Field 22 in the original numbering = index 19 in the
	// post-')' split (the split's first item is field 3, "state").
	v, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// bootTime returns the host's boot moment in unix nanoseconds.
// Computed by subtracting /proc/uptime's reported uptime from
// time.Now(). Precision is limited by /proc/uptime's 10ms
// granularity, which is fine for our 2-second comparison slack.
func bootTime() (int64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	uptimeSec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return time.Now().UnixNano() - int64(uptimeSec*1e9), true
}

// detectedHZ caches the once-per-process probe result so repeated
// PIDStartTime calls don't redo the discovery I/O.
var (
	hzOnce sync.Once
	hzVal  int64
)

// detectHZ figures out the kernel's user_HZ (clock ticks per
// second) by reading /proc/self/stat for our own process and
// finding which of {100, 250, 1000} produces a process-start time
// closest to time.Now(). Since this is our OWN process which just
// started, the correct HZ should give a delta within milliseconds;
// the wrong HZ gives a 2.5x or 10x offset that's easy to detect.
//
// Linux kernels not built with one of those three HZ values fall
// through to the default 100, which is also the libcpu_features
// default the Go runtime expects on Linux.
func detectHZ() int64 {
	hzOnce.Do(func() {
		hzVal = probeHZ()
	})
	return hzVal
}

func probeHZ() int64 {
	const fallback = 100
	startJiffies, ok := readStarttimeJiffies(os.Getpid())
	if !ok {
		return fallback
	}
	boot, ok := bootTime()
	if !ok {
		return fallback
	}
	nowNanos := time.Now().UnixNano()
	candidates := []int64{100, 250, 1000}
	bestHZ := int64(fallback)
	bestDiff := int64(1 << 62)
	for _, hz := range candidates {
		startSec := float64(startJiffies) / float64(hz)
		predicted := boot + int64(startSec*1e9)
		diff := nowNanos - predicted
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			bestHZ = hz
		}
	}
	return bestHZ
}
