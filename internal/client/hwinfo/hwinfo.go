// Package hwinfo collects hardware and OS information for the worker.
package hwinfo

import (
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

	info.OSVer = detectOSVersion()
	info.CPU = detectCPU()
	info.GPU = detectGPU()
	info.VRAMMB = detectVRAM()
	info.RAMMB = detectRAM()

	return info
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
	}
	return 0
}
