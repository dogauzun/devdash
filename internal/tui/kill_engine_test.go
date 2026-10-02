package tui

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// TestKillEngineSource runs the kill flow against a real engine over collector.Fake: the plan
// comes from engine.NewPlan on the engine's snapshot, Kill is the recording fakeKiller (never
// the engine's: nothing is signalled), and the refresh after the kill reaches the engine,
// whose next snapshot arrives before the tick would have produced it.
func TestKillEngineSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A pid above every OS's pid limit (2^22 on Linux), so it can never be devdash's own
		// ancestry, which NewPlan reads (read only) from the OS.
		const pid = 5_000_000
		started := time.Now().Add(-time.Hour)
		before := collector.Step{Result: collector.Result{
			TakenAt:   time.Now(),
			Host:      model.Host{Hostname: "box", UID: 501},
			Processes: []collector.Process{{PID: pid, PPID: 1, StartTime: started, UID: 501, Name: "postgres", Argv: []string{"postgres"}}},
		}}
		after := collector.Step{Result: collector.Result{TakenAt: time.Now(), Host: model.Host{Hostname: "box", UID: 501}}}
		e := engine.New(engine.Options{
			Collector:  &collector.Fake{Steps: []collector.Step{before, after}},
			Resolver:   model.NewResolver("", nil),
			LookupUser: func(int) string { return "me" },
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go e.Run(ctx)

		fk := &fakeKiller{}
		m := New(Options{Source: e, Now: time.Now, Kill: fk.kill}) // Plan: engine.NewPlan
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		step := func() { m.Update(m.wait()()) }

		step()
		if len(m.upd.Snapshot.Processes) != 1 {
			t.Fatalf("engine snapshot has %d processes, want postgres", len(m.upd.Snapshot.Processes))
		}
		target := m.upd.Snapshot.Processes[0]
		selectRow(t, m, target.Key())
		press(m, "x", "enter") // outside every project: the second prompt
		if line(m, "postgres is outside every project") == "" {
			t.Fatalf("no second confirmation:\n%s", screen(m))
		}
		start := time.Now()
		run(t, m, press(m, "Y"))
		if len(fk.plans) != 1 || len(fk.plans[0].Procs) != 1 || fk.plans[0].Procs[0].Key() != target.Key() {
			t.Fatalf("Kill got %+v, want postgres's plan once", fk.plans)
		}
		if m.kill.active() || line(m, "killed 1 process") == "" {
			t.Errorf("after the kill: modal open %v\n%s", m.kill.active(), screen(m))
		}

		step() // the snapshot the refresh asked for
		if waited := time.Since(start); waited >= engine.DefaultTick {
			t.Errorf("next snapshot after %s: the refresh did not reach the engine", waited)
		}
		if n := len(m.upd.Snapshot.Processes); n != 0 {
			t.Errorf("snapshot after the kill has %d processes, want 0", n)
		}
	})
}
