// Package windows provides Windows-specific GPU utilization probing.
package windows

import (
	"os/exec"
	"strconv"
	"strings"
)

// Prober reads GPU utilization on Windows.
// Supports: NVIDIA (nvidia-smi), Intel/AMD/any WDDM 2.0+ GPU (typeperf), Moore Threads (mthreads-gmi).
type Prober struct {
	supported     bool
	reason        string
	hasNvidia     bool
	hasMusa       bool
	driverName    string
	driverVersion string
}

func New() *Prober {
	p := &Prober{}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		p.hasNvidia = true
		p.supported = true
	}
	if _, err := exec.LookPath("mthreads-gmi"); err == nil {
		p.hasMusa = true
		p.supported = true
	}

	// Windows 10+ has built-in GPU performance counters via typeperf (works for all WDDM 2.0+ GPUs)
	if !p.supported {
		if gpuCountersAvailable() {
			p.supported = true
		}
	}

	if !p.supported {
		_, vulkanErr := exec.LookPath("vulkaninfo")
		if vulkanErr == nil {
			p.reason = "GPU detected (Vulkan available) but no monitoring tool found — install NVIDIA drivers or ensure Windows GPU performance counters are accessible"
		} else {
			p.reason = "no GPU monitoring tool found — install nvidia-smi (NVIDIA), mthreads-gmi (Moore Threads), or ensure Windows GPU performance counters are accessible"
		}
	}

	// Detect driver name and version
	p.driverName, p.driverVersion = detectDriver(p.hasNvidia, p.hasMusa)

	return p
}

// gpuCountersAvailable checks if Windows GPU performance counters exist.
func gpuCountersAvailable() bool {
	// Query if any GPU Engine counters exist (available on Windows 10 1709+ for all WDDM 2.0+ GPUs)
	out, err := exec.Command("typeperf", "-qx", "GPU Engine").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "GPU Engine")
}

// Supported returns whether GPU monitoring is available and a reason if not.
func (p *Prober) Supported() (bool, string) {
	return p.supported, p.reason
}

// Utilization returns the current GPU utilization percentage (0-100) on Windows.
func (p *Prober) Utilization() (int, error) {
	if !p.supported {
		return 0, nil
	}

	// Try NVIDIA first (most accurate)
	if p.hasNvidia {
		if pct, err := nvidiaSMI(); err == nil {
			return pct, nil
		}
	}

	// Try Moore Threads
	if p.hasMusa {
		if pct, err := mthreadsGMI(); err == nil {
			return pct, nil
		}
	}

	// Fallback: Windows GPU performance counters (works for Intel, AMD, NVIDIA — any WDDM 2.0+ GPU)
	if pct, err := windowsGPUCounters(); err == nil {
		return pct, nil
	}

	return 0, nil
}

// nvidiaSMI queries GPU utilization via nvidia-smi.
func nvidiaSMI() (int, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}

	// nvidia-smi may return multiple GPUs; take the max
	var maxUtil int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		val, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		if val > maxUtil {
			maxUtil = val
		}
	}
	return maxUtil, nil
}

// mthreadsGMI queries GPU utilization via mthreads-gmi (Moore Threads MUSA).
func mthreadsGMI() (int, error) {
	out, err := exec.Command("mthreads-gmi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}

	val, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return val, nil
}

// windowsGPUCounters reads GPU utilization via Windows Performance Counters (typeperf).
// Works for any WDDM 2.0+ GPU (Intel, AMD, NVIDIA) on Windows 10 1709+.
func windowsGPUCounters() (int, error) {
	// typeperf with -sc 1 takes a single sample
	out, err := exec.Command("typeperf", `\GPU Engine(*engtype_3D)\Utilization Percentage`, "-sc", "1").Output()
	if err != nil {
		return 0, err
	}

	// typeperf output format:
	// "(PDH-CSV 4.0)","\\MACHINE\GPU Engine(...)\Utilization Percentage"
	// "05/06/2026 16:00:00.000","12.345678"
	// The second line has the values (comma-separated for multiple counters)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, exec.ErrNotFound
	}

	// Parse the data line (last non-empty line before the summary)
	var maxUtil float64
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "\"") && !strings.Contains(line, "Exiting") {
			fields := strings.Split(line, ",")
			for _, field := range fields[1:] { // skip timestamp
				field = strings.Trim(field, "\" \r")
				if field == "" || field == " " {
					continue
				}
				val, err := strconv.ParseFloat(field, 64)
				if err == nil && val > maxUtil {
					maxUtil = val
				}
			}
		}
	}

	return int(maxUtil), nil
}

// DriverInfo returns the GPU driver name and version detected on this system.
func (p *Prober) DriverInfo() (string, string) {
	return p.driverName, p.driverVersion
}

// detectDriver determines the GPU driver name and version on Windows.
func detectDriver(hasNvidia, hasMusa bool) (name, version string) {
	// NVIDIA: nvidia-smi reports driver version directly
	if hasNvidia {
		if out, err := exec.Command("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader,nounits").Output(); err == nil {
			ver := strings.TrimSpace(string(out))
			if ver != "" {
				return "nvidia", strings.Split(ver, "\n")[0]
			}
		}
	}

	// Moore Threads: mthreads-gmi
	if hasMusa {
		if out, err := exec.Command("mthreads-gmi", "--query-gpu=driver_version", "--format=csv,noheader,nounits").Output(); err == nil {
			ver := strings.TrimSpace(string(out))
			if ver != "" {
				return "musa", ver
			}
			return "musa", ""
		}
	}

	// Fallback: query WDDM driver via PowerShell (works for Intel, AMD, and any WDDM GPU)
	if out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`Get-CimInstance Win32_VideoController | Select-Object -First 1 -ExpandProperty DriverVersion`).Output(); err == nil {
		ver := strings.TrimSpace(string(out))
		if ver != "" {
			// Determine vendor from adapter name
			driverName := detectWDDMDriverName()
			return driverName, ver
		}
	}

	return "", ""
}

// detectWDDMDriverName queries the Windows display adapter name to determine the GPU vendor.
func detectWDDMDriverName() string {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`Get-CimInstance Win32_VideoController | Select-Object -First 1 -ExpandProperty Name`).Output()
	if err != nil {
		return "wddm"
	}
	adapterName := strings.ToLower(strings.TrimSpace(string(out)))
	switch {
	case strings.Contains(adapterName, "intel"):
		return "intel"
	case strings.Contains(adapterName, "amd") || strings.Contains(adapterName, "radeon"):
		return "amd"
	case strings.Contains(adapterName, "nvidia") || strings.Contains(adapterName, "geforce"):
		return "nvidia"
	default:
		return "wddm"
	}
}
