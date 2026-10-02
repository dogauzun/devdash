// Package engine owns the refresh loop: it calls the collector on a ticker, builds immutable
// snapshots with model.Build and publishes them. The TUI and the CLI are its thin clients.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strconv"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// Refresh interval bounds (spec "CLI and JSON schema", --tick).
const (
	DefaultTick = 2 * time.Second
	MinTick     = 500 * time.Millisecond
)

const (
	collectTimeout = 1500 * time.Millisecond
	slowTick       = 500 * time.Millisecond // a tick slower than this counts toward backoff
	slowRun        = 3                      // consecutive slow (or fast) ticks that double (or halve) the interval
	maxBackoff     = 10 * time.Second
	manyProcs      = 5000
	manyProcsTick  = 5 * time.Second
)

// errBusy is a tick skipped because an abandoned Collect is still running.
var errBusy = fmt.Errorf("previous collection still running: %w", context.DeadlineExceeded)

// Options configures an Engine.
type Options struct {
	Collector collector.Collector
	Resolver  *model.Resolver // reused across ticks; only the refresh goroutine touches it
	Tick      time.Duration   // refresh interval; 0 means DefaultTick, raised to MinTick
}

// Update is what Run publishes after every tick, good or not.
type Update struct {
	// Snapshot is the latest good snapshot (the zero value, SchemaVersion 0, until one
	// succeeds). It is never mutated, so consumers may keep it without locks; its TakenAt
	// says how old it is.
	Snapshot model.Snapshot
	Err      error         // why this tick failed (wraps context.DeadlineExceeded on a timeout); nil on success
	Missed   int           // consecutive failed or timed-out ticks since Snapshot; the TUI shows "stale" from 2
	Interval time.Duration // current interval, after adaptive backoff
}

// Engine runs the refresh loop. Create it with New, then call Run once.
type Engine struct {
	o       Options
	updates chan Update
	refresh chan struct{}
	users   userCache

	// Owned by the goroutine running Run (or Snapshot).
	prev     model.Snapshot
	inflight chan outcome // non-nil while an abandoned Collect may still be running
	interval time.Duration
	slow     int // consecutive slow ticks
	fast     int // consecutive fast ticks
	missed   int
	procs    int // process count of the last good tick
}

type outcome struct {
	raw model.Raw
	err error
}

// New returns an Engine that has not started.
func New(o Options) *Engine {
	if o.Tick == 0 {
		o.Tick = DefaultTick
	}
	o.Tick = max(o.Tick, MinTick)
	return &Engine{
		o:        o,
		updates:  make(chan Update, 1),
		refresh:  make(chan struct{}, 1),
		users:    userCache{names: map[int]string{}, lookup: lookupUser},
		interval: o.Tick,
	}
}

// Updates returns the channel Run publishes on. It holds at most one value, the latest; an
// unread update is replaced, so a slow consumer never delays collection. It is closed when
// Run returns.
func (e *Engine) Updates() <-chan Update { return e.updates }

// Refresh asks Run for an immediate tick (the r key, or after an action). It never blocks;
// requests made while a tick is running coalesce into one more tick.
func (e *Engine) Refresh() {
	select {
	case e.refresh <- struct{}{}:
	default:
	}
}

// Run ticks immediately, then every interval, until ctx is done; then it closes Updates and
// returns. A Collect stuck past its timeout is abandoned, not waited for.
//
// ponytail: with more than 5000 processes the spec also reads argv only for processes in a
// project or with a listener; that needs a collector option that does not exist yet.
func (e *Engine) Run(ctx context.Context) {
	defer close(e.updates)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.refresh:
		}

		start := time.Now()
		raw, err := e.collect(ctx)
		if ctx.Err() != nil {
			return
		}
		var snap model.Snapshot
		if err == nil {
			snap = e.build(raw)
			e.procs = len(raw.Processes)
		}
		e.adapt(time.Since(start) > slowTick || errors.Is(err, context.DeadlineExceeded))
		if err == nil {
			snap.Warnings = append(snap.Warnings, e.warnings()...)
			e.prev, e.missed = snap, 0
		} else {
			e.missed++
		}

		select { // latest wins: drop an unread update
		case <-e.updates:
		default:
		}
		e.updates <- Update{Snapshot: e.prev, Err: err, Missed: e.missed, Interval: e.interval}
		t.Reset(e.interval)
	}
}

