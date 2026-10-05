// Package engine owns the refresh loop: it calls the collector on a ticker, builds immutable
// snapshots with model.Build and publishes them. The TUI and the CLI are its thin clients.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"slices"
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
	Resolver  *model.Resolver // reused across ticks; used by one goroutine at a time (see inProject)
	Tick      time.Duration   // refresh interval; 0 means DefaultTick, raised to MinTick
	// LookupUser names a uid for Process.User, called once per uid for the engine's life (each
	// Snapshot or SnapshotAfter call is its own engine). nil means the OS lookup: the current
	// user, then the user database, then the number itself. Tests set it so their output does
	// not depend on the accounts of the machine they run on.
	LookupUser func(uid int) string
	Docker     ContainerSource // nil means no Docker: no containers, no "docker" timing
}

// ContainerSource is the Docker input, implemented by docker.Source. Fetch returns the
// containers to use now and a warning when Docker is present but not answering.
type ContainerSource interface {
	Fetch(ctx context.Context) ([]model.Container, *model.Warning)
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
	// Warnings are the engine's own hints for this tick (refresh_slowed, many_processes), in
	// the same deduplicated form as Snapshot.Warnings, which holds only collector and Build
	// warnings. This is the only place engine hints appear; the footer shows both lists.
	Warnings []model.Warning
}

// Engine runs the refresh loop. Create it with New, then call Run once.
type Engine struct {
	o       Options
	updates chan Update
	refresh chan struct{}
	users   userCache
	docker  dockerLatest // the latest Fetch, shared by Run's Docker goroutine and its ticks

	// Owned by the goroutine running Run (or Snapshot).
	prev     model.Snapshot
	base     model.Snapshot // the sample prev's CPU percent was measured from
	inflight chan outcome   // non-nil while an abandoned Collect may still be running
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
	if o.LookupUser == nil {
		o.LookupUser = lookupUser
	}
	return &Engine{
		o:        o,
		updates:  make(chan Update, 1),
		refresh:  make(chan struct{}, 1),
		users:    userCache{names: map[int]string{}, lookup: o.LookupUser},
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
// With Options.Docker, a second goroutine fetches containers at once and every 5 s, each Fetch
// bounded by 1 s; every tick builds with the latest list it stored (none until the first
// Fetch returns) and never waits for a Fetch. Run returns after that goroutine has stopped.
//
// After a good tick with more than 5000 processes, each Collect reads argv only for processes
// in a project or with a listener, plus the few the collector adds by name (long, runtime or
// systemd; collector.Options.InProject, answered by inProject); Snapshot and SnapshotAfter
// always read every argv.
func (e *Engine) Run(ctx context.Context) {
	defer close(e.updates)
	if e.o.Docker != nil {
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); e.watchDocker(ctx) }()
		defer func() { cancel(); <-done }() // before close(e.updates)
	}
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
		var co collector.Options
		if e.procs > manyProcs {
			co.InProject = e.inProject
		}
		raw, err := e.collect(ctx, co)
		if ctx.Err() != nil {
			return
		}
		var snap, base model.Snapshot
		if err == nil {
			// CPU percent spans at least one tick: a Refresh during a tick starts the next one as
			// that tick ends (a kill asks twice), and measured from prev it would show devdash's
			// own collection (DEV-181). So a sample sooner than a tick after prev is measured
			// from prev's base, which is a tick or more older.
			base = e.prev
			if raw.TakenAt.Sub(base.TakenAt) < e.o.Tick {
				base = e.base
			}
			snap = e.build(raw, base, e.docker.load())
			e.procs = len(raw.Processes)
		}
		e.adapt(time.Since(start) > slowTick || errors.Is(err, context.DeadlineExceeded))
		if err == nil {
			e.prev, e.base, e.missed = snap, base, 0
		} else {
			e.missed++
		}

		select { // latest wins: drop an unread update
		case <-e.updates:
		default:
		}
		e.updates <- Update{Snapshot: e.prev, Err: err, Missed: e.missed, Interval: e.interval, Warnings: e.warnings()}
		t.Reset(e.interval)
	}
}

