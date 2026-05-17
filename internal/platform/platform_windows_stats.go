//go:build windows

package platform

import (
	"os/exec"
	"strconv"
	"strings"
)

// windowsStats invokes PowerShell + Get-CimInstance to query
// Win32_Processor.LoadPercentage and Win32_OperatingSystem memory
// totals. PowerShell startup dominates the latency (~500ms cold),
// which the dashboard absorbs on a background goroutine.
//
// LoadPercentage is 0..100 (CPU utilisation, averaged across cores)
// which doesn't map exactly to Unix's load average semantic but is
// the closest cheap signal Windows exposes -- we divide by 100 to
// keep the value in the 0..few range the chart's auto-axis treats
// well, the same conversion Unix paths get for free because load
// average is already in that range.
type windowsStats struct{}

func (windowsStats) Sample() Sample {
	script := `$cpu = (Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average
$os = Get-CimInstance Win32_OperatingSystem
$mem = [math]::Round((($os.TotalVisibleMemorySize - $os.FreePhysicalMemory) / $os.TotalVisibleMemorySize) * 100, 2)
Write-Output ($cpu/100)
Write-Output $mem`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return Sample{Err: "powershell: " + err.Error()}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return Sample{Err: "powershell: short output"}
	}
	cpu, _ := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	mem, _ := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	return Sample{CPULoad: cpu, MemPercent: mem}
}
