//go:build linux

package platform

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// linuxStats samples /proc/loadavg + /proc/meminfo for the dashboard's
// STATS panel. Single-digit-millisecond cost, no subprocess. Stable
// across every Linux distro since /proc has been mandatory since
// Linux 2.0; format hasn't changed in any way that breaks field
// indexing.
//
// Notes on Linux compatibility flavours:
//   - Alpine / busybox: /proc is present, fields match.
//   - Older kernels (pre-3.14) without MemAvailable: fallback to
//     MemFree + Buffers + Cached, which approximates the same
//     "memory you can reclaim without swapping" notion.
//   - Containers: as long as /proc is mounted (default for
//     docker / podman / kubernetes), this reads the host's view
//     unless cgroup-aware /proc is set up. That's the caller's
//     choice; this code reports whatever /proc serves.
type linuxStats struct{}

func (linuxStats) Sample() Sample {
	cpu, err := readLoadAvg1()
	if err != nil {
		return Sample{Err: err.Error()}
	}
	mem, err := readMemPercent()
	if err != nil {
		return Sample{CPULoad: cpu, Err: err.Error()}
	}
	return Sample{CPULoad: cpu, MemPercent: mem}
}

func readLoadAvg1() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, fmt.Errorf("loadavg: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("loadavg: empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("loadavg parse: %v", err)
	}
	return v, nil
}

func readMemPercent() (float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("meminfo: %v", err)
	}
	defer f.Close()
	var total, avail, free, buffers, cached int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseKB(line)
		case strings.HasPrefix(line, "MemFree:"):
			free = parseKB(line)
		case strings.HasPrefix(line, "Buffers:"):
			buffers = parseKB(line)
		case strings.HasPrefix(line, "Cached:"):
			cached = parseKB(line)
		}
	}
	if total <= 0 {
		return 0, fmt.Errorf("meminfo total missing")
	}
	if avail == 0 {
		// MemAvailable was added in Linux 3.14; fall back for older
		// kernels (RHEL 6 etc.) by approximating "reclaimable" as
		// free + buffers + cached.
		avail = free + buffers + cached
	}
	used := float64(total-avail) / float64(total) * 100
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return used, nil
}

func parseKB(line string) int64 {
	// "MemTotal:       16330092 kB"
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(parts[1], 10, 64)
	return n
}