// Snapshot collects and builds one snapshot without starting the loop, for the CLI. CPU
// percent is NaN everywhere, as on any first sample.
func Snapshot(ctx context.Context, o Options) (model.Snapshot, error) {
	return SnapshotAfter(ctx, o, model.Snapshot{})
}

// SnapshotAfter is Snapshot with prev as the previous sample, so CPU percent is a number for
// every process also in prev (`devdash --json` samples twice, 200 ms apart).
//
// With Options.Docker, both make one Fetch, bounded by 1 s and run alongside Collect; its
// duration is Timing["docker"] and its warning joins Warnings.
func SnapshotAfter(ctx context.Context, o Options, prev model.Snapshot) (model.Snapshot, error) {
	e := New(o)
	var docker chan dockerResult
	if o.Docker != nil {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel() // a failed Collect stops the Fetch too
		docker = make(chan dockerResult, 1)
		go func() { docker <- fetch(ctx, o.Docker) }()
	}
	raw, err := e.collect(ctx, collector.Options{}) // one-shot: no count to limit argv by
	if err != nil {
		return model.Snapshot{}, err
	}
	var d dockerResult
	if docker != nil {
		d = <-docker
	}
	return e.build(raw, prev, d), nil
}

// collect runs Collect in its own goroutine and stops waiting after collectTimeout, because
// a read can block in the kernel past its context. While an abandoned Collect is still
// running no new one starts; that tick fails with errBusy.
func (e *Engine) collect(ctx context.Context, o collector.Options) (model.Raw, error) {
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
		raw, err := e.o.Collector.Collect(ctx, o)
		ch <- outcome{raw, err}
	}()
	select {
	case out := <-ch:
		return out.raw, out.err
	case <-ctx.Done():
		e.inflight = ch
		return model.Raw{}, ctx.Err()
	}
}

// inProject is collector.Options.InProject: which of procs (Argv not read yet) Resolve puts in
// a project, by their own or an ancestor's cwd (spec steps 1-5; step 6 needs the argv this
// decides on). It resolves a copy, so procs is not touched. It runs on the Collect goroutine
// and uses Options.Resolver, which the refresh goroutine never uses meanwhile: build runs only
// after collect has received that Collect's result, and no tick builds while an abandoned
// Collect is in flight.
func (e *Engine) inProject(procs []model.Process) []bool {
	in := make([]bool, len(procs))
	if e.o.Resolver == nil {
		return in
	}
	ps := slices.Clone(procs)
	e.o.Resolver.Resolve(ps)
	for i, p := range ps {
		in[i] = p.ProjectID != ""
	}
	return in
}

// build turns a sample and the latest Docker result into a snapshot, measuring CPU percent from
// prev, and fills User on every real process (the PID 0 pseudo-process has no
// owner to name).
func (e *Engine) build(raw model.Raw, prev model.Snapshot, d dockerResult) model.Snapshot {
	s := model.Build(withDocker(raw, d), prev, d.containers, e.o.Resolver)
	if d.done {
		s.Timing["docker"] = d.took
	}
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

// warnings are the engine's own footer hints for Update.Warnings, fresh on every tick.
func (e *Engine) warnings() []model.Warning {
	var ws []model.Warning
	if e.procs > manyProcs {
		ws = append(ws, model.Warning{Code: "many_processes", Count: e.procs,
			Hint: fmt.Sprintf("over %d processes: refreshing every %v, argv only for projects and listeners", manyProcs, e.floor())})
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

// lookupUser names a uid: the current user, then any other, then the number itself. Without
// cgo, os/user on macOS still calls libSystem's getpwuid_r, which asks the directory service;
// on Linux it reads /etc/passwd.
//
// ponytail: runs in the Run goroutine outside the collection timeout, so a hung network
// directory stalls the loop on a new uid; look up in a goroutine with a timeout, using the
// numeric uid meanwhile, if that ever happens.
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
