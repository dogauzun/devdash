package tui

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Kill modal tests. Plans come from fakePlanner (or engine.NewPlan, which at most reads
// devdash's own ancestry from the OS) and kills from fakeKiller: nothing is ever signalled.

// planCall is one recorded Plan call.
type planCall struct {
	key  model.RowKey
	opts engine.KillOptions
}

// fakePlanner plans from the snapshot as the engine would, without any OS read: the target,
// then in tree mode its descendants breadth first. refuse makes it return a refusal for the
// given options instead.
type fakePlanner struct {
	calls  []planCall
	plans  []engine.Plan // what each call returned
	refuse func(engine.KillOptions) error
}

func (f *fakePlanner) plan(s model.Snapshot, key model.RowKey, o engine.KillOptions) (engine.Plan, error) {
	f.calls = append(f.calls, planCall{key, o})
	if f.refuse != nil {
		if err := f.refuse(o); err != nil {
			f.plans = append(f.plans, engine.Plan{})
			return engine.Plan{}, err
		}
	}
	var p engine.Plan
	for _, proc := range s.Processes {
		if proc.Key() == key {
			p.Procs = []model.Process{proc}
			p.Outside = proc.ProjectID == ""
		}
	}
	if p.Procs == nil {
		return engine.Plan{}, &engine.Refusal{Reason: "gone"}
	}
	if o.Tree {
		for i := 0; i < len(p.Procs); i++ {
			for _, c := range s.Processes {
				if c.PPID == p.Procs[i].PID && c.PID > 0 {
					p.Procs = append(p.Procs, c)
				}
			}
		}
	}
	p.Signal = syscall.SIGTERM
	if o.Force {
		p.Signal = syscall.SIGKILL
	}
	f.plans = append(f.plans, p)
	return p, nil
}

// fakeKiller records every plan it is given and answers with the next scripted result, or,
// once the script is used up, with every planned process signalled and exited.
type fakeKiller struct {
	plans    []engine.Plan
	timeouts []time.Duration
	results  []func(engine.Plan) (engine.Result, error)
}

func (f *fakeKiller) kill(p engine.Plan, timeout time.Duration) (engine.Result, error) {
	f.plans = append(f.plans, p)
	f.timeouts = append(f.timeouts, timeout)
	if len(f.results) > 0 {
		r := f.results[0]
		f.results = f.results[1:]
		return r(p)
	}
	return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true, Exited: true} }), nil
}

// outcomes builds a Result for p with one outcome per process from f.
func outcomes(p engine.Plan, f func(model.Process) engine.Outcome) engine.Result {
	var r engine.Result
	for _, proc := range p.Procs {
		o := f(proc)
		o.Process = proc
		r.Outcomes = append(r.Outcomes, o)
	}
	return r
}

// newKillTest returns a model at w by h fed with s, with fakePlanner and fakeKiller wired in.
func newKillTest(t *testing.T, w, h int, s model.Snapshot) (*Model, *fakeSource, *fakePlanner, *fakeKiller) {
	t.Helper()
	fp, fk := &fakePlanner{}, &fakeKiller{}
	m, src := newTest(t, w, h, func(o *Options) { o.Plan, o.Kill = fp.plan, fk.kill })
	feed(m, s)
	return m, src, fp, fk
}

// selectRow moves the selection down to the row with key k.
func selectRow(t *testing.T, m *Model, k model.RowKey) {
	t.Helper()
	for range 100 {
		if m.sel == k {
			return
		}
		press(m, "down")
	}
	t.Fatalf("row %+v not reachable", k)
}

// scroll sends key presses like press, plus "pgup" and "pgdown", which press does not know.
func scroll(m *Model, keys ...string) {
	for _, k := range keys {
		switch k {
		case "pgup":
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
		case "pgdown":
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		default:
			press(m, k)
		}
	}
}

// run executes cmd as the program would and delivers its message.
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to run")
	}
	m.Update(cmd())
}

func TestKillNeedsAProcess(t *testing.T) {
	m, _ := newTest(t, 80, 24) // its Plan and Kill fail the test when called
	press(m, "x")
	if m.kill.active() || !strings.Contains(screen(m), "select a process to kill") {
		t.Error("x with nothing selected opened the modal or said nothing")
	}
	feed(m, fixture())
	if r, _ := m.selected(); r.Key.Header == model.GroupNone {
		t.Fatalf("the first row is not a header: %+v", r.Key)
	}
	press(m, "x")
	if m.kill.active() || !strings.Contains(screen(m), "select a process to kill") {
		t.Error("x on a header opened the modal or said nothing")
	}
}

func TestKillProcessMode(t *testing.T) {
	s := fixture()
	m, _, fp, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 200))
	if cmd := press(m, "x"); cmd != nil {
		t.Error("opening the modal returned a command")
	}
	if !m.kill.active() {
		t.Fatal("x on a process did not open the modal")
	}
	if want := []planCall{{keyOf(s, 200), engine.KillOptions{}}}; !reflect.DeepEqual(fp.calls, want) {
		t.Errorf("Plan calls %+v, want %+v", fp.calls, want)
	}
	if got, want := line(m, "kill api"), "kill api (pid 200): process mode, SIGTERM to 1 process"; got != want {
		t.Errorf("title\n got %q\nwant %q", got, want)
	}
	if got := strings.Fields(line(m, "200 ")); !reflect.DeepEqual(got, []string{"200", "api", "api", "8080,8081"}) {
		t.Errorf("plan line %q, want pid, name, project and ports", got)
	}
	if line(m, "p process  t tree  f force  enter confirm  esc cancel") == "" {
		t.Errorf("no key hint line:\n%s", screen(m))
	}
	// Other keys are ignored while the modal is open: q does not quit, j does not move.
	sel := m.sel
	if cmd := press(m, "q"); cmd != nil {
		t.Error("q returned a command while the modal is open")
	}
	press(m, "j", "r")
	if !m.kill.active() || m.sel != sel || len(fk.plans) != 0 {
		t.Error("an unrelated key closed the modal, moved the selection or killed")
	}
}

