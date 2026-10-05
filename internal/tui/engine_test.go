package tui

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

// runAsked runs the dashboard with S already confirmed by the first message; then, when sig is
// set, sends sig to this process. Without late, Run ends on the signal alone. With late, the
// first message is also the quit y sends, and sig is sent after the program returned, just
// before Run removes its signal handler (beforeSignalStop), and received by this process before
// Run goes on. It fails the test when Run does not return within 10 s.
func runAsked(t *testing.T, sig syscall.Signal, late bool) (sudo bool, err error) {
	t.Helper()
	send := func() {
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Error(err)
		}
	}
	if sig != 0 {
		// Also delivered here, so a signal Run no longer catches does not end the test binary.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, sig)
		defer signal.Stop(ch)
		if late {
			beforeSignalStop = func() {
				send()
				select { // macOS may deliver a signal sent to the process after kill returns
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Errorf("%v not received", sig)
				}
			}
			defer func() { beforeSignalStop = func() {} }()
		}
	}
	first := true
	filter := tea.WithFilter(func(tm tea.Model, msg tea.Msg) tea.Msg {
		if !first {
			return msg
		}
		first = false
		tm.(*Model).sudo.asked = true
		if sig == 0 || late {
			return tea.QuitMsg{} // the quit y sends
		}
		send()
		return msg
	})
	type result struct {
		sudo bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		src := &fakeSource{ch: make(chan engine.Update)}
		var out strings.Builder
		sudo, err := Run(context.Background(), Options{Source: src, Kill: failKill(t)}, tea.WithInput(strings.NewReader("")), tea.WithOutput(&out), tea.WithWindowSize(80, 24), filter)
		done <- result{sudo, err}
	}()
	select {
	case r := <-done:
		return r.sudo, r.err
	case <-time.After(10 * time.Second): // longer than beforeSignalStop's wait, so no t.Errorf comes after the test
		t.Fatalf("Run did not return on %v", sig)
		return false, nil
	}
}

// TestRunSudo: the quit y sends, with no signal, reports the sudo request (DEV-144).
func TestRunSudo(t *testing.T) {
	if sudo, err := runAsked(t, 0, false); err != nil || !sudo {
		t.Errorf("Run after y: sudo %v, %v; want true, nil", sudo, err)
	}
}

// TestRunReturnsOnSignal: SIGINT, SIGTERM and SIGHUP each end the dashboard like ctrl-c, with
// no error, and never with the sudo request, even one y has already recorded (DEV-147,
// DEV-164, DEV-167).
func TestRunReturnsOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if sudo, err := runAsked(t, sig, false); err != nil || sudo {
			t.Errorf("Run on %v: sudo %v, %v; want false, nil", sig, sudo, err)
		}
	}
}

// TestRunSignalAfterProgram: after y, a signal that arrives once the program has returned, before
// Run removes its handler, still cancels the sudo request (DEV-177).
func TestRunSignalAfterProgram(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if sudo, err := runAsked(t, sig, true); err != nil || sudo {
			t.Errorf("Run on %v after the program returned: sudo %v, %v; want false, nil", sig, sudo, err)
		}
	}
}