// Snapshot collects and builds one snapshot without starting the loop, for the CLI. CPU
// percent is NaN everywhere, as on any first sample.
func Snapshot(ctx context.Context, o Options) (model.Snapshot, error) {
	e := New(o)
	raw, err := e.collect(ctx)
	if err != nil {
		return model.Snapshot{}, err
	}
	return e.build(raw), nil
}

// collect runs Collect in its own goroutine and stops waiting after collectTimeout, because
// a read can block in the kernel past its context. While an abandoned Collect is still
// running no new one starts; that tick fails with errBusy.
func (e *Engine) collect(ctx context.Context) (model.Raw, error) {
	if e.inflight != nil {
		select {
		case <-e.inflight: // finished late; its sample is stale, drop it
			e.inflight = nil
		default:
			return model.Raw{}, errBusy
		}
	}
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	ch := make(chan outcome, 1) // buffered: an abandoned goroutine can always finish
	go func() {
		raw, err := e.o.Collector.Collect(ctx)
		ch <- outcome{raw, err}
	}()
	select {
	case o := <-ch:
		return o.raw, o.err
	case <-ctx.Done():
		e.inflight = ch
		return model.Raw{}, ctx.Err()
	}
}

// build turns a sample into a snapshot, using the previous good one for CPU percent, and
// fills User on every real process (the PID 0 pseudo-process has no owner to name).
func (e *Engine) build(raw model.Raw) model.Snapshot {
	s := model.Build(raw, e.prev, nil, e.o.Resolver)
	for i := range s.Processes {
		if p := &s.Processes[i]; p.PID != 0 {
			p.User = e.users.name(p.UID)
		}
	}
	return s
}

// adapt applies the backoff rule: slowRun consecutive slow ticks double the interval up to
// maxBackoff, slowRun consecutive fast ones halve it, never below the floor.
func (e *Engine) adapt(slow bool) {
	if slow {
		e.slow, e.fast = e.slow+1, 0
	} else {
		e.slow, e.fast = 0, e.fast+1
	}
	switch {
	case e.slow == slowRun:
		e.slow = 0
		if e.interval < maxBackoff {
			e.interval = min(e.interval*2, maxBackoff)
		}
	case e.fast == slowRun:
		e.fast = 0
		e.interval /= 2
	}
	e.interval = max(e.interval, e.floor())
}

// floor is the configured interval, raised to manyProcsTick on a machine with many processes.
func (e *Engine) floor() time.Duration {
	if e.procs > manyProcs {
		return max(e.o.Tick, manyProcsTick)
	}
	return e.o.Tick
}

// warnings are the engine's own footer hints, in the snapshot's deduplicated form.
func (e *Engine) warnings() []model.Warning {
	var ws []model.Warning
	if e.procs > manyProcs {
		ws = append(ws, model.Warning{Code: "many_processes", Count: e.procs,
			Hint: fmt.Sprintf("over %d processes: refreshing every %v", manyProcs, e.floor())})
	}
	if e.interval > e.floor() {
		ws = append(ws, model.Warning{Code: "refresh_slowed", Count: 1,
			Hint: fmt.Sprintf("collection is slow: refreshing every %v", e.interval)})
	}
	return ws
}

// userCache resolves each uid to a user name once for the life of the engine.
type userCache struct {
	names  map[int]string
	lookup func(uid int) string
}

func (c *userCache) name(uid int) string {
	n, ok := c.names[uid]
	if !ok {
		n = c.lookup(uid)
		c.names[uid] = n
	}
	return n
}

// lookupUser names a uid without cgo: the current user (pure Go falls back to $USER, which
// covers macOS directory-service accounts), then /etc/passwd, then the number itself.
func lookupUser(uid int) string {
	id := strconv.Itoa(uid)
	if u, err := user.Current(); err == nil && u.Uid == id {
		return u.Username
	}
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}
	return id
}
