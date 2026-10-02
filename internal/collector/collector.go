// Package collector reads processes and listening sockets from the OS.
// One implementation per OS is compiled in via build tags (collector_<goos>.go);
// this file holds only the platform-independent types.
package collector

import (
	"context"
	"os"
	"runtime"

	"github.com/dogauzun/devdash/internal/model"
)

// The raw types are model's, so a Result goes into model.Build without conversion.
type (
	// Result is one raw sample.
	Result = model.Raw
	// Process carries only the collected fields; see model.Raw.
	Process = model.Process
	// Listener is a listening socket with its owner pid, 0 when unknown.
	Listener = model.RawListener
)

// Collector produces a Result for the current machine. Collect may block on a stuck read
// and checks ctx only between reads, so callers run it in a goroutine.
type Collector interface {
	Collect(ctx context.Context) (Result, error)
}

// host describes this machine and the effective uid devdash runs as.
func host() model.Host {
	name, _ := os.Hostname()
	return model.Host{OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: name, UID: os.Geteuid()}
}
