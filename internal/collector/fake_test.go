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
	if r, err := f.Collect(ctx); err != nil || pid(r) != 1 {
		t.Errorf("step 1: %v %v", r, err)
	}
	if _, err := f.Collect(ctx); !errors.Is(err, boom) {
		t.Errorf("step 2: %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := f.Collect(tctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("step 3: %v", err)
	}
	for range 2 { // last step repeats
		if r, err := f.Collect(ctx); err != nil || pid(r) != 2 {
			t.Errorf("step 4+: %v %v", r, err)
		}
	}
	if _, err := (&Fake{}).Collect(ctx); err == nil {
		t.Error("empty fake: want an error")
	}
}

// TestFakeIntoBuild is the DEV-12 "done when": a snapshot built from the fake, no OS access.
func TestFakeIntoBuild(t *testing.T) {
	f := &Fake{Steps: []Step{{Result: Result{
		Processes: []Process{{PID: 10, Name: "node"}},
		Listeners: []Listener{{Proto: "tcp4", Port: 3000, PID: 10}, {Proto: "tcp4", Port: 22}},
	}}}}
	raw, err := f.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := model.Build(raw, model.Snapshot{}, nil, model.NewResolver("", nil))
	if len(s.Processes) != 2 || s.Processes[0].Kind != model.KindServer || s.Processes[1].Unknown&model.FieldOwner == 0 {
		t.Errorf("snapshot %+v", s.Processes)
	}
}
