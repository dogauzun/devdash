package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// Docker cadence (spec "Architecture", threading): its own goroutine, its own timeout.
const (
	dockerTick    = 5 * time.Second
	dockerTimeout = time.Second // bounds one Fetch; the Source applies its own 500 ms per request
)

// dockerResult is one finished Fetch.
type dockerResult struct {
	containers []model.Container
	warning    *model.Warning
	took       time.Duration
	done       bool // false until the first Fetch returns
}

// dockerLatest holds the latest dockerResult, written by the Docker goroutine and read by
// every tick.
type dockerLatest struct {
	mu sync.Mutex
	r  dockerResult
}

func (l *dockerLatest) store(r dockerResult) {
	l.mu.Lock()
	l.r = r
	l.mu.Unlock()
}

func (l *dockerLatest) load() dockerResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r
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

// watchDocker fetches at once, then every dockerTick, storing each result, until ctx is done.
// A Fetch slower than dockerTick skips the beats it overlapped instead of queueing them.
func (e *Engine) watchDocker(ctx context.Context) {
	t := time.NewTicker(dockerTick)
	defer t.Stop()
	for {
		e.docker.store(fetch(ctx, e.o.Docker))
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