func TestKillModes(t *testing.T) {
	s := fixture()
	m, _, fp, _ := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	if got := fp.calls[len(fp.calls)-1].opts; got != (engine.KillOptions{Tree: true}) {
		t.Errorf("t planned with %+v, want Tree", got)
	}
	if !strings.Contains(line(m, "kill vite (node)"), "tree mode, SIGTERM to 2 processes") {
		t.Errorf("tree title %q", line(m, "kill vite (node)"))
	}
	if got := strings.Fields(line(m, "102 ")); len(got) < 3 || got[1] != "esbuild" || got[2] != "shop" {
		t.Errorf("descendant line %q", got)
	}

	press(m, "f")
	if got := fp.calls[len(fp.calls)-1].opts; got != (engine.KillOptions{Tree: true, Force: true}) {
		t.Errorf("f planned with %+v, want Tree and Force", got)
	}
	if !strings.Contains(line(m, "kill vite (node)"), "tree mode, force, SIGKILL to 2 processes") {
		t.Errorf("force title %q", line(m, "kill vite (node)"))
	}

	press(m, "p")
	if got := fp.calls[len(fp.calls)-1].opts; got != (engine.KillOptions{Force: true}) {
		t.Errorf("p planned with %+v, want Force alone", got)
	}
	if !strings.Contains(line(m, "kill vite (node)"), "process mode, force, SIGKILL to 1 process") || line(m, "102 ") != "" {
		t.Errorf("process mode still shows the tree:\n%s", screen(m))
	}

	press(m, "f")
	if got := fp.calls[len(fp.calls)-1].opts; got != (engine.KillOptions{}) {
		t.Errorf("second f planned with %+v, want SIGTERM again", got)
	}
	if !strings.Contains(line(m, "kill vite (node)"), "process mode, SIGTERM") {
		t.Errorf("title after f f %q", line(m, "kill vite (node)"))
	}
}

// The modal names a process by its tool label (spec "Release 1.1", tool labels): in its title,
// the plan's lines and the report's.
func TestKillToolLabel(t *testing.T) {
	s := fixture()
	m, _, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true} }), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x")
	if got, want := line(m, "kill vite"), "kill vite (node) (pid 101): process mode, SIGTERM to 1 process"; got != want {
		t.Errorf("title\n got %q\nwant %q", got, want)
	}
	if got := line(m, "101 "); !strings.HasPrefix(strings.TrimSpace(got), "101  vite (node)  shop  5173") {
		t.Errorf("plan line %q, want pid, label, project and ports", got)
	}
	run(t, m, press(m, "enter"))
	if got := line(m, "101 "); !strings.HasPrefix(strings.TrimSpace(got), "101  vite (node)  still running") {
		t.Errorf("report line %q, want pid, label and outcome", got)
	}
}

func TestKillGroupNote(t *testing.T) {
	s := fixture()
	fp := &fakePlanner{}
	m, _ := newTest(t, 80, 24, func(o *Options) {
		o.Plan = func(s model.Snapshot, k model.RowKey, ko engine.KillOptions) (engine.Plan, error) {
			p, err := fp.plan(s, k, ko)
			if ko.Tree {
				p.Group = 101
			}
			return p, err
		}
	})
	feed(m, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x")
	if line(m, "process group") != "" {
		t.Error("process mode shows a process group note")
	}
	press(m, "t")
	if !strings.Contains(line(m, "process group"), "pid 101 leads its process group") {
		t.Errorf("no process group note:\n%s", screen(m))
	}
}

func TestKillCancel(t *testing.T) {
	s := fixture()
	m, _, _, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 200))
	press(m, "x", "t", "f")
	if cmd := press(m, "esc"); cmd != nil {
		t.Error("esc returned a command")
	}
	if m.kill.active() || len(fk.plans) != 0 {
		t.Fatalf("esc: modal open %v, %d kills", m.kill.active(), len(fk.plans))
	}
	if line(m, "kill api") != "" {
		t.Error("the modal is still drawn after esc")
	}
	// The keyboard is back to the table, and the next x starts over in process mode.
	press(m, "x")
	if !strings.Contains(line(m, "kill api"), "process mode, SIGTERM") {
		t.Errorf("reopened modal kept the old mode: %q", line(m, "kill api"))
	}
}

