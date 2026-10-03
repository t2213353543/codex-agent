package server

import (
	"sync"
	"time"
)

type operationsHostResources struct {
	Supported            bool     `json:"supported"`
	CPUStatus            string   `json:"cpu_status"`
	CPUPercent           *float64 `json:"cpu_percent"`
	CPUSampleSeconds     *float64 `json:"cpu_sample_seconds"`
	MemoryTotalBytes     *uint64  `json:"memory_total_bytes"`
	MemoryAvailableBytes *uint64  `json:"memory_available_bytes"`
}

type operationsHostCounters struct {
	Supported                    bool
	CPUAvailable                 bool
	Idle, Total                  uint64
	MemoryTotal, MemoryAvailable *uint64
}

type operationsHostSampler struct {
	mu               sync.Mutex
	previous         operationsHostCounters
	at               time.Time
	percent, seconds *float64
}

var operationsHostSamples operationsHostSampler

func (s *operationsHostSampler) snapshot(c operationsHostCounters, now time.Time) operationsHostResources {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := operationsHostResources{Supported: c.Supported, CPUStatus: "unavailable", MemoryTotalBytes: c.MemoryTotal, MemoryAvailableBytes: c.MemoryAvailable}
	if !c.Supported {
		out.CPUStatus = "unsupported"
		return out
	}
	if !c.CPUAvailable {
		return out
	}
	if s.at.IsZero() || c.Total < s.previous.Total || c.Idle < s.previous.Idle {
		s.previous, s.at, s.percent, s.seconds = c, now, nil, nil
	} else if elapsed := now.Sub(s.at).Seconds(); elapsed >= 0.25 && c.Total > s.previous.Total {
		total, idle := c.Total-s.previous.Total, c.Idle-s.previous.Idle
		if idle <= total {
			percent := 100 * float64(total-idle) / float64(total)
			s.percent, s.seconds = &percent, &elapsed
		}
		s.previous, s.at = c, now
	}
	out.CPUPercent, out.CPUSampleSeconds = s.percent, s.seconds
	out.CPUStatus = "warming_up"
	if out.CPUPercent != nil {
		out.CPUStatus = "ok"
	}
	return out
}

func collectOperationsHost() operationsHostResources {
	return operationsHostSamples.snapshot(readOperationsHostCounters(), time.Now())
}
