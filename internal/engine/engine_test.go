package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// collectFunc adapts a function to collector.Collector, for collectors the Fake cannot script:
// slow ones and ones stuck past their context.
type collectFunc func(ctx context.Context) (collector.Result, error)

func (f collectFunc) Collect(ctx context.Context, _ collector.Options) (collector.Result, error) {
	return f(ctx)
}

func step(pid int) collector.Step {
	return collector.Step{Result: collector.Result{Processes: []collector.Process{{PID: pid, UID: os.Getuid()}}}}
}

func pid(u Update) int {
	if len(u.Snapshot.Processes) == 0 {
		return 0
	}
	return u.Snapshot.Processes[0].PID
}

// start runs a new engine in the bubble; the returned func stops it and waits for Run to return.
func start(t *testing.T, o Options) (*Engine, func()) {
	t.Helper()
	if o.Resolver == nil {
		o.Resolver = model.NewResolver("", nil)
	}
	e := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	return e, func() { cancel(); <-done }
}

// next waits for the next update and checks it arrives after exactly want of bubble time.
func next(t *testing.T, e *Engine, since time.Time, want time.Duration) Update {
	t.Helper()
	u, ok := <-e.Updates()
	if !ok {
		t.Fatal("updates closed")
	}
	if got := time.Since(since); got != want {
		t.Errorf("update at %v, want %v", got, want)
	}
	return u
}

func TestTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1), step(2), step(3)}}})
		defer stop()
		t0 := time.Now()
		for i, at := range []time.Duration{0, 2 * time.Second, 4 * time.Second} {
			u := next(t, e, t0, at)
			if pid(u) != i+1 || u.Err != nil || u.Missed != 0 || u.Interval != DefaultTick {
				t.Errorf("tick %d: pid %d err %v missed %d interval %v", i, pid(u), u.Err, u.Missed, u.Interval)
			}
		}
	})
}

func TestLatestWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1), step(2), step(3), step(4)}}})
		defer stop()
		time.Sleep(4*time.Second + time.Millisecond) // three ticks, nobody reading
		synctest.Wait()
		if u := <-e.Updates(); pid(u) != 3 {
			t.Errorf("slow consumer got pid %d, want the latest (3)", pid(u))
		}
		select {
		case u := <-e.Updates():
			t.Errorf("second value buffered: pid %d", pid(u))
		default:
		}
	})
}

func TestRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1), step(2), step(3)}}})
		defer stop()
		t0 := time.Now()
		next(t, e, t0, 0)
		time.Sleep(500 * time.Millisecond)
		e.Refresh()
		if u := next(t, e, t0, 500*time.Millisecond); pid(u) != 2 {
			t.Errorf("refresh: pid %d", pid(u))
		}
		// The regular cadence restarts from the refresh.
		if u := next(t, e, t0, 2500*time.Millisecond); pid(u) != 3 {
			t.Errorf("after refresh: pid %d", pid(u))
		}
	})
}

func TestStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{
			step(1), {Block: true}, {Err: boom}, step(2),
		}}})
		defer stop()
		t0 := time.Now()
		next(t, e, t0, 0)
		u := next(t, e, t0, 3500*time.Millisecond) // 2 s interval + 1.5 s timeout
		if !errors.Is(u.Err, context.DeadlineExceeded) || pid(u) != 1 || u.Missed != 1 {
			t.Errorf("timed out tick: err %v pid %d missed %d", u.Err, pid(u), u.Missed)
		}
		u = next(t, e, t0, 5500*time.Millisecond)
		if !errors.Is(u.Err, boom) || pid(u) != 1 || u.Missed != 2 {
			t.Errorf("failed tick: err %v pid %d missed %d", u.Err, pid(u), u.Missed)
		}
		u = next(t, e, t0, 7500*time.Millisecond)
		if u.Err != nil || pid(u) != 2 || u.Missed != 0 {
			t.Errorf("recovered: err %v pid %d missed %d", u.Err, pid(u), u.Missed)
		}
	})
}