func TestKillConfirm(t *testing.T) {
	for _, confirm := range []string{"enter", "y"} {
		t.Run(confirm, func(t *testing.T) {
			s := fixture()
			m, src, fp, fk := newKillTest(t, 80, 24, s)
			selectRow(t, m, keyOf(s, 101))
			press(m, "x", "t")
			shown := fp.plans[len(fp.plans)-1]
			cmd := press(m, confirm)
			if len(fk.plans) != 0 {
				t.Fatal("Kill ran on the UI goroutine instead of in the command")
			}
			run(t, m, cmd)
			if len(fk.plans) != 1 || !reflect.DeepEqual(fk.plans[0], shown) {
				t.Fatalf("Kill got %+v, want exactly the shown plan %+v", fk.plans, shown)
			}
			if fk.timeouts[0] != engine.DefaultKillTimeout {
				t.Errorf("timeout %v, want %v", fk.timeouts[0], engine.DefaultKillTimeout)
			}
			if src.refreshes != 1 {
				t.Errorf("%d refreshes after the kill, want 1", src.refreshes)
			}
			if m.kill.active() {
				t.Error("modal still open after every process exited")
			}
			if !strings.Contains(screen(m), "killed 2 processes") {
				t.Errorf("no summary in the footer:\n%s", screen(m))
			}
		})
	}
}

func TestKillPlanIgnoresRefresh(t *testing.T) {
	s := fixture()
	m, _, fp, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	shown := fp.plans[len(fp.plans)-1]
	calls := len(fp.calls)

	// A new snapshot arrives while the modal is open: esbuild is gone and a new child appeared.
	s2 := fixture()
	s2.TakenAt = now
	for i, p := range s2.Processes {
		if p.PID == 102 {
			p.PID, p.Name = 104, "tsc"
			s2.Processes[i] = p
		}
	}
	feed(m, s2)
	if len(fp.calls) != calls {
		t.Error("a refresh re-planned the open modal")
	}
	if line(m, "102 ") == "" || line(m, "104 ") != "" {
		t.Errorf("the shown plan changed with the snapshot:\n%s", screen(m))
	}
	run(t, m, press(m, "enter"))
	if len(fk.plans) != 1 || !reflect.DeepEqual(fk.plans[0], shown) {
		t.Errorf("Kill got %+v, want the plan shown before the refresh %+v", fk.plans, shown)
	}
}

func TestKillRefusedContainer(t *testing.T) {
	s, hidden := fixture(), hiddenProxy()
	for _, tc := range []struct {
		name string
		snap model.Snapshot
		key  model.RowKey
		hint string
	}{
		{"container row", s, model.RowKey{ContainerID: "4e5d6c7b8a90"}, "docker stop shop-web-1"},
		{"proxy of a container port", s, keyOf(s, 300), "docker stop shop-db-1"},
		// DEV-89: the title names the container, not "unknown".
		{"unreadable owner of a container port", hidden, hidden.Processes[len(hidden.Processes)-1].Key(),
			"docker stop shop-db-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := &fakeKiller{}
			// The real NewPlan: it refuses container targets after only reading devdash's own
			// ancestry; it signals nothing.
			m, _ := newTest(t, 80, 24, func(o *Options) { o.Plan, o.Kill = engine.NewPlan, fk.kill })
			feed(m, tc.snap)
			selectRow(t, m, tc.key)
			press(m, "x")
			if !m.kill.active() {
				t.Fatal("a refused target did not open the modal")
			}
			if strings.Contains(screen(m), "cannot kill unknown") {
				t.Errorf("the title does not name the container:\n%s", screen(m))
			}
			if line(m, tc.hint) == "" {
				t.Errorf("no %q hint:\n%s", tc.hint, screen(m))
			}
			for _, k := range []string{"enter", "y", "p", "t", "f"} {
				if cmd := press(m, k); cmd != nil {
					t.Errorf("%s returned a command on a refused target", k)
				}
			}
			if !m.kill.active() || line(m, tc.hint) == "" || len(fk.plans) != 0 {
				t.Errorf("keys changed the refusal or killed (%d kills)", len(fk.plans))
			}
			press(m, "esc")
			if m.kill.active() {
				t.Error("esc did not close the refusal")
			}
		})
	}
}

func TestKillReplanRefused(t *testing.T) {
	s := fixture()
	m, _, fp, fk := newKillTest(t, 80, 24, s)
	fp.refuse = func(o engine.KillOptions) error {
		if o.Tree {
			return &engine.Refusal{Reason: "process group 101 contains pid 90, which runs devdash"}
		}
		return nil
	}
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	if line(m, "runs devdash") == "" {
		t.Fatalf("tree refusal not shown:\n%s", screen(m))
	}
	if cmd := press(m, "enter"); cmd != nil || len(fk.plans) != 0 {
		t.Fatal("confirm on a refused tree returned a command or killed")
	}
	if line(m, "enter confirm") != "" {
		t.Error("a refused plan still offers enter confirm")
	}
	press(m, "p")
	if line(m, "runs devdash") != "" || line(m, "101 ") == "" {
		t.Errorf("p did not go back to the process plan:\n%s", screen(m))
	}
	run(t, m, press(m, "enter"))
	if len(fk.plans) != 1 || len(fk.plans[0].Procs) != 1 {
		t.Errorf("Kill got %+v, want the process plan", fk.plans)
	}
}

func TestKillPlanError(t *testing.T) {
	s := fixture()
	m, _ := newTest(t, 80, 24, func(o *Options) {
		o.Plan = func(model.Snapshot, model.RowKey, engine.KillOptions) (engine.Plan, error) {
			return engine.Plan{}, errors.New("getpgid: no such process")
		}
	})
	feed(m, s)
	selectRow(t, m, keyOf(s, 200))
	press(m, "x")
	if m.kill.active() {
		t.Error("a plan error opened the modal")
	}
	if !strings.Contains(screen(m), "getpgid: no such process") {
		t.Errorf("plan error not in the footer:\n%s", screen(m))
	}
}

