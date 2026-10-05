package engine

import (
	"context"
	"slices"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// Docker cadence (spec "Architecture", threading): its own goroutine, its own timeout.
const (
	// DockerTick is the beat Run calls Fetch on; a docker.Source rounds its retry holdoff up
	// to whole beats of it.
	DockerTick    = 5 * time.Second
	dockerTimeout = time.Second // bounds one Fetch; the Source applies its own 500 ms per request
)

// dockerResult is one finished Fetch.
type dockerResult struct {
	containers []model.Container
	warning    *model.Warning
	took       time.Duration
	done       bool // false until the first Fetch returns
}

// latestDocker is the latest dockerResult watchDocker stored, or the zero dockerResult (done
// false) before the first one. A stored result is never modified: watchDocker stores a new one
// each time, and Build does not mutate its containers.
func (e *Engine) latestDocker() dockerResult {
	if r := e.docker.Load(); r != nil {
		return *r
	}
	return dockerResult{}
}

// fetch runs one Fetch bounded by dockerTimeout. It relies on Fetch honouring ctx (the
// Source's net/http requests do); unlike Collect it is not abandoned.
func fetch(ctx context.Context, src ContainerSource) dockerResult {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	start := time.Now()
	cs, w := src.Fetch(ctx)
	return dockerResult{containers: cs, warning: w, took: time.Since(start), done: true}
}

// watchDocker fetches at once, then every DockerTick, storing each result, until ctx is done.
// A Fetch slower than DockerTick skips the beats it overlapped instead of queueing them.
func (e *Engine) watchDocker(ctx context.Context) {
	t := time.NewTicker(DockerTick)
	defer t.Stop()
	for {
		r := fetch(ctx, e.o.Docker)
		e.docker.Store(&r)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// withDocker adds a Fetch's warning to raw, where Build merges it with the collector's. raw's
// Warnings slice belongs to the collector, so it is copied, never appended to in place.
func withDocker(raw model.Raw, d dockerResult) model.Raw {
	if d.warning != nil {
		raw.Warnings = append(slices.Clip(raw.Warnings), *d.warning)
	}
	return raw
}
