package tui

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// TestEngineSource drives the model from a real engine over collector.Fake, the way runTUI
// wires it: real updates, failed ticks turning the header stale, and the channel closing when
// the engine stops.
func TestEngineSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		good := collector.Step{Result: collector.Result{
			TakenAt:   time.Now(),
			Host:      model.Host{Hostname: "box", UID: 501},
			Processes: []collector.Process{{PID: 42, PPID: 1, StartTime: time.Now(), UID: 501, Name: "node", Argv: []string{"node"}}},
			Listeners: []collector.Listener{{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 3000, PID: 42}},
		}}
		bad := collector.Step{Err: errors.New("boom")}
		e := engine.New(engine.Options{
			Collector:  &collector.Fake{Steps: []collector.Step{good, bad, bad}},
			Resolver:   model.NewResolver("", nil),
			LookupUser: func(int) string { return "me" },
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.Run(ctx); close(done) }()

		m := New(Options{Source: e, Now: time.Now, Kill: failKill(t)})
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		step := func() tea.Cmd { _, cmd := m.Update(m.wait()()); return cmd }

		step()
		header := func() string { return strings.Split(screen(m), "\n")[0] }
		if got, want := header(), "box · 0 s ago · 0 projects · 1 listener · 0 containers"; got != want {
			t.Errorf("header\n got %q\nwant %q", got, want)
		}
		step() // first failed tick: not stale yet
		step() // second: stale, the good snapshot is kept
		if got := header(); !strings.HasPrefix(got, "box · stale 4 s · 0 projects · 1 listener") {
			t.Errorf("after two failed ticks: header %q", got)
		}
		if line(m, "refresh failed: boom") == "" {
			t.Errorf("footer does not show the failed tick:\n%s", screen(m))
		}

		cancel()
		<-done
		for range 2 { // an update may still be buffered before the close
			if cmd := step(); cmd != nil && cmd() == (tea.QuitMsg{}) {
				return
			}
		}
		t.Error("the model does not quit when the engine stops")
	})
}

// TestRunReturnsOnCancel: a cancelled ctx ends Run without an error.
func TestRunReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeSource{ch: make(chan engine.Update)}
	var out strings.Builder
	sudo, err := Run(ctx, Options{Source: src, Kill: failKill(t)}, tea.WithInput(strings.NewReader("")), tea.WithOutput(&out), tea.WithWindowSize(80, 24))
	if err != nil || sudo {
		t.Errorf("Run with a cancelled ctx: sudo %v, %v; want false, nil", sudo, err)
	}
}