// TestStuckCollect: a Collect that ignores its context is abandoned, and no second one starts
// until it returns.
func TestStuckCollect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		c := collectFunc(func(ctx context.Context) (collector.Result, error) {
			if calls.Add(1) == 2 {
				<-release // stuck in the kernel: ctx is not checked
			}
			return step(int(calls.Load())).Result, nil
		})
		e, stop := start(t, Options{Collector: c})
		defer stop()
		t0 := time.Now()
		next(t, e, t0, 0)
		u := next(t, e, t0, 3500*time.Millisecond)
		if !errors.Is(u.Err, context.DeadlineExceeded) || u.Missed != 1 {
			t.Errorf("stuck tick: err %v missed %d", u.Err, u.Missed)
		}
		// Still stuck: the next ticks fail at once, without calling Collect again.
		for i, at := range []time.Duration{5500 * time.Millisecond, 7500 * time.Millisecond} {
			u = next(t, e, t0, at)
			if !errors.Is(u.Err, context.DeadlineExceeded) || u.Missed != i+2 || pid(u) != 1 {
				t.Errorf("busy tick %d: err %v missed %d pid %d", i, u.Err, u.Missed, pid(u))
			}
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("Collect called %d times while one was stuck, want 2", n)
		}
		if u.Interval != 4*time.Second {
			t.Errorf("three timed-out ticks: interval %v, want 4s", u.Interval)
		}
		close(release)
		u = next(t, e, t0, 11500*time.Millisecond)
		if u.Err != nil || pid(u) != 3 || calls.Load() != 3 {
			t.Errorf("after release: err %v pid %d calls %d", u.Err, pid(u), calls.Load())
		}
	})
}

func TestAdaptiveInterval(t *testing.T) {
	tests := []struct {
		name string
		tick time.Duration
		slow []bool // per tick: true takes 600 ms
		want []time.Duration
	}{
		{
			name: "doubles to the cap, halves to the floor",
			tick: 2 * time.Second,
			slow: []bool{true, true, true, true, true, true, true, true, true, true, true, true,
				false, false, false, false, false, false, false, false, false, false, false, false},
			want: []time.Duration{2e9, 2e9, 4e9, 4e9, 4e9, 8e9, 8e9, 8e9, 10e9, 10e9, 10e9, 10e9,
				10e9, 10e9, 5e9, 5e9, 5e9, 2.5e9, 2.5e9, 2.5e9, 2e9, 2e9, 2e9, 2e9},
		},
		{
			name: "a fast tick resets the slow run",
			tick: 2 * time.Second,
			slow: []bool{true, true, false, true, true, true},
			want: []time.Duration{2e9, 2e9, 2e9, 2e9, 2e9, 4e9},
		},
		{
			name: "configured above the cap never shrinks",
			tick: 30 * time.Second,
			slow: []bool{true, true, true},
			want: []time.Duration{30e9, 30e9, 30e9},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var n atomic.Int32
				c := collectFunc(func(ctx context.Context) (collector.Result, error) {
					if tt.slow[min(int(n.Add(1))-1, len(tt.slow)-1)] {
						time.Sleep(600 * time.Millisecond)
					}
					return step(1).Result, nil
				})
				e, stop := start(t, Options{Collector: c, Tick: tt.tick})
				defer stop()
				for i, want := range tt.want {
					u := <-e.Updates()
					if u.Interval != want {
						t.Errorf("tick %d: interval %v, want %v", i, u.Interval, want)
					}
					if got := hasWarning(u, "refresh_slowed"); got != (want > tt.tick) {
						t.Errorf("tick %d: refresh_slowed warning %v at %v", i, got, want)
					}
				}
			})
		})
	}
}

func hasWarning(u Update, code string) bool {
	for _, w := range u.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestSlowedHintOnFailedTicks: backoff caused by timed-out ticks is reported although no new
// snapshot is built, and the held snapshot is not touched.
func TestSlowedHintOnFailedTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1), {Block: true}}}})
		defer stop()
		first := <-e.Updates()
		for i, want := range []time.Duration{2e9, 2e9, 4e9, 4e9, 4e9, 8e9} {
			u := <-e.Updates()
			if u.Interval != want || hasWarning(u, "refresh_slowed") != (want > DefaultTick) {
				t.Errorf("failed tick %d: interval %v, want %v; warnings %v", i, u.Interval, want, u.Warnings)
			}
			if len(u.Snapshot.Warnings) != 0 {
				t.Errorf("failed tick %d: held snapshot changed: %v", i, u.Snapshot.Warnings)
			}
		}
		if len(first.Snapshot.Warnings) != 0 {
			t.Errorf("first snapshot changed: %v", first.Snapshot.Warnings)
		}
	})
}

func TestTickBounds(t *testing.T) {
	for _, tt := range []struct{ tick, want time.Duration }{
		{0, DefaultTick}, {100 * time.Millisecond, MinTick}, {time.Second, time.Second},
	} {
		if got := New(Options{Tick: tt.tick}).interval; got != tt.want {
			t.Errorf("Tick %v: interval %v, want %v", tt.tick, got, tt.want)
		}
	}
}

