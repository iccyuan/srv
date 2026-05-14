package ui

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// localStats reads the host's current CPU load + memory-used% via
// platform-native paths. Used by liveSource.Stats so the dashboard's
// STATS panel reflects the machine running `srv ui` itself, not the
// remote profile -- the user explicitly asked for local readings
// after the previous design pulled them from the SSH target.
//
// Implementations per platform:
//
//	linux   → read /proc/loadavg + /proc/meminfo directly. Fast, no
//	          subprocess.
//	darwin  → `sysctl -n vm.loadavg` + `vm_stat`. ~10ms shell-out.
//	windows → PowerShell Get-CimInstance Win32_Processor +
//	          Win32_OperatingSystem. ~500ms shell-out (PowerShell
//	          start cost dominates). Acceptable for a 3s sampler.
//	other   → returns Err = "stats not supported on <GOOS>".
func localStats() StatsSample {
	now := time.Now()
	switch runtime.GOOS {
	case "linux":
		return linuxStats(now)
	case "darwin":
		return darwinStats(now)
	case "windows":
		return windowsStats(now)
	default:
		return StatsSample{When: now, Err: "stats not supported on " + runtime.GOOS}
	}
}

func linuxStats(now time.Time) StatsSample {
	cpu, err := readLoadAvg1()
	if err != nil {
		return StatsSample{When: now, Err: err.Error()}
	}
	mem, err := readMemPercent()
	if err != nil {
		return StatsSample{CPULoad: cpu, When: now, Err: err.Error()}
	}
	return StatsSample{CPULoad: cpu, MemPercent: mem, When: now}
}

// readLoadAvg1 grabs the 1-minute load average from /proc/loadavg.
// One small read, no parsing libraries.
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

// readMemPercent computes used% from /proc/meminfo: (Total-Available)/Total*100.
// MemAvailable was added in Linux 3.14; on older kernels we fall back to
// (Total-Free-Buffers-Cached)/Total which is close enough for a panel
// trend line.
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
		// Fallback for kernels predating MemAvailable.
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

// darwinStats runs vm_stat + sysctl to get load + page-based memory.
// Slower than linuxStats (~5-10ms) because of the subprocess, but
// still well under the 3s sampling cadence.
func darwinStats(now time.Time) StatsSample {
	loadOut, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return StatsSample{When: now, Err: "sysctl: " + err.Error()}
	}
	// Output: "{ 1.23 0.98 0.85 }"
	cpu := 0.0
	for _, f := range strings.Fields(string(loadOut)) {
		if v, perr := strconv.ParseFloat(f, 64); perr == nil {
			cpu = v
			break
		}
	}
	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil {
		return StatsSample{CPULoad: cpu, When: now, Err: "vm_stat: " + err.Error()}
	}
	pageSize := int64(4096)
	var free, active, inactive, wired, compressed int64
	sc := bufio.NewScanner(strings.NewReader(string(vmOut)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Mach Virtual Memory Statistics:"):
			// "(page size of 16384 bytes)" -- pull the number out.
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
		return StatsSample{CPULoad: cpu, When: now, Err: "vm_stat: zero total"}
	}
	return StatsSample{CPULoad: cpu, MemPercent: float64(used) / float64(total) * 100, When: now}
}

// vmPages parses "Pages free:                               12345." into 12345.
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

// windowsStats runs PowerShell to get CPU usage + memory percentage.
// PowerShell start-up dominates the cost (~500ms cold); the dashboard
// absorbs this on a background goroutine. Output format we ask for:
//
//	<cpu_load>
//	<mem_percent>
//
// Win32_Processor LoadPercentage is 0..100 (the percent currently in
// use averaged across cores), which doesn't map exactly to Unix's
// load-avg semantic but is the closest cheap signal available -- we
// divide by 100 to keep it in the same 0..few range the chart's
// auto-axis treats well.
func windowsStats(now time.Time) StatsSample {
	script := `$cpu = (Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average
$os = Get-CimInstance Win32_OperatingSystem
$mem = [math]::Round((($os.TotalVisibleMemorySize - $os.FreePhysicalMemory) / $os.TotalVisibleMemorySize) * 100, 2)
Write-Output ($cpu/100)
Write-Output $mem`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return StatsSample{When: now, Err: "powershell: " + err.Error()}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return StatsSample{When: now, Err: "powershell: short output"}
	}
	cpu, _ := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	mem, _ := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	return StatsSample{CPULoad: cpu, MemPercent: mem, When: now}
}