func TestKillOutsideConfirmsTwice(t *testing.T) {
	s, pg := withPostgres(fixture())
	m, _, _, fk := newKillTest(t, 80, 24, s)
	openOther(m) // postgres is in other
	selectRow(t, m, pg.Key())
	press(m, "x")
	if got := strings.Fields(line(m, "400 ")); !reflect.DeepEqual(got, []string{"400", "postgres", "-", "5433"}) {
		t.Errorf("plan line %q, want - for no project", got)
	}

	if cmd := press(m, "enter"); cmd != nil || len(fk.plans) != 0 {
		t.Fatal("the first confirm signalled a process outside every project")
	}
	if line(m, "postgres is outside every project") == "" || line(m, "Confirm again") == "" {
		t.Fatalf("no second confirmation:\n%s", screen(m))
	}
	// p, t and f do nothing at the second prompt; esc cancels everything.
	for _, k := range []string{"p", "t", "f"} {
		if cmd := press(m, k); cmd != nil {
			t.Errorf("%s returned a command at the second prompt", k)
		}
	}
	press(m, "esc")
	if m.kill.active() || len(fk.plans) != 0 {
		t.Fatal("esc at the second prompt did not cancel")
	}

	press(m, "x", "y")
	run(t, m, press(m, "Y"))
	if len(fk.plans) != 1 || fk.plans[0].Procs[0].PID != 400 {
		t.Errorf("Kill got %+v after two confirms, want postgres once", fk.plans)
	}
}

func TestKillSurvivors(t *testing.T) {
	s := fixture()
	m, src, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			return engine.Outcome{Signalled: true, Exited: proc.PID != 102}
		}), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	if !m.kill.active() {
		t.Fatal("modal closed although a process survived")
	}
	if src.refreshes != 1 {
		t.Errorf("%d refreshes after the kill, want 1", src.refreshes)
	}
	if got := strings.Fields(line(m, "102 ")); len(got) < 3 || got[1] != "esbuild" || !strings.Contains(line(m, "102 "), "still running") {
		t.Errorf("survivor line %q", line(m, "102 "))
	}
	if line(m, "101 ") != "" {
		t.Error("an exited process is listed as a survivor")
	}
	if line(m, "1 of 2 processes exited") == "" || line(m, "f force-kill survivors") == "" {
		t.Errorf("no survivor report or force offer:\n%s", screen(m))
	}

	// f force-kills the survivors only, with SIGKILL and no second confirmation.
	cmd := press(m, "f")
	if line(m, "signalling SIGKILL") == "" {
		t.Errorf("not signalling after f:\n%s", screen(m))
	}
	run(t, m, cmd)
	if len(fk.plans) != 2 {
		t.Fatalf("%d kills, want 2", len(fk.plans))
	}
	want := engine.Plan{Procs: []model.Process{s.Processes[2]}, Signal: syscall.SIGKILL}
	if !reflect.DeepEqual(fk.plans[1], want) {
		t.Errorf("force plan %+v, want only esbuild with SIGKILL %+v", fk.plans[1], want)
	}
	if m.kill.active() || !strings.Contains(screen(m), "killed 2 processes") { // both rounds (DEV-155)
		t.Errorf("after the force kill: modal open %v\n%s", m.kill.active(), screen(m))
	}
	if src.refreshes != 2 {
		t.Errorf("%d refreshes, want 2", src.refreshes)
	}
}

// TestKillForceKeepsUnsignalled: a process the first round did not signal is still running
// after the force round killed the survivors, so the report stays open and names it, with
// S when sudo can help; the force round signals the survivors only (DEV-165).
func TestKillForceKeepsUnsignalled(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		s := fixture()
		fp, fk := &fakePlanner{}, &fakeKiller{}
		m, _ := newTest(t, 80, 24, func(o *Options) { o.Plan, o.Kill, o.Sudo = fp.plan, fk.kill, sudo })
		feed(m, s)
		fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
			return outcomes(p, func(proc model.Process) engine.Outcome {
				if proc.PID == 101 {
					return engine.Outcome{Err: engine.ErrPermission}
				}
				return engine.Outcome{Signalled: true}
			}), nil
		})
		selectRow(t, m, keyOf(s, 101))
		press(m, "x", "t")
		run(t, m, press(m, "enter"))
		run(t, m, press(m, "f"))
		want := engine.Plan{Procs: []model.Process{s.Processes[2]}, Signal: syscall.SIGKILL}
		if len(fk.plans) != 2 || !reflect.DeepEqual(fk.plans[1], want) {
			t.Fatalf("sudo %v: kills %+v, want the force round to SIGKILL only esbuild", sudo, fk.plans)
		}
		if !m.kill.active() || strings.Contains(screen(m), "killed ") {
			t.Fatalf("sudo %v: the force round reads as a complete kill:\n%s", sudo, screen(m))
		}
		if !strings.Contains(line(m, "101 "), "permission denied, run with sudo") || line(m, "102 ") != "" {
			t.Errorf("sudo %v: report does not name only vite as still running:\n%s", sudo, screen(m))
		}
		if line(m, "1 of 2 processes exited") == "" || line(m, "force-kill") != "" {
			t.Errorf("sudo %v: report title or hints:\n%s", sudo, screen(m))
		}
		if got := line(m, "esc close, then S rerun with sudo") != ""; got != sudo {
			t.Errorf("sudo %v: S named %v:\n%s", sudo, got, screen(m))
		}
	}

	// A second force round counts each process once: esbuild survives SIGTERM and the first
	// SIGKILL, then exits.
	s := fixture()
	m, _, _, fk := newKillTest(t, 80, 24, s)
	survive := func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			return engine.Outcome{Signalled: true, Exited: proc.PID != 102}
		}), nil
	}
	fk.results = append(fk.results, survive, survive)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	run(t, m, press(m, "f"))
	if line(m, "1 of 2 processes exited") == "" || line(m, "101 ") != "" || line(m, "102 ") == "" {
		t.Errorf("after one force round:\n%s", screen(m))
	}
	run(t, m, press(m, "f"))
	if m.kill.active() || status(m) != "killed 2 processes" {
		t.Errorf("after two force rounds: modal open %v, status %q", m.kill.active(), status(m))
	}
}

