// Package hwinfo collects hardware and OS information for the worker.
package hwinfo

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Info holds the hardware statistics sent to the cluster manager.
type Info struct {
	OS               string `json:"os"`
	OSVer            string `json:"os_version"`
	GPU              string `json:"gpu"`
	GPUDriver        string `json:"gpu_driver"`
	GPUDriverVersion string `json:"gpu_driver_version"`
	VRAMMB           int    `json:"vram_mb"`
	CPU              string `json:"cpu"`
	CPUCores         int    `json:"cpu_cores"`
	RAMMB            int    `json:"ram_mb"`
}

// Collect gathers hardware information from the current system.
func Collect() Info {
	info := Info{
		OS:       runtime.GOOS,
		CPUCores: runtime.NumCPU(),
	}

	// Windows: a single PowerShell invocation pulls every value at once.
	// Each powershell.exe startup is ~300-500 ms, so spawning five separate
	// processes added several seconds to client boot and raced with the
	// TOFU challenge timing in the integration test.
	if runtime.GOOS == "windows" {
		collectWindows(&info)
		return info
	}

	info.OSVer = detectOSVersion()
	info.CPU = detectCPU()
	info.GPU = detectGPU()
	info.VRAMMB = detectVRAM()
	info.RAMMB = detectRAM()

	return info
}

