//go:build linux

package server

import (
	"os"
	"strconv"
	"strings"
)

func readOperationsHostCounters() operationsHostCounters {
	out := operationsHostCounters{Supported: true}
	if raw, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(raw), "\n")
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[0] == "cpu" {
			out.CPUAvailable = true
			// Linux's guest fields are already included in user/nice. Sum only
			// user, nice, system, idle, iowait, irq, softirq and steal.
			for i := 1; i < len(fields) && i <= 8; i++ {
				value, err := strconv.ParseUint(fields[i], 10, 64)
				if err != nil {
					out.CPUAvailable = false
					break
				}
				out.Total += value
				if i == 4 || i == 5 {
					out.Idle += value
				}
			}
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[2] != "kB" {
				continue
			}
			if fields[0] != "MemTotal:" && fields[0] != "MemAvailable:" {
				continue
			}
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			value *= 1024
			if fields[0] == "MemTotal:" {
				out.MemoryTotal = &value
			} else {
				out.MemoryAvailable = &value
			}
		}
	}
	return out
}