func TestKillReportErrors(t *testing.T) {
	s := fixture()
	m, _, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			switch proc.PID {
			case 101:
				return engine.Outcome{Err: engine.ErrPermission}
			case 102:
				return engine.Outcome{Err: engine.ErrStartTime}
			}
			return engine.Outcome{Signalled: true, Exited: true}
		}), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	if !strings.Contains(line(m, "101 "), "permission denied, run with sudo") {
		t.Errorf("permission line %q", line(m, "101 "))
	}
	if !warned(t, m, "101 ", "permission denied, run with sudo") || warned(t, m, "102 ", "not signalled: pid reused") {
		t.Error("the permission hint, and only it, is in the warning colour (DEV-108)")
	}
	if !strings.Contains(line(m, "102 "), "not signalled: pid reused") {
		t.Errorf("start time line %q", line(m, "102 "))
	}
	if line(m, "force-kill") != "" {
		t.Error("force offered with no survivors")
	}
	if cmd := press(m, "f"); cmd != nil || len(fk.plans) != 1 {
		t.Error("f without survivors killed again")
	}
	if line(m, "esc close") == "" {
		t.Errorf("no esc hint:\n%s", screen(m))
	}
	press(m, "esc")
	if m.kill.active() {
		t.Error("esc did not close the report")
	}
}

func TestKillError(t *testing.T) {
	s := fixture()
	m, src, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(engine.Plan) (engine.Result, error) {
		return engine.Result{}, errors.New("kill: pid 90 not allowed")
	})
	selectRow(t, m, keyOf(s, 200))
	press(m, "x")
	run(t, m, press(m, "enter"))
	if !m.kill.active() || line(m, "nothing was signalled") == "" || line(m, "kill: pid 90 not allowed") == "" {
		t.Errorf("Kill error not shown in the modal:\n%s", screen(m))
	}
	if src.refreshes != 0 {
		t.Errorf("%d refreshes after a kill that signalled nothing, want 0", src.refreshes)
	}
}

func TestKillIgnoresKeysWhileSignalling(t *testing.T) {
	s := fixture()
	m, _, fp, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 200))
	press(m, "x")
	cmd := press(m, "enter") // not run yet: Kill is still "running"
	if line(m, "signalling SIGTERM, waiting up to 3s") == "" {
		t.Errorf("no signalling line:\n%s", screen(m))
	}
	calls := len(fp.calls)
	for _, k := range []string{"esc", "enter", "y", "p", "t", "f", "q", "x"} {
		if c := press(m, k); c != nil {
			t.Errorf("%s returned a command while signalling", k)
		}
	}
	if !m.kill.active() || len(fp.calls) != calls || len(fk.plans) != 0 {
		t.Fatal("a key closed the modal, re-planned or killed while signalling")
	}
	if c := press(m, "ctrl+c"); c == nil || c() != (tea.QuitMsg{}) {
		t.Error("ctrl+c does not quit while signalling")
	}
	run(t, m, cmd)
	if len(fk.plans) != 1 || m.kill.active() {
		t.Errorf("%d kills, modal open %v; want one kill and a closed modal", len(fk.plans), m.kill.active())
	}
	// A stray result with no kill running changes nothing.
	m.Update(killDoneMsg{})
	if m.kill.active() {
		t.Error("a stray result opened the modal")
	}
}

func TestKillViewFits(t *testing.T) {
	s := fixture()
	// A tree of 40 children under node, more than any 80x24 body holds.
	for i := range 40 {
		s.Processes = append(s.Processes, model.Process{PID: 1000 + i, PPID: 101, StartTime: at(time.Minute), Name: "worker",
			ProjectID: shopID, Kind: model.KindOther})
	}
	for _, size := range [][2]int{{80, 24}, {120, 40}, {60, 10}, {40, 5}, {200, 3}, {10, 1}} {
		m, _, _, _ := newKillTest(t, size[0], size[1], s)
		selectRow(t, m, keyOf(s, 101))
		press(m, "x", "t")
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) != size[1] {
			t.Errorf("%v: %d lines, want %d", size, len(lines), size[1])
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w != size[0] {
				t.Errorf("%v: line %d is %d cells wide, want %d", size, i, w, size[0])
			}
		}
		if size == [2]int{80, 24} {
			if line(m, "kill vite (node)") == "" || line(m, "of 42, ↑↓ to scroll") == "" || line(m, "enter confirm") == "" {
				t.Errorf("80x24 tree plan lacks the title, the position or the hint:\n%s", screen(m))
			}
		}
	}
}

