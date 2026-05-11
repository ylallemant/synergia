// Package darwin provides macOS-specific GPU utilization probing.
package darwin

import (
	"os/exec"
	"regexp"
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

	return parseIORegUtilization(string(out)), nil
}

// utilPatterns matches "Key"=<integer> pairs that may appear either as
// standalone lines or embedded inside a PerformanceStatistics dictionary
// on a single line. regexp.QuoteMeta escapes the parens in "GPU Activity(%)".
var utilPatterns = func() []*regexp.Regexp {
	keys := []string{
		`"GPU Activity(%)"`,
		`"Device Utilization %"`,
		`"GPU Core Utilization(%)"`,
		`"Renderer Utilization %"`,
	}
	re := make([]*regexp.Regexp, 0, len(keys))
	for _, k := range keys {
		re = append(re, regexp.MustCompile(regexp.QuoteMeta(k)+`=(\d+)`))
	}
	return re
}()

// parseIORegUtilization extracts GPU utilisation from ioreg output.
// ioreg on Apple Silicon bundles all stats into a single-line dictionary:
//
//	"PerformanceStatistics" = {"Device Utilization %"=24,"Tiler Utilization %"=24,...}
//
// The previous line-split + last-field approach failed for that layout.
// Regex finds the value immediately after the key regardless of surrounding text.
func parseIORegUtilization(output string) int {
	for _, re := range utilPatterns {
		if m := re.FindStringSubmatch(output); len(m) >= 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				return v
			}
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
