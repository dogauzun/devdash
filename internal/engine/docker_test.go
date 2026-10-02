package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// fakeDocker is a scripted ContainerSource: call n (from 1) runs fetch(ctx, n). It records
// when each call started and how long its context allowed, in bubble time.
type fakeDocker struct {
	t0    time.Time
	fetch func(ctx context.Context, n int) ([]model.Container, *model.Warning)

	mu        sync.Mutex
	calls     []time.Duration // start of each call, since t0
	deadlines []time.Duration // each call's context deadline, relative to its start
	returned  atomic.Int32
}

func (f *fakeDocker) Fetch(ctx context.Context) ([]model.Container, *model.Warning) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Since(f.t0))
	n := len(f.calls)
	dl := time.Duration(-1)
	if d, ok := ctx.Deadline(); ok {
		dl = time.Until(d)
	}
	f.deadlines = append(f.deadlines, dl)
	f.mu.Unlock()
	defer f.returned.Add(1)
	return f.fetch(ctx, n)
}

func (f *fakeDocker) starts() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// container is call n's list: one container named "c<n>" publishing 8080.
func container(n int) []model.Container {
	return []model.Container{{ID: "id", Name: "c" + string(rune('0'+n)), Image: "nginx", State: "running",
		Ports: []model.PortMapping{{HostPort: 8080, ContainerPort: 80, Proto: "tcp"}}}}
}

func names(s model.Snapshot) []string {
	var ns []string
	for _, c := range s.Containers {
		ns = append(ns, c.Name)
	}
	return ns
}

func warning(ws []model.Warning, code string) *model.Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

var unreachable = model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: not reachable at unix:///var/run/docker.sock"}

// TestDockerCadence: Fetch runs at start and every 5 s on its own goroutine, each call with a
// 1 s timeout; every tick builds with the latest list.
func TestDockerCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(_ context.Context, n int) ([]model.Container, *model.Warning) {
			return container(n), nil
		}}
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Docker: d})
		t0 := time.Now()
		next(t, e, t0, 0) // races the first Fetch: either list is right
		for _, tt := range []struct {
			at   time.Duration
			want string
		}{{2e9, "c1"}, {4e9, "c1"}, {6e9, "c2"}, {8e9, "c2"}, {12e9, "c3"}} {
			if tt.at == 12e9 {
				next(t, e, t0, 10e9) // ties with the third Fetch
			}
			u := next(t, e, t0, tt.at)
			if got := names(u.Snapshot); !slices.Equal(got, []string{tt.want}) {
				t.Errorf("tick at %v: containers %v, want %s", tt.at, got, tt.want)
			}
			if _, ok := u.Snapshot.Timing["docker"]; !ok {
				t.Errorf("tick at %v: no docker timing in %v", tt.at, u.Snapshot.Timing)
			}
		}
		stop()
		if got, want := d.starts(), []time.Duration{0, 5e9, 10e9}; !slices.Equal(got, want) {
			t.Errorf("Fetch calls at %v, want %v", got, want)
		}
		for i, dl := range d.deadlines {
			if dl != dockerTimeout {
				t.Errorf("call %d: context deadline %v after start, want %v", i+1, dl, dockerTimeout)
			}
		}
	})
}

// TestDockerSlowFetch: a Fetch that takes longer than its timeout (even ignoring its context)
// never delays a collector tick; the ticks keep the list they had.
func TestDockerSlowFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(_ context.Context, n int) ([]model.Container, *model.Warning) {
			if n == 2 {
				time.Sleep(2500 * time.Millisecond) // past its 1 s timeout, ctx not checked
			}
			return container(n), nil
		}}
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Docker: d})
		defer stop()
		t0 := time.Now()
		next(t, e, t0, 0)
		for _, tt := range []struct {
			at   time.Duration
			want string
		}{{2e9, "c1"}, {4e9, "c1"}, {6e9, "c1"}, {8e9, "c2"}} {
			u := next(t, e, t0, tt.at)
			if got := names(u.Snapshot); !slices.Equal(got, []string{tt.want}) {
				t.Errorf("tick at %v: containers %v, want %s", tt.at, got, tt.want)
			}
			if u.Interval != DefaultTick {
				t.Errorf("tick at %v: interval %v", tt.at, u.Interval)
			}
		}
		// The slow call (5 s to 7.5 s) delayed nothing, and no call started while it ran.
		if got, want := d.starts(), []time.Duration{0, 5e9}; !slices.Equal(got, want) {
			t.Errorf("Fetch calls at %v, want %v", got, want)
		}
	})
}

// TestDockerWarning: Fetch's warning goes into the snapshot's warnings (merged like collector
// warnings), not Update.Warnings, and leaves with the next list that has none.
func TestDockerWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(_ context.Context, n int) ([]model.Container, *model.Warning) {
			if n == 1 {
				w := unreachable
				return container(n), &w
			}
			return container(n), nil
		}}
		f := &collector.Fake{Steps: []collector.Step{{Result: collector.Result{
			Processes: []collector.Process{{PID: 1}},
			Warnings:  []model.Warning{{Code: "process_fields_unreadable", Count: 3, Hint: "run with sudo"}},
		}}}}
		e, stop := start(t, Options{Collector: f, Docker: d})
		defer stop()
		t0 := time.Now()
		next(t, e, t0, 0)
		u := next(t, e, t0, 2*time.Second)
		if w := warning(u.Snapshot.Warnings, unreachable.Code); w == nil || *w != unreachable {
			t.Errorf("snapshot warnings %v, want %v", u.Snapshot.Warnings, unreachable)
		}
		if warning(u.Snapshot.Warnings, "process_fields_unreadable") == nil {
			t.Errorf("collector warning lost: %v", u.Snapshot.Warnings)
		}
		if warning(u.Warnings, unreachable.Code) != nil {
			t.Errorf("docker warning in Update.Warnings: %v", u.Warnings)
		}
		next(t, e, t0, 4*time.Second)
		u = next(t, e, t0, 6*time.Second)
		if warning(u.Snapshot.Warnings, unreachable.Code) != nil || len(u.Snapshot.Warnings) != 1 {
			t.Errorf("after a good fetch: warnings %v", u.Snapshot.Warnings)
		}
	})
}