func TestKillScroll(t *testing.T) {
	s := fixture()
	// node, esbuild and 38 workers: a 40-pid tree plan, more than an 80x24 body shows.
	for i := range 38 {
		s.Processes = append(s.Processes, model.Process{PID: 1000 + i, PPID: 102, StartTime: at(time.Minute),
			Name: "worker", ProjectID: shopID, Kind: model.KindOther})
	}
	m, _, fp, fk := newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	shown := fp.plans[len(fp.plans)-1]
	if len(shown.Procs) != 40 {
		t.Fatalf("plan has %d pids, want 40", len(shown.Procs))
	}
	if !strings.Contains(line(m, "kill vite (node)"), "SIGTERM to 40 processes") {
		t.Errorf("title %q lacks the total count", line(m, "kill vite (node)"))
	}
	// 24 lines: header, footer warning and hints leave 21; title and hint leave 19 (the
	// blank lines go first when the list does not fit): 18 pids and the position line.
	if line(m, "pids 1-18 of 40, ↑↓ to scroll") == "" {
		t.Fatalf("first page position missing:\n%s", screen(m))
	}
	if line(m, "101 ") == "" || line(m, "1015 ") == "" || line(m, "1016 ") != "" {
		t.Errorf("first page is not pids 1-18:\n%s", screen(m))
	}

	press(m, "down", "j")
	if line(m, "pids 3-20 of 40") == "" || line(m, "101 ") != "" || line(m, "1017 ") == "" {
		t.Errorf("down, j did not scroll by two:\n%s", screen(m))
	}
	press(m, "k")
	if line(m, "pids 2-19 of 40") == "" {
		t.Errorf("k did not scroll up:\n%s", screen(m))
	}
	scroll(m, "pgdown", "pgdown", "pgdown", "down")
	if line(m, "pids 23-40 of 40") == "" || line(m, "1037 ") == "" || line(m, "1020 ") == "" || line(m, "1019 ") != "" {
		t.Errorf("not at the end after paging past it:\n%s", screen(m))
	}
	press(m, "up")
	if line(m, "pids 22-39 of 40") == "" {
		t.Errorf("one up from the end:\n%s", screen(m))
	}
	scroll(m, "pgup", "pgup", "pgup")
	if line(m, "pids 1-18 of 40") == "" {
		t.Errorf("not at the top after paging past it:\n%s", screen(m))
	}
	scroll(m, "pgdown")
	if len(fp.calls) != 2 || !m.kill.active() {
		t.Fatal("scrolling re-planned or closed the modal")
	}

	// Confirming does not require scrolling to the end, and Kill gets the whole plan.
	run(t, m, press(m, "enter"))
	if len(fk.plans) != 1 || !reflect.DeepEqual(fk.plans[0], shown) {
		t.Errorf("Kill got %d plans, want exactly the 40-pid plan shown", len(fk.plans))
	}
}

func TestKillScrollResets(t *testing.T) {
	s := fixture()
	for i := range 38 {
		s.Processes = append(s.Processes, model.Process{PID: 1000 + i, PPID: 102, StartTime: at(time.Minute),
			Name: "worker", ProjectID: shopID, Kind: model.KindOther})
	}
	m, _, _, fk := newKillTest(t, 80, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true} }), nil
	})
	selectRow(t, m, keyOf(s, 101))
	scroll(m, "x", "t", "pgdown")
	press(m, "f") // a re-plan starts at the top
	if line(m, "pids 1-18 of 40") == "" {
		t.Errorf("re-plan kept the scroll:\n%s", screen(m))
	}
	scroll(m, "pgdown")
	run(t, m, press(m, "enter"))
	// Every pid survived: the report scrolls too, from the top.
	if line(m, "0 of 40 processes exited") == "" || !strings.Contains(line(m, "of 40, ↑↓ to scroll"), "pids 1-") {
		t.Fatalf("survivor report does not start at the top:\n%s", screen(m))
	}
	scroll(m, "pgdown", "pgdown", "pgdown")
	if !strings.Contains(line(m, "of 40, ↑↓ to scroll"), "-40 of 40") || line(m, "1037 ") == "" {
		t.Errorf("survivor report does not scroll to the end:\n%s", screen(m))
	}
	if line(m, "f force-kill survivors") == "" {
		t.Error("force offer lost while scrolling")
	}
}

// afterKill is s as taken d after the kill finished (the fixed clock: fakeKiller returns at
// now); a negative d is a snapshot started before it.
func afterKill(s model.Snapshot, d time.Duration) model.Snapshot {
	s.TakenAt = now.Add(d)
	return s
}

// without returns s without the processes with pids.
func without(s model.Snapshot, pids ...int) model.Snapshot {
	s.Processes = slices.DeleteFunc(slices.Clone(s.Processes), func(p model.Process) bool { return slices.Contains(pids, p.PID) })
	return s
}

// killed kills the fixture process with pid after keys (t for tree mode), with every planned
// process exiting, and returns the model with the modal closed.
func killed(t *testing.T, s model.Snapshot, pid int, keys ...string) *Model {
	t.Helper()
	m, _, _, _ := newKillTest(t, 120, 40, s)
	selectRow(t, m, keyOf(s, pid))
	press(m, append([]string{"x"}, keys...)...)
	run(t, m, press(m, "enter"))
	if m.kill.active() {
		t.Fatalf("the modal is still open:\n%s", screen(m))
	}
	return m
}