func TestManyProcesses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		procs := make([]collector.Process, 5001)
		for i := range procs {
			procs[i] = collector.Process{PID: i + 1}
		}
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{{Result: collector.Result{Processes: procs}}}}})
		defer stop()
		t0 := time.Now()
		for _, at := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second} {
			u := next(t, e, t0, at) // fast ticks never halve below 5 s
			if u.Interval != 5*time.Second || !hasWarning(u, "many_processes") || hasWarning(u, "refresh_slowed") {
				t.Errorf("at %v: interval %v warnings %v", at, u.Interval, u.Warnings)
			}
		}
	})
}

// TestManyProcessesLimitsArgv: after a good tick with more than 5000 processes the next
// Collect limits argv (Options.InProject set); after one with 5000 or fewer it does not.
// Snapshot, the one-shot of --json, port and kill, never limits it (DEV-92).
func TestManyProcessesLimitsArgv(t *testing.T) {
	procs := func(n int) collector.Step {
		ps := make([]collector.Process, n)
		for i := range ps {
			ps[i] = collector.Process{PID: i + 1}
		}
		return collector.Step{Result: collector.Result{Processes: ps}}
	}
	synctest.Test(t, func(t *testing.T) {
		f := &collector.Fake{Steps: []collector.Step{procs(5001), procs(5001), procs(5000), procs(5000)}}
		e, stop := start(t, Options{Collector: f})
		for range 4 {
			<-e.Updates()
		}
		stop()
		calls := f.Calls()
		if len(calls) < 4 {
			t.Fatalf("%d calls, want 4", len(calls))
		}
		for i, want := range []bool{false, true, true, false} {
			if got := calls[i].InProject != nil; got != want {
				t.Errorf("Collect %d: argv limited %v, want %v", i+1, got, want)
			}
		}
	})

	f := &collector.Fake{Steps: []collector.Step{procs(5001)}}
	o := Options{Collector: f, Resolver: model.NewResolver("", nil)}
	first, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotAfter(context.Background(), o, first); err != nil {
		t.Fatal(err)
	}
	for i, c := range f.Calls() {
		if c.InProject != nil {
			t.Errorf("one-shot Collect %d limits argv", i+1)
		}
	}
}

// TestInProject: the engine answers InProject with its Resolver, by a process's own cwd or an
// ancestor's (spec steps 1-5), without touching the processes it is given (DEV-92).
func TestInProject(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	procs := []model.Process{
		{PID: 10, PPID: 1, Cwd: filepath.Join(repo, "src")},
		{PID: 11, PPID: 1, Cwd: outside},
		{PID: 12, PPID: 10, Cwd: "/"}, // its parent is in the repository
	}
	e := New(Options{Resolver: model.NewResolver("", nil)})
	if got, want := e.inProject(procs), []bool{true, false, true}; !slices.Equal(got, want) {
		t.Errorf("inProject = %v, want %v", got, want)
	}
	for _, p := range procs {
		if p.ProjectID != "" {
			t.Errorf("pid %d: ProjectID set to %q on the caller's slice", p.PID, p.ProjectID)
		}
	}
	if got := New(Options{}).inProject(procs); !slices.Equal(got, make([]bool, len(procs))) {
		t.Errorf("no Resolver: %v, want all false", got)
	}
}

func TestShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1), {Block: true}}}})
		<-e.Updates()
		time.Sleep(2500 * time.Millisecond) // mid-collection
		stop()
		if _, ok := <-e.Updates(); ok {
			t.Error("update after shutdown")
		}
		// synctest.Test fails if any goroutine of the engine is still around.
	})
}

func TestCPUPercentUsesPrevious(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now()
		raw := func(at time.Duration, cpu time.Duration) collector.Step {
			return collector.Step{Result: collector.Result{TakenAt: t0.Add(at),
				Processes: []collector.Process{{PID: 7, StartTime: t0, CPUTime: cpu}}}}
		}
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{raw(0, 0), raw(2*time.Second, time.Second)}}})
		defer stop()
		if u := <-e.Updates(); !math.IsNaN(u.Snapshot.Processes[0].CPUPercent) {
			t.Errorf("first sample: %v", u.Snapshot.Processes[0].CPUPercent)
		}
		if u := <-e.Updates(); u.Snapshot.Processes[0].CPUPercent != 50 {
			t.Errorf("second sample: %v, want 50", u.Snapshot.Processes[0].CPUPercent)
		}
	})
}

