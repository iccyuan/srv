//go:build darwin

package platform

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
)

// darwinStats shells out to sysctl + vm_stat for CPU load + paged
// memory percentage. Slower than the Linux /proc reads (~5-10ms
// because of two subprocesses), but well under the dashboard's 3s
// sampling cadence.
//
// macOS-specific quirks:
//   - vm_stat reports its page size in the first line ("page size
//     of 16384 bytes" on Apple Silicon, 4096 on Intel); we parse it
//     out so the byte arithmetic works regardless of CPU.
//   - Pages "speculative" exist on modern macOS but we don't count
//     them -- they're free-on-demand pages the kernel treats as
//     reclaimable, so attributing them to "used" would over-report.
//   - sysctl vm.loadavg returns "{ 1.23 0.98 0.85 }" with leading
//     and trailing braces; the parser walks fields and picks the
//     first parseable float.
type darwinStats struct{}

func (darwinStats) Sample() Sample {
	loadOut, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return Sample{Err: "sysctl: " + err.Error()}
	}
	cpu := 0.0
	for _, f := range strings.Fields(string(loadOut)) {
		if v, perr := strconv.ParseFloat(f, 64); perr == nil {
			cpu = v
			break
		}
	}
	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil {
		return Sample{CPULoad: cpu, Err: "vm_stat: " + err.Error()}
	}
	pageSize := int64(4096)
	var free, active, inactive, wired, compressed int64
	sc := bufio.NewScanner(strings.NewReader(string(vmOut)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Mach Virtual Memory Statistics:"):
			if i := strings.Index(line, "size of "); i >= 0 {
				rest := line[i+len("size of "):]
				parts := strings.Fields(rest)
				if len(parts) > 0 {
					if v, perr := strconv.ParseInt(parts[0], 10, 64); perr == nil {
						pageSize = v
					}
				}
			}
		case strings.HasPrefix(line, "Pages free:"):
			free = vmPages(line)
		case strings.HasPrefix(line, "Pages active:"):
			active = vmPages(line)
		case strings.HasPrefix(line, "Pages inactive:"):
			inactive = vmPages(line)
		case strings.HasPrefix(line, "Pages wired down:"):
			wired = vmPages(line)
		case strings.HasPrefix(line, "Pages occupied by compressor:"):
			compressed = vmPages(line)
		}
	}
	used := (active + wired + compressed) * pageSize
	total := (active + inactive + wired + free + compressed) * pageSize
	if total <= 0 {
		return Sample{CPULoad: cpu, Err: "vm_stat: zero total"}
	}
	return Sample{CPULoad: cpu, MemPercent: float64(used) / float64(total) * 100}
}

// vmPages parses "Pages free: 12345." into 12345. vm_stat always
// trails its numbers with a literal period.
func vmPages(line string) int64 {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	last := parts[len(parts)-1]
	last = strings.TrimSuffix(last, ".")
	n, _ := strconv.ParseInt(last, 10, 64)
	return n
}