// status is the footer's status line, "" when there is none.
func status(m *Model) string {
	return line(m, "killed ")
}

// TestKillResultPorts: once every process a kill signalled exited, the first snapshot taken
// after the kill finished adds each of their ports, ascending: free, or the holders that still
// hold it.
func TestKillResultPorts(t *testing.T) {
	s := fixture()
	m := killed(t, s, 101, "t") // vite and esbuild
	if got, want := status(m), "killed 2 processes"; got != want {
		t.Fatalf("status %q, want %q", got, want)
	}
	// A snapshot that started before the kill finished may still list vite: not reported.
	feed(m, afterKill(without(s, 101, 102), -time.Millisecond))
	if got, want := status(m), "killed 2 processes"; got != want {
		t.Errorf("a snapshot from before the kill finished: %q, want %q", got, want)
	}
	feed(m, afterKill(without(s, 101, 102), time.Second))
	if got, want := status(m), "killed 2 processes · 5173 free"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
	// Reported once: a later snapshot changes nothing.
	feed(m, afterKill(s, 2*time.Second))
	if got, want := status(m), "killed 2 processes · 5173 free"; got != want {
		t.Errorf("a second snapshot: %q, want %q", got, want)
	}

	// A forked child holds the port, by its label and pid, and a container publishes it, by
	// its name; comma-joined in snapshot order.
	m = killed(t, s, 101)
	after := without(s, 101)
	child := model.Process{PID: 105, PPID: 1, StartTime: at(time.Hour), UID: 501, Name: "node", ProjectID: shopID,
		Argv: []string{"node", "node_modules/.bin/vite"}, Listeners: []model.Listener{lis("tcp6", "::", 5173)}}
	after.Processes = append(after.Processes, child)
	after.Containers = append(after.Containers, model.Container{ID: "c0ffee", Name: "web-dev", State: "running",
		Ports: []model.PortMapping{{HostPort: 5173, ContainerPort: 80, Proto: "tcp"}}})
	feed(m, afterKill(after, time.Second))
	if got, want := status(m), "killed 1 process · 5173 still held by vite (node) 105, web-dev"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}

	// Every port, ascending, whatever the listeners' order; the unknown owner by that name.
	s2 := fixture()
	s2.Processes[4].Listeners = []model.Listener{lis("tcp6", "::1", 8081), lis("tcp4", "127.0.0.1", 8080), lis("udp4", "0.0.0.0", 8125)}
	m = killed(t, s2, 200)
	after = without(s2, 200)
	after.Processes[len(after.Processes)-1].Listeners = append(after.Processes[len(after.Processes)-1].Listeners, lis("tcp4", "0.0.0.0", 8081))
	feed(m, afterKill(after, time.Second))
	if got, want := status(m), "killed 1 process · 8080 free · 8081 still held by unknown owner"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}

	// No port held: the status stays as it was.
	m = killed(t, s, 103)
	feed(m, afterKill(without(s, 103), time.Second))
	if got, want := status(m), "killed 1 process"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}

	// Only the processes the kill signalled count: one gone before the signal adds no port, and
	// a kill that signalled nothing reports none.
	withPort := fixture()
	withPort.Processes[2].Listeners = []model.Listener{lis("tcp4", "127.0.0.1", 5174)} // esbuild
	for _, c := range []struct {
		name      string
		signalled func(pid int) bool
		want      string
	}{
		{"vite signalled, esbuild gone", func(pid int) bool { return pid == 101 }, "killed 1 process, 1 already gone · 5173 free"},
		{"vite gone, esbuild signalled", func(pid int) bool { return pid == 102 }, "killed 1 process, 1 already gone · 5174 free"},
		{"nothing signalled", func(int) bool { return false }, "killed 0 processes, 2 already gone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, _, fk := newKillTest(t, 120, 40, withPort)
			fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
				return outcomes(p, func(proc model.Process) engine.Outcome {
					return engine.Outcome{Signalled: c.signalled(proc.PID), Exited: true}
				}), nil
			})
			selectRow(t, m, keyOf(withPort, 101))
			press(m, "x", "t")
			run(t, m, press(m, "enter"))
			feed(m, afterKill(without(withPort, 101, 102), time.Second))
			if got := status(m); got != c.want {
				t.Errorf("status %q, want %q", got, c.want)
			}
		})
	}

	// A force round is the same kill (DEV-155): vite exits on SIGTERM, esbuild survives and
	// f kills it; the status counts both and lists both ports.
	m, _, _, fk := newKillTest(t, 120, 40, withPort)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			return engine.Outcome{Signalled: true, Exited: proc.PID != 102}
		}), nil
	})
	selectRow(t, m, keyOf(withPort, 101))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	run(t, m, press(m, "f"))
	feed(m, afterKill(without(withPort, 101, 102), time.Second))
	if got, want := status(m), "killed 2 processes · 5173 free · 5174 free"; got != want {
		t.Errorf("after the force round: %q, want %q", got, want)
	}
}

// TestKillTableWide: the modal's columns are padded by display width, so a double-width name
// does not push its own columns right (DEV-156).
func TestKillTableWide(t *testing.T) {
	got := killTable([]string{"101\tvite (node)\tshop\t5173", "102\t服务器服务器\tshop\t-"})
	want := []string{"101  vite (node)   shop  5173", "102  服务器服务器  shop  -"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("killTable\n%q, want\n%q", got, want)
	}
}