func TestSnapshot(t *testing.T) {
	r := model.NewResolver("", nil)
	boom := errors.New("boom")
	f := &collector.Fake{Steps: []collector.Step{{Result: collector.Result{
		Processes: []collector.Process{{PID: 10, UID: os.Getuid()}},
		Listeners: []collector.Listener{{Proto: "tcp4", Port: 22}},
	}}}}
	s, err := Snapshot(context.Background(), Options{Collector: f, Resolver: r})
	if err != nil || len(s.Processes) != 2 {
		t.Fatalf("snapshot %+v, err %v", s.Processes, err)
	}
	if s.Processes[0].User == "" || s.Processes[1].User != "" {
		t.Errorf("users %q (pid 10), %q (pid 0), want a name and empty", s.Processes[0].User, s.Processes[1].User)
	}
	if _, err := Snapshot(context.Background(), Options{Collector: &collector.Fake{Steps: []collector.Step{{Err: boom}}}, Resolver: r}); !errors.Is(err, boom) {
		t.Errorf("failing collector: err %v", err)
	}
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		stuck := collectFunc(func(context.Context) (collector.Result, error) { <-release; return collector.Result{}, nil })
		t0 := time.Now()
		_, err := Snapshot(context.Background(), Options{Collector: stuck, Resolver: r})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) != collectTimeout {
			t.Errorf("stuck collector: err %v after %v", err, time.Since(t0))
		}
		close(release)
	})
}

func TestSnapshotAfter(t *testing.T) {
	t0 := time.Now()
	step := func(at, cpu time.Duration) collector.Step {
		return collector.Step{Result: collector.Result{TakenAt: t0.Add(at),
			Processes: []collector.Process{{PID: 7, StartTime: t0, CPUTime: cpu}}}}
	}
	o := Options{Collector: &collector.Fake{Steps: []collector.Step{step(0, 0), step(200*time.Millisecond, 50*time.Millisecond)}}, Resolver: model.NewResolver("", nil)}
	first, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s, err := SnapshotAfter(context.Background(), o, first)
	if err != nil || s.Processes[0].CPUPercent != 25 {
		t.Errorf("cpu %v, err %v; want 25", s.Processes[0].CPUPercent, err)
	}
}

func TestUserCache(t *testing.T) {
	calls := map[int]int{}
	c := userCache{names: map[int]string{}, lookup: func(uid int) string { calls[uid]++; return "u" + strconv.Itoa(uid) }}
	for _, uid := range []int{0, 501, 0, 501, 501} {
		if got := c.name(uid); got != "u"+strconv.Itoa(uid) {
			t.Errorf("name(%d) = %q", uid, got)
		}
	}
	if calls[0] != 1 || calls[501] != 1 {
		t.Errorf("lookups %v, want one per uid", calls)
	}
}

func TestLookupUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	if got := lookupUser(os.Getuid()); got != me.Username {
		t.Errorf("own uid: %q, want %q", got, me.Username)
	}
	const nobodyHas = 2147400047
	if got := lookupUser(nobodyHas); got != strconv.Itoa(nobodyHas) {
		t.Errorf("unknown uid: %q, want the number", got)
	}
}

func TestLookupUserOption(t *testing.T) {
	calls := 0
	lookup := func(uid int) string { calls++; return "user" + strconv.Itoa(uid) }
	steps := func() *collector.Fake {
		s := collector.Step{Result: collector.Result{Processes: []collector.Process{{PID: 10, UID: 7}, {PID: 11, UID: 7}}}}
		return &collector.Fake{Steps: []collector.Step{s, s}}
	}
	check := func(t *testing.T, what string, s model.Snapshot) {
		t.Helper()
		for _, p := range s.Processes {
			if p.User != "user7" {
				t.Errorf("%s: pid %d user %q, want user7", what, p.PID, p.User)
			}
		}
	}

	o := Options{Collector: steps(), Resolver: model.NewResolver("", nil), LookupUser: lookup}
	first, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "Snapshot", first)
	s, err := SnapshotAfter(context.Background(), o, first)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "SnapshotAfter", s)
	if calls != 2 {
		t.Errorf("%d lookups, want one per uid per call", calls)
	}

	synctest.Test(t, func(t *testing.T) {
		calls = 0
		e, stop := start(t, Options{Collector: steps(), LookupUser: lookup})
		defer stop()
		check(t, "Run", (<-e.Updates()).Snapshot)
		check(t, "Run", (<-e.Updates()).Snapshot)
		if calls != 1 {
			t.Errorf("Run: %d lookups, want one per uid for the engine's life", calls)
		}
	})
}