// TestDockerShutdown: cancelling Run stops the Docker goroutine, and Run returns only after
// the Fetch in flight has returned.
func TestDockerShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(ctx context.Context, n int) ([]model.Container, *model.Warning) {
			if n == 2 {
				<-ctx.Done()
				time.Sleep(100 * time.Millisecond) // cleaning up after the cancel
			}
			return container(n), nil
		}}
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Docker: d})
		time.Sleep(5*time.Second + 500*time.Millisecond) // the second Fetch is waiting on its context
		synctest.Wait()
		if n := d.returned.Load(); n != 1 {
			t.Fatalf("%d calls returned before shutdown, want 1", n)
		}
		t0 := time.Now()
		stop()
		if n := d.returned.Load(); n != 2 || time.Since(t0) != 100*time.Millisecond {
			t.Errorf("Run returned after %v with a Fetch in flight (%d returned)", time.Since(t0), n)
		}
		for range e.Updates() { // drain the last buffered update; the channel must be closed
		}
		if n := len(d.starts()); n != 2 {
			t.Errorf("%d Fetch calls, want 2", n)
		}
	})
}

// TestNoDocker: without a source, no containers and no docker timing.
func TestNoDocker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, stop := start(t, Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}})
		defer stop()
		<-e.Updates()
		u := <-e.Updates()
		if _, ok := u.Snapshot.Timing["docker"]; ok || u.Snapshot.Containers != nil {
			t.Errorf("no Docker: containers %v timing %v", u.Snapshot.Containers, u.Snapshot.Timing)
		}
	})
}

// TestSnapshotDocker: the one-shot snapshot fetches once, with the 1 s timeout, and records the
// fetch's duration as Timing["docker"] and its warning in Warnings.
func TestSnapshotDocker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(_ context.Context, n int) ([]model.Container, *model.Warning) {
			time.Sleep(30 * time.Millisecond)
			w := unreachable
			return container(n), &w
		}}
		o := Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Resolver: model.NewResolver("", nil), Docker: d}
		s, err := Snapshot(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(s); !slices.Equal(got, []string{"c1"}) {
			t.Errorf("containers %v", got)
		}
		if s.Timing["docker"] != 30*time.Millisecond {
			t.Errorf("docker timing %v, want 30ms", s.Timing["docker"])
		}
		if w := warning(s.Warnings, unreachable.Code); w == nil || *w != unreachable {
			t.Errorf("warnings %v", s.Warnings)
		}
		s, err = SnapshotAfter(context.Background(), o, s)
		if err != nil || !slices.Equal(names(s), []string{"c2"}) {
			t.Errorf("SnapshotAfter: containers %v, err %v", names(s), err)
		}
		if got := len(d.starts()); got != 2 {
			t.Errorf("%d Fetch calls for two snapshots, want 2", got)
		}
		if d.deadlines[0] != dockerTimeout {
			t.Errorf("deadline %v, want %v", d.deadlines[0], dockerTimeout)
		}

		s, err = Snapshot(context.Background(), Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Resolver: model.NewResolver("", nil)})
		if _, ok := s.Timing["docker"]; ok || err != nil {
			t.Errorf("no Docker: timing %v, err %v", s.Timing, err)
		}
	})
}

// TestSnapshotDockerBounded: a Fetch waiting on its context ends the one-shot snapshot after
// the 1 s timeout; Fetch runs alongside Collect, so a collector failure still wins.
func TestSnapshotDockerBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDocker{t0: time.Now(), fetch: func(ctx context.Context, _ int) ([]model.Container, *model.Warning) {
			<-ctx.Done()
			return nil, nil
		}}
		r := model.NewResolver("", nil)
		t0 := time.Now()
		s, err := Snapshot(context.Background(), Options{Collector: &collector.Fake{Steps: []collector.Step{step(1)}}, Resolver: r, Docker: d})
		if err != nil || time.Since(t0) != dockerTimeout || s.Timing["docker"] != dockerTimeout {
			t.Errorf("hanging Fetch: err %v after %v, docker timing %v", err, time.Since(t0), s.Timing["docker"])
		}
		boom := errors.New("boom")
		t0 = time.Now()
		_, err = Snapshot(context.Background(), Options{Collector: &collector.Fake{Steps: []collector.Step{{Err: boom}}}, Resolver: r, Docker: d})
		if !errors.Is(err, boom) || time.Since(t0) != 0 {
			t.Errorf("failing collector: err %v after %v", err, time.Since(t0))
		}
		synctest.Wait() // the abandoned Fetch saw its context cancelled and returned
		if n := d.returned.Load(); n != 2 {
			t.Errorf("%d Fetch calls returned, want 2", n)
		}
	})
}