// TestKillResultKeyClears: a key pressed before the first snapshot after the kill clears the
// status, as any key clears it, and the ports are not reported.
func TestKillResultKeyClears(t *testing.T) {
	s := fixture()
	m := killed(t, s, 101, "t")
	press(m, "down")
	feed(m, afterKill(without(s, 101, 102), time.Second))
	if got := status(m); got != "" || line(m, "5173 free") != "" {
		t.Errorf("status %q after a key:\n%s", got, screen(m))
	}

	// A kill that left survivors reports in the modal; its ports are not waited for.
	m, _, _, fk := newKillTest(t, 120, 40, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			return engine.Outcome{Signalled: true, Exited: proc.PID != 102}
		}), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	feed(m, afterKill(without(s, 101), time.Second))
	if !m.kill.active() || line(m, "5173") != "" {
		t.Errorf("survivors: modal open %v\n%s", m.kill.active(), screen(m))
	}
}

// withLongLabels is the fixture plus, in api, the Python worker 73827 with a child 73828,
// node server.js 69590 and a process with a wide name, 55555.
func withLongLabels() model.Snapshot {
	s := fixture()
	proc := func(pid, ppid int, name string, argv ...string) model.Process {
		return model.Process{PID: pid, PPID: ppid, StartTime: at(time.Minute), UID: 501, User: "me", Name: name,
			Argv: argv, Cwd: apiID, ProjectID: apiID, Kind: model.KindOther}
	}
	worker := proc(73827, 1, "Python", "/usr/bin/python3", "worker_with_a_very_long_script_name.py")
	worker.Listeners = []model.Listener{lis("tcp4", "127.0.0.1", 8001)} // keeps its own row, not folded with its child
	s.Processes = append(s.Processes, worker,
		proc(73828, 73827, "Python", "/usr/bin/python3", "-c", "from multiprocessing.spawn import spawn_main"),
		proc(69590, 1, "node", "node", "server.js"),
		proc(55555, 1, "サーバーサーバーサーバーサーバー", "サーバーサーバーサーバーサーバー"))
	return s
}

// TestKillTitleCut: a kill title wider than the screen cuts the label with `…`, measured in
// cells, so the pid, the mode, the signal and the count always show; below a few cells of
// label the line is cut at the edge instead (DEV-184).
func TestKillTitleCut(t *testing.T) {
	s := withLongLabels()
	for _, tc := range []struct {
		w    int
		pid  int
		keys []string
		want string
	}{
		{80, 73827, nil, "kill worker_with_a_very_long_sc… (pid 73827): process mode, SIGTERM to 1 process"},
		{80, 73827, []string{"t"}, "kill worker_with_a_very_long_scr… (pid 73827): tree mode, SIGTERM to 2 processes"},
		{80, 73827, []string{"t", "f"}, "kill worker_with_a_very_l… (pid 73827): tree mode, force, SIGKILL to 2 processes"},
		{60, 69590, nil, "kill server… (pid 69590): process mode, SIGTERM to 1 process"},
		{80, 69590, nil, "kill server.js (node) (pid 69590): process mode, SIGTERM to 1 process"}, // fits: whole
		{60, 55555, nil, "kill サーバ… (pid 55555): process mode, SIGTERM to 1 process"},             // cells, not runes
		// Too narrow for the label's few cells: the line is cut at the edge.
		{40, 73827, []string{"t", "f"}, "kill worke… (pid 73827): tree mode, forc"},
	} {
		m, _, _, _ := newKillTest(t, tc.w, 24, s)
		selectRow(t, m, keyOf(s, tc.pid))
		press(m, append([]string{"x"}, tc.keys...)...)
		if got := line(m, "kill "); got != tc.want {
			t.Errorf("%d columns, %d %v:\n got %q\nwant %q", tc.w, tc.pid, tc.keys, got, tc.want)
		}
	}
}

// TestKillTitleCutEveryStage: the outside, running, report and refused titles cut the label
// the same way (DEV-184).
func TestKillTitleCutEveryStage(t *testing.T) {
	s := withLongLabels()
	s.Processes[len(s.Processes)-4].ProjectID = "" // the worker, outside every project
	m, _, fp, fk := newKillTest(t, 70, 24, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true} }), nil
	})
	openOther(m)
	selectRow(t, m, keyOf(s, 73827))
	press(m, "x", "enter")
	want := "kill worker_with_a_ve… (pid 73827): process mode, SIGTERM to 1 process"
	if got := line(m, "kill "); got != want || m.kill.stage != killOutside {
		t.Errorf("outside title\n got %q\nwant %q", got, want)
	}
	cmd := press(m, "Y")
	if got := line(m, "kill "); got != want {
		t.Errorf("running title\n got %q\nwant %q", got, want)
	}
	run(t, m, cmd)
	if got, want := line(m, "kill "), "kill worker_with_a_v… (pid 73827): 0 of 1 process exited after SIGTERM"; got != want {
		t.Errorf("report title\n got %q\nwant %q", got, want)
	}

	press(m, "esc")
	fp.refuse = func(engine.KillOptions) error { return &engine.Refusal{Reason: "pid 73827 (Python) runs devdash"} }
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	press(m, "x")
	if got, want := line(m, "cannot kill"), "cannot kill worker_with_a_very_long_scr…"; got != want {
		t.Errorf("refused title\n got %q\nwant %q", got, want)
	}
}
