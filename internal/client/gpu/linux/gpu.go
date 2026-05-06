// Package linux provides Linux-specific GPU utilization probing.
package linux

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

// Prober reads GPU utilization on Linux via vendor-specific tools.
// Supports: NVIDIA (nvidia-smi), AMD (rocm-smi), Intel (intel_gpu_top), Moore Threads (mthreads-gmi).
type Prober struct {
	supported     bool
	reason        string
	driverName    string
	driverVersion string
}

func New() *Prober {
	p := &Prober{}
	_, nvidiaErr := exec.LookPath("nvidia-smi")
	_, rocmErr := exec.LookPath("rocm-smi")
	_, intelErr := exec.LookPath("intel_gpu_top")
	_, musaErr := exec.LookPath("mthreads-gmi")

	if nvidiaErr != nil && rocmErr != nil && intelErr != nil && musaErr != nil {
		// Check if Vulkan is present (GPU exists but no monitoring tool)
		_, vulkanErr := exec.LookPath("vulkaninfo")
		if vulkanErr == nil {
			p.supported = false
			p.reason = "GPU detected (Vulkan available) but no monitoring tool found — install nvidia-smi, rocm-smi, or intel_gpu_top for contention detection"
		} else {
			p.supported = false
			p.reason = "no GPU monitoring tool found — install nvidia-smi (NVIDIA), rocm-smi (AMD), intel_gpu_top (Intel), or mthreads-gmi (Moore Threads)"
		}
	} else {
		p.supported = true
	}

	// Detect driver name and version
	p.driverName, p.driverVersion = detectDriver()

	return p
}

// Supported returns whether GPU monitoring is available on this Linux system.
func (p *Prober) Supported() (bool, string) {
	return p.supported, p.reason
}

// Utilization returns the current GPU utilization percentage (0-100) on Linux.
func (p *Prober) Utilization() (int, error) {
	// Try NVIDIA first (most common for LLM inference)
	if pct, err := nvidiaSMI(); err == nil {
		return pct, nil
	}

	// Try AMD ROCm
	if pct, err := rocmSMI(); err == nil {
		return pct, nil
	}

	// Try Intel
	if pct, err := intelGPUTop(); err == nil {
		return pct, nil
	}

	// Try Moore Threads (MUSA)
	if pct, err := mthreadsGMI(); err == nil {
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

// rocmSMI queries GPU utilization via rocm-smi (AMD).
func rocmSMI() (int, error) {
	out, err := exec.Command("rocm-smi", "--showuse", "--json").Output()
	if err != nil {
		return 0, err
	}

	// Parse "GPU use (%)" from rocm-smi output
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "GPU use") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				numStr := strings.TrimSpace(parts[1])
				numStr = strings.Trim(numStr, "\",% ")
				val, err := strconv.Atoi(numStr)
				if err == nil {
					return val, nil
				}
			}
		}
	}

	return 0, nil
}

// intelGPUTop queries GPU utilization via intel_gpu_top (Intel).
// Runs a single 1-second sample in JSON mode.
func intelGPUTop() (int, error) {
	out, err := exec.Command("intel_gpu_top", "-J", "-s", "500", "-l", "1").Output()
	if err != nil {
		return 0, err
	}

	// intel_gpu_top -J outputs JSON with engine utilization; parse the Render/3D engine busy percentage
	var data struct {
		Engines map[string]struct {
			Busy float64 `json:"busy"`
		} `json:"engines"`
	}

	// Output may have multiple JSON objects (one per sample); take the last complete one
	lines := strings.TrimSpace(string(out))
	// Find the last '{' ... '}' block
	lastBrace := strings.LastIndex(lines, "}")
	if lastBrace < 0 {
		return 0, exec.ErrNotFound
	}
	firstBrace := strings.LastIndex(lines[:lastBrace], "{")
	if firstBrace < 0 {
		return 0, exec.ErrNotFound
	}

	if err := json.Unmarshal([]byte(lines[firstBrace:lastBrace+1]), &data); err != nil {
		return 0, err
	}

	// Find highest busy percentage across all engines
	var maxBusy float64
	for _, engine := range data.Engines {
		if engine.Busy > maxBusy {
			maxBusy = engine.Busy
		}
	}

	return int(maxBusy), nil
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

// DriverInfo returns the GPU driver name and version detected on this system.
func (p *Prober) DriverInfo() (string, string) {
	return p.driverName, p.driverVersion
}

// detectDriver determines the GPU driver name and version by probing available tools.
func detectDriver() (name, version string) {
	// NVIDIA: nvidia-smi reports driver version directly
	if out, err := exec.Command("nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader,nounits").Output(); err == nil {
		ver := strings.TrimSpace(string(out))
		if ver != "" {
			return "nvidia", strings.Split(ver, "\n")[0]
		}
	}

	// AMD: rocm-smi --showdriverversion
	if out, err := exec.Command("rocm-smi", "--showdriverversion").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "Driver version") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return "amdgpu", strings.TrimSpace(parts[1])
				}
			}
		}
		// Fallback: if rocm-smi exists, report amdgpu without version
		return "amdgpu", ""
	}

	// Intel: modinfo i915 provides driver version
	if out, err := exec.Command("modinfo", "-F", "version", "i915").Output(); err == nil {
		ver := strings.TrimSpace(string(out))
		if ver != "" {
			return "i915", ver
		}
		// i915 module exists but no version field — report kernel version
		if kver, kerr := exec.Command("uname", "-r").Output(); kerr == nil {
			return "i915", strings.TrimSpace(string(kver))
		}
		return "i915", ""
	}

	// Moore Threads: mthreads-gmi
	if out, err := exec.Command("mthreads-gmi", "--query-gpu=driver_version", "--format=csv,noheader,nounits").Output(); err == nil {
		ver := strings.TrimSpace(string(out))
		if ver != "" {
			return "musa", ver
		}
		return "musa", ""
	}

	return "", ""
}
