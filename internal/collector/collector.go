// Package collector reads processes and listening sockets from the OS.
// One implementation per OS is compiled in via build tags (collector_<goos>.go);
// this file holds only the platform-independent types.
package collector

import (
	"context"
	"net/netip"
	"time"
)

// Process is one process as read from the OS.
type Process struct {
	PID       int           `json:"pid"`
	PPID      int           `json:"ppid"`
	UID       int           `json:"uid"`
	StartTime time.Time     `json:"start_time"`
	Name      string        `json:"name"` // comm / p_comm
	Argv      []string      `json:"argv"`
	Cwd       string        `json:"cwd"`      // "" when unreadable
	CPUTime   time.Duration `json:"cpu_time"` // cumulative user+system
	RSSBytes  uint64        `json:"rss_bytes"`
	Unknown   []string      `json:"unknown"` // names of fields that could not be read: "argv", "cwd", "cpu", "mem"
}

// Listener is one listening TCP socket.
type Listener struct {
	Proto string     `json:"proto"` // "tcp4" | "tcp6"
	Addr  netip.Addr `json:"addr"`  // bind address
	Port  uint16     `json:"port"`
	PID   int        `json:"pid"` // 0 when the owner is unknown
}

// Result is one raw snapshot.
type Result struct {
	Processes []Process                `json:"processes"`
	Listeners []Listener               `json:"listeners"`
	Warnings  []string                 `json:"warnings"`
	Timings   map[string]time.Duration `json:"timings"` // per source, e.g. "proctable", "argv_cwd", "listeners"
}

// Collector produces a Result for the current machine.
type Collector interface {
	Collect(ctx context.Context) (Result, error)
}
