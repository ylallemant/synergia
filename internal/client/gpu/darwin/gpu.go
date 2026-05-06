// Package darwin provides macOS-specific GPU utilization probing.
package darwin

import (
	"os/exec"
	"strconv"
	"strings"
)

// Prober reads GPU utilization on macOS via ioreg and process detection.
type Prober struct{}

func New() *Prober {
	return &Prober{}
}

// Supported returns true on macOS — GPU monitoring is always available via ioreg.
func (p *Prober) Supported() (bool, string) {
	return true, ""
}

// Utilization returns the current GPU utilization percentage (0-100) on macOS.
func (p *Prober) Utilization() (int, error) {
	// Query IOAccelerator for GPU performance statistics (Apple Silicon + Intel Macs)
	out, err := exec.Command("ioreg", "-r", "-c", "IOAccelerator", "-d", "1").Output()
	if err != nil {
		return 0, err
	}

	if pct := parseIORegUtilization(string(out)); pct > 0 {
		return pct, nil
	}

	// Fallback: detect known GPU-heavy processes
	return detectGPUProcesses(), nil
}

// parseIORegUtilization extracts GPU utilization from ioreg output.
func parseIORegUtilization(output string) int {
	keys := []string{
		"\"GPU Activity(%)\"",
		"\"Device Utilization %\"",
		"\"GPU Core Utilization(%)\"",
	}

	for _, line := range strings.Split(output, "\n") {
		for _, key := range keys {
			if strings.Contains(line, key) {
				return extractPercentage(line)
			}
		}
	}

	return 0
}

// extractPercentage pulls a numeric value from an ioreg line like:
//
//	"GPU Activity(%)" = 75
func extractPercentage(line string) int {
	parts := strings.Split(line, "=")
	if len(parts) < 2 {
		return 0
	}
	numStr := strings.TrimSpace(parts[len(parts)-1])
	numStr = strings.Trim(numStr, "\"")
	val, err := strconv.Atoi(numStr)
	if err != nil {
		fval, ferr := strconv.ParseFloat(numStr, 64)
		if ferr != nil {
			return 0
		}
		return int(fval)
	}
	return val
}

// detectGPUProcesses checks for known GPU-intensive applications on macOS.
func detectGPUProcesses() int {
	gpuProcesses := []string{
		"MTLCompilerServi", // Metal shader compilation (games)
		"Steam",
		"steamwebhelper",
		"Blender",
		"DaVinci Resolve",
		"Final Cut Pro",
		"Compressor",
		"Unity",
		"Unreal",
	}

	out, err := exec.Command("ps", "-eo", "comm").Output()
	if err != nil {
		return 0
	}

	processes := string(out)
	for _, proc := range gpuProcesses {
		if strings.Contains(processes, proc) {
			return 80 // Assume high utilization when a known GPU process is running
		}
	}

	return 0
}

// DriverInfo returns "metal" and the macOS Metal/GPU driver version.
func (p *Prober) DriverInfo() (string, string) {
	// Use system_profiler to get the Metal support version
	out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output()
	if err != nil {
		return "metal", ""
	}

	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Metal Support:") || strings.HasPrefix(trimmed, "Metal Family:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				return "metal", strings.TrimSpace(parts[1])
			}
		}
	}

	// Fallback: report macOS version as the driver version
	if ver, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		return "metal", strings.TrimSpace(string(ver))
	}

	return "metal", ""
}
