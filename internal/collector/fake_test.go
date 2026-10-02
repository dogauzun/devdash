package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{Steps: []Step{
		{Result: Result{Processes: []Process{{PID: 1}}}},
		{Err: boom},
		{Block: true},
		{Result: Result{Processes: []Process{{PID: 2}}}},
	}}
	pid := func(r Result) int {
		if len(r.Processes) == 0 {
			return 0
		}
		return r.Processes[0].PID
	}
	ctx := context.Background()
	if r, err := f.Collect(ctx, Options{}); err != nil || pid(r) != 1 {
		t.Errorf("step 1: %v %v", r, err)
	}
	if _, err := f.Collect(ctx, Options{}); !errors.Is(err, boom) {
		t.Errorf("step 2: %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := f.Collect(tctx, Options{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("step 3: %v", err)
	}
	for range 2 { // last step repeats
		if r, err := f.Collect(ctx, Options{}); err != nil || pid(r) != 2 {
			t.Errorf("step 4+: %v %v", r, err)
		}
	}
	if _, err := (&Fake{}).Collect(ctx, Options{}); err == nil {
		t.Error("empty fake: want an error")
	}
}

// TestFakeCalls: the Fake records the Options of each Collect, so engine tests can check them.
func TestFakeCalls(t *testing.T) {
	f := &Fake{Steps: []Step{{}}}
	inProject := func([]Process) []bool { return nil }
	for _, o := range []Options{{}, {InProject: inProject}} {
		if _, err := f.Collect(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.Calls(); len(c) != 2 || c[0].InProject != nil || c[1].InProject == nil {
		t.Errorf("calls %+v, want no InProject then one", c)
	}
}

// TestFakeIntoBuild is the DEV-12 "done when": a snapshot built from the fake, no OS access.
func TestFakeIntoBuild(t *testing.T) {
	f := &Fake{Steps: []Step{{Result: Result{
		Processes: []Process{{PID: 10, Name: "node"}},
		Listeners: []Listener{{Proto: "tcp4", Port: 3000, PID: 10}, {Proto: "tcp4", Port: 22}},
	}}}}
	raw, err := f.Collect(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := model.Build(raw, model.Snapshot{}, nil, model.NewResolver("", nil))
	if len(s.Processes) != 2 || s.Processes[0].Kind != model.KindServer || s.Processes[1].Unknown&model.FieldOwner == 0 {
		t.Errorf("snapshot %+v", s.Processes)
	}
}
