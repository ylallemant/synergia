//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// findPIDsHoldingPort parses `netstat -ano -p tcp` to find every PID with a
// LISTENING socket on the given local port. lsof isn't shipped with Windows,
// so this is the equivalent — same return shape as the Unix version.
//
// netstat output format (one socket per line, after a header block):
//
//	Proto  Local Address          Foreign Address        State           PID
//	TCP    127.0.0.1:7500         0.0.0.0:0              LISTENING       9936
func findPIDsHoldingPort(port string) []int {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil
	}
	suffix := ":" + port
	var pids []int
	seen := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "TCP" {
			continue
		}
		if !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		if fields[3] != "LISTENING" {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}
