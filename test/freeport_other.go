//go:build !windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// findPIDsHoldingPort uses lsof to enumerate processes bound to the given
// TCP port. lsof is universally available on macOS/Linux dev machines.
func findPIDsHoldingPort(port string) []int {
	out, err := exec.Command("lsof", "-ti", ":"+port).Output()
	if err != nil {
		return nil
	}
	var pids []int
	seen := map[int]bool{}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}