// collectWindows runs a single PowerShell invocation that emits a small
// JSON object covering every field the dashboard exposes. Compared with
// the five-call version this trades ~2.5 s of process-spawn overhead for
// ~0.5 s. Any field whose CIM query fails is left at zero/empty so the
// surrounding test still surfaces the gap.
//
// VRAM: Win32_VideoController.AdapterRAM is a uint32 capped at ~4 GiB, so
// cards with more than 4 GB (the target audience of this project) would
// be under-reported. The script also reads HardwareInformation.qwMemorySize
// (uint64) from the GPU device-class registry key, which modern WDDM
// drivers populate with the true VRAM size. The matching entry is found
// by DriverDesc == GPU name; falls back to the largest qwMemorySize across
// all GPU-class subkeys for multi-adapter systems. AdapterRAM remains the
// final fallback for legacy drivers.
func collectWindows(info *Info) {
	const script = `
$ErrorActionPreference = 'SilentlyContinue'
$os  = Get-CimInstance Win32_OperatingSystem
$cpu = Get-CimInstance Win32_Processor | Select-Object -First 1
$gpu = Get-CimInstance Win32_VideoController | Select-Object -First 1
$cs  = Get-CimInstance Win32_ComputerSystem

# AdapterRAM is a uint32 capped at ~4 GiB. Read the uint64 qwMemorySize
# from the display-adapter device class for accurate VRAM on big cards.
$vram = [int64]$gpu.AdapterRAM
$classKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}'
$entries = @(Get-ChildItem $classKey -ErrorAction SilentlyContinue |
  Where-Object { $_.PSChildName -match '^\d{4}$' })
$readSize = {
  param($props)
  if ($props.'HardwareInformation.qwMemorySize') {
    return [int64]$props.'HardwareInformation.qwMemorySize'
  }
  if ($props.'HardwareInformation.MemorySize') {
    return [int64]$props.'HardwareInformation.MemorySize'
  }
  return [int64]0
}
$matched = 0
$largest = 0
foreach ($e in $entries) {
  $p = Get-ItemProperty -Path $e.PSPath -ErrorAction SilentlyContinue
  $v = & $readSize $p
  if ($v -gt $largest) { $largest = $v }
  if ($p.DriverDesc -eq $gpu.Name -and $v -gt $matched) { $matched = $v }
}
if ($matched -gt 0)      { $vram = $matched }
elseif ($largest -gt 0)  { $vram = $largest }

[PSCustomObject]@{
  OSVer = $os.Version
  CPU   = $cpu.Name
  GPU   = $gpu.Name
  VRAM  = $vram
  RAM   = [int64]$cs.TotalPhysicalMemory
} | ConvertTo-Json -Compress
`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		// Fall back to per-field detection so we at least try.
		info.OSVer = detectOSVersion()
		info.CPU = detectCPU()
		info.GPU = detectGPU()
		info.VRAMMB = detectVRAM()
		info.RAMMB = detectRAM()
		return
	}
	var raw struct {
		OSVer string `json:"OSVer"`
		CPU   string `json:"CPU"`
		GPU   string `json:"GPU"`
		VRAM  int64  `json:"VRAM"`
		RAM   int64  `json:"RAM"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		info.OSVer = detectOSVersion()
		info.CPU = detectCPU()
		info.GPU = detectGPU()
		info.VRAMMB = detectVRAM()
		info.RAMMB = detectRAM()
		return
	}
	info.OSVer = strings.TrimSpace(raw.OSVer)
	info.CPU = strings.TrimSpace(raw.CPU)
	info.GPU = strings.TrimSpace(raw.GPU)
	if raw.VRAM > 0 {
		info.VRAMMB = int(raw.VRAM / (1024 * 1024))
	}
	if raw.RAM > 0 {
		info.RAMMB = int(raw.RAM / (1024 * 1024))
	}
	// Fallbacks for fields the single-call script didn't fill in.
	if info.OSVer == "" {
		info.OSVer = "unknown"
	}
	if info.CPU == "" {
		info.CPU = fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if info.GPU == "" {
		info.GPU = "unknown"
	}
}

func detectOSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sw_vers", "-productVersion").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		out, err := exec.Command("uname", "-r").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "windows":
		if v := psQuery(`(Get-CimInstance Win32_OperatingSystem).Version`); v != "" {
			return v
		}
	}
	return "unknown"
}

func detectCPU() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		out, err := exec.Command("grep", "-m1", "model name", "/proc/cpuinfo").Output()
		if err == nil {
			parts := strings.SplitN(string(out), ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	case "windows":
		if v := psQuery(`(Get-CimInstance Win32_Processor | Select-Object -First 1).Name`); v != "" {
			return v
		}
	}
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

func detectGPU() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Chipset Model:") || strings.HasPrefix(line, "Chip Model:") {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						return strings.TrimSpace(parts[1])
					}
				}
			}
		}
	case "linux":
		out, err := exec.Command("lspci").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "VGA") || strings.Contains(line, "3D controller") {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) >= 2 {
						return strings.TrimSpace(parts[len(parts)-1])
					}
				}
			}
		}
	case "windows":
		if v := psQuery(`(Get-CimInstance Win32_VideoController | Select-Object -First 1).Name`); v != "" {
			return v
		}
	}
	return "unknown"
}

func detectVRAM() int {
	switch runtime.GOOS {
	case "darwin":
		// On Apple Silicon, GPU shares unified memory — report total memory
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err == nil {
				return int(bytes / (1024 * 1024))
			}
		}
	case "linux":
		out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
		if err == nil {
			val, err := strconv.Atoi(strings.TrimSpace(string(out)))
			if err == nil {
				return val
			}
		}
	case "windows":
		// Win32_VideoController.AdapterRAM is a uint32 capped at 4 GiB — values
		// from cards with more than ~4 GB VRAM are wrong (typically reported as
		// ~4095 MB). For accurate >4GB VRAM we'd need to read the registry key
		// HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968...}\<n>\
		// HardwareInformation.qwMemorySize (uint64). Left as a TODO — most users
		// fall below 4 GB anyway, and an under-reported value is still useful
		// for VRAM-tier eligibility on lower-spec hardware.
		if v := psQuery(`(Get-CimInstance Win32_VideoController | Select-Object -First 1).AdapterRAM`); v != "" {
			if bytes, err := strconv.ParseInt(v, 10, 64); err == nil && bytes > 0 {
				return int(bytes / (1024 * 1024))
			}
		}
	}
	return 0
}

func detectRAM() int {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err == nil {
				return int(bytes / (1024 * 1024))
			}
		}
	case "linux":
		out, err := exec.Command("grep", "MemTotal", "/proc/meminfo").Output()
		if err == nil {
			parts := strings.Fields(string(out))
			if len(parts) >= 2 {
				kb, err := strconv.Atoi(parts[1])
				if err == nil {
					return kb / 1024
				}
			}
		}
	case "windows":
		if v := psQuery(`(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory`); v != "" {
			if bytes, err := strconv.ParseInt(v, 10, 64); err == nil && bytes > 0 {
				return int(bytes / (1024 * 1024))
			}
		}
	}
	return 0
}

// psQuery runs a single PowerShell expression and returns its trimmed stdout.
// Returns "" on failure. Used by Windows hwinfo detection.
func psQuery(expr string) string {
	out, err := exec.Command("powershell", "-NoProfile", "-Command", expr).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
