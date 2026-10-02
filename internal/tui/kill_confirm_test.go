package tui

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Review fixes for the kill modal: the second confirmation cannot be reached by a held or
// double-tapped key, and confirm is offered only while at least one pid is on screen.

// withPostgres adds postgres, a server outside every project, to s.
func withPostgres(s model.Snapshot) (model.Snapshot, model.Process) {
	pg := model.Process{PID: 400, PPID: 1, StartTime: at(time.Hour), UID: 501, User: "me", Name: "postgres",
		Argv: []string{"postgres"}, Kind: model.KindServer, Listeners: []model.Listener{lis("tcp4", "127.0.0.1", 5433)}}
	s.Processes = append(s.Processes, pg)
	return s, pg
}

// repeat is k as a terminal sends it while the key is held down.
func repeat(k string) tea.KeyPressMsg {
	msg := key(k)
	msg.IsRepeat = true
	return msg
}

// send delivers key presses and returns every command they produced.
func send(m *Model, keys ...tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd
	for _, k := range keys {
		if _, cmd := m.Update(k); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

func TestKillSecondConfirmNeedsY(t *testing.T) {
	s, pg := withPostgres(fixture())
	for _, keys := range [][]tea.KeyPressMsg{
		{key("enter"), key("enter")},
		{key("enter"), repeat("enter")},
		{key("y"), key("y")},
		{key("y"), repeat("y")},
		{key("enter"), key("y")},
		{key("y"), key("enter")},
		{key("Y")},
		{key("Y"), key("Y")},
		{key("enter"), repeat("Y")},
		{key("enter"), key("enter"), key("enter"), key("y"), key("y")},
	} {
		m, _, _, fk := newKillTest(t, 80, 24, s)
		selectRow(t, m, pg.Key())
		press(m, "x")
		if cmds := send(m, keys...); len(cmds) != 0 || len(fk.plans) != 0 {
			t.Errorf("%v signalled a process outside every project", keys)
		}
		if !m.kill.active() {
			t.Errorf("%v closed the modal", keys)
		}
	}

	m, _, _, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, pg.Key())
	press(m, "x", "enter")
	if line(m, "Confirm again: press Y") == "" {
		t.Errorf("second prompt does not ask for Y:\n%s", screen(m))
	}
	run(t, m, press(m, "Y"))
	if len(fk.plans) != 1 || fk.plans[0].Procs[0].PID != 400 {
		t.Errorf("Kill got %+v after enter, Y; want postgres once", fk.plans)
	}
}

func TestKillIgnoresRepeatedKeys(t *testing.T) {
	s := fixture()
	m, _, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true} }), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x")
	if cmds := send(m, repeat("enter"), repeat("y")); len(cmds) != 0 || len(fk.plans) != 0 {
		t.Fatal("a repeated confirm key signalled")
	}
	run(t, m, press(m, "enter"))
	if line(m, "f force-kill survivors") == "" {
		t.Fatalf("no survivor report:\n%s", screen(m))
	}
	if cmds := send(m, repeat("f")); len(cmds) != 0 || len(fk.plans) != 1 {
		t.Error("a repeated f force-killed the survivors")
	}
}

// pidLine matches a plan line: an indented pid, then the name.
var pidLine = regexp.MustCompile(`(?m)^  \d+  `)

func TestKillConfirmNeedsAVisiblePid(t *testing.T) {
	s := fixture()
	for i := range 40 {
		s.Processes = append(s.Processes, model.Process{PID: 1000 + i, PPID: 102, StartTime: at(time.Minute),
			Name: "worker", ProjectID: shopID, Kind: model.KindOther})
	}
	s, pg := withPostgres(s)
	plans := []struct {
		name   string
		target model.RowKey
		keys   []string // after x
	}{
		{"one pid", keyOf(s, 103), nil},
		{"42-pid tree", keyOf(s, 101), []string{"t"}},
	}
	for _, p := range plans {
		for _, w := range []int{80, 40} {
			for h := 1; h <= 30; h++ {
				name := fmt.Sprintf("%s at %dx%d", p.name, w, h)
				m, _, _, fk := newKillTest(t, w, 30, s)
				selectRow(t, m, p.target)
				press(m, append([]string{"x"}, p.keys...)...)
				m.Update(tea.WindowSizeMsg{Width: w, Height: h})
				sc := screen(m)
				visible := pidLine.MatchString(sc)
				if line(m, "enter confirm") != "" && !visible {
					t.Errorf("%s: confirm offered with no pid on screen:\n%s", name, sc)
				}
				if !visible {
					if cmd := press(m, "enter"); cmd != nil || len(fk.plans) != 0 {
						t.Errorf("%s: enter signalled with no pid on screen", name)
					}
				}
			}
		}
	}

	// The second prompt, after the terminal shrinks under it.
	for h := 1; h <= 30; h++ {
		m, _, _, fk := newKillTest(t, 80, 30, s)
		selectRow(t, m, pg.Key())
		press(m, "x", "enter")
		m.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		sc := screen(m)
		visible := pidLine.MatchString(sc)
		if line(m, "press Y") != "" && !visible {
			t.Errorf("80x%d: second confirm offered with no pid on screen:\n%s", h, sc)
		}
		if !visible {
			if cmd := press(m, "Y"); cmd != nil || len(fk.plans) != 0 {
				t.Errorf("80x%d: Y signalled with no pid on screen", h)
			}
		}
	}
}

// TestKillSmallPlanFits: when a short plan does not fit with its blank lines, the blank lines
// go before any pid line (80x8 showed "2 pids" and no pid while confirm worked).
func TestKillSmallPlanFits(t *testing.T) {
	s := fixture()
	for _, h := range []int{7, 8, 9} {
		m, _, _, _ := newKillTest(t, 80, h, s)
		selectRow(t, m, keyOf(s, 101))
		press(m, "x", "t")
		if line(m, "101 ") == "" || line(m, "102 ") == "" || line(m, "enter confirm") == "" {
			t.Errorf("80x%d: the 2-pid plan or its hint is not all on screen:\n%s", h, screen(m))
		}
	}
}
