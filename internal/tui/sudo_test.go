package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// S reruns the dashboard under sudo (DEV-144): offered only when Options.Sudo says sudo can
// run and a warning or a kill result needs root, always behind a confirmation.

// sudoWarning is a permission warning sudo fixes, as a non-root collector reports it.
var sudoWarning = model.Warning{Code: "process_fields_unreadable", Count: 3,
	Hint: "processes of other users have unreadable fields; run with sudo to see them", Sudo: true}

// withWarnings is the fixture with ws as its only warnings.
func withWarnings(ws ...model.Warning) model.Snapshot {
	s := fixture()
	s.Warnings = ws
	return s
}

// newSudoTest is a model at 80x24 on s, with sudo available when sudo is true.
func newSudoTest(t *testing.T, s model.Snapshot, sudo bool) *Model {
	t.Helper()
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Sudo = sudo })
	feed(m, s)
	return m
}

const (
	sudoFooter  = "S rerun with sudo"
	sudoConfirm = "restarts as root"
)

func TestSudoConfirm(t *testing.T) {
	m := newSudoTest(t, withWarnings(sudoWarning), true)
	if !strings.Contains(screen(m), sudoFooter) {
		t.Errorf("footer does not offer S:\n%s", screen(m))
	}
	if cmd := press(m, "S"); cmd != nil {
		t.Error("S returned a command")
	}
	sc := screen(m)
	for _, want := range []string{sudoConfirm, "selection, filter and sort are reset", "sudo asks for your password", "y rerun with sudo"} {
		if !strings.Contains(sc, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, sc)
		}
	}
	if m.sudo.asked {
		t.Fatal("S alone asked for sudo")
	}
	cmd := press(m, "y")
	if cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Error("y did not quit")
	}
	if !m.sudo.asked {
		t.Error("y did not ask for sudo")
	}
}

// TestSudoSmall: in a short terminal the explanation gives way before the key line, which
// is the last thing dropped; where it fits, the prompt is drawn whole as before (DEV-184).
func TestSudoSmall(t *testing.T) {
	whole := strings.Join([]string{
		"rerun devdash with sudo",
		"",
		"  The dashboard quits and restarts as root under sudo, with the same flags;",
		"  sudo asks for your password on this terminal.",
		"  The selection, filter and sort are reset.",
		"",
		"  y rerun with sudo  any other key cancels",
	}, "\n")
	for _, tc := range []struct {
		w, h int
		want string
	}{
		{100, 12, whole},
		{80, 24, whole},
		// Body of 6 lines: the blank lines go first, then the explanation fits.
		{60, 10, strings.Join([]string{
			"rerun devdash with sudo",
			"  The dashboard quits and restarts as root under sudo, with",
			"  the same flags;",
			"  sudo asks for your password on this terminal.",
			"  The selection, filter and sort are reset.",
			"  y rerun with sudo  any other key cancels",
		}, "\n")},
		// Body of 4 lines: the explanation is cut from its end.
		{60, 8, strings.Join([]string{
			"rerun devdash with sudo",
			"  The dashboard quits and restarts as root under sudo, with",
			"  the same flags;",
			"  y rerun with sudo  any other key cancels",
		}, "\n")},
	} {
		m, _ := newTest(t, tc.w, tc.h, func(o *Options) { o.Sudo = true })
		feed(m, withWarnings(sudoWarning))
		press(m, "S")
		if sc := screen(m); !strings.Contains(sc, "\n"+tc.want+"\n") {
			t.Errorf("%dx%d:\n%s\nwant the prompt\n%s", tc.w, tc.h, sc, tc.want)
		}
	}
}

func TestSudoCancel(t *testing.T) {
	for _, k := range []string{"n", "Y", "esc", "enter", "q", "j", "S", "x"} {
		m := newSudoTest(t, withWarnings(sudoWarning), true)
		press(m, "down")
		before := screen(m)
		press(m, "S")
		if cmd := press(m, k); cmd != nil {
			t.Errorf("%s in the confirmation returned a command", k)
		}
		if m.sudo.asked || m.sudo.open {
			t.Errorf("%s: asked %v, open %v; want both false", k, m.sudo.asked, m.sudo.open)
		}
		if got := screen(m); got != before {
			t.Errorf("%s did not leave the table as it was:\n%s\nwant\n%s", k, got, before)
		}
	}
	m := newSudoTest(t, withWarnings(sudoWarning), true)
	press(m, "S")
	if cmd := press(m, "ctrl+c"); cmd == nil || cmd() != (tea.QuitMsg{}) || m.sudo.asked {
		t.Error("ctrl+c in the confirmation did not quit without sudo")
	}
	m = newSudoTest(t, withWarnings(sudoWarning), true)
	press(m, "S")
	if _, cmd := m.Update(repeat("y")); cmd != nil || m.sudo.asked || !m.sudo.open {
		t.Error("a held y confirmed")
	}
}

func TestSudoNotOffered(t *testing.T) {
	docker := model.Warning{Code: "docker_unreachable", Count: 1,
		Hint: "docker: permission denied on /var/run/docker.sock (add yourself to the docker group)"}
	root := model.Warning{Code: "process_fields_unreadable", Count: 2,
		Hint: "running as root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE to see them"}
	namespace := model.Warning{Code: "listener_owner_unreadable", Count: 1,
		Hint: "owner not visible from this pid namespace, or the socket is held by the kernel"}
	for _, tc := range []struct {
		name string
		s    model.Snapshot
		sudo bool
	}{
		{"elevation unavailable", withWarnings(sudoWarning), false},
		{"no warning", withWarnings(), true},
		{"docker permission only", withWarnings(docker), true},
		{"root without CAP_SYS_PTRACE", withWarnings(root), true},
		{"listener outside the pid namespace", withWarnings(namespace), true},
		{"the fixture's warning, not marked for sudo", fixture(), true},
	} {
		m := newSudoTest(t, tc.s, tc.sudo)
		before := screen(m)
		if strings.Contains(before, sudoFooter) {
			t.Errorf("%s: footer offers S:\n%s", tc.name, before)
		}
		if cmd := press(m, "S"); cmd != nil || m.sudo.open || screen(m) != before {
			t.Errorf("%s: S did something:\n%s", tc.name, screen(m))
		}
	}
}

// killDenied runs a kill of vite (pid 101) on m that ends with permission denied.
func killDenied(t *testing.T, sudo bool) *Model {
	t.Helper()
	s := fixture()
	fp, fk := &fakePlanner{}, &fakeKiller{}
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Plan, o.Kill, o.Sudo = fp.plan, fk.kill, sudo })
	feed(m, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(model.Process) engine.Outcome { return engine.Outcome{Err: engine.ErrPermission} }), nil
	})
	selectRow(t, m, keyOf(s, 101))
	press(m, "x")
	run(t, m, press(m, "enter"))
	return m
}

func TestSudoAfterKillPermission(t *testing.T) {
	m := killDenied(t, true)
	if !strings.Contains(line(m, "101 "), "permission denied, run with sudo") {
		t.Errorf("permission line %q", line(m, "101 "))
	}
	if line(m, "esc close, then S rerun with sudo") == "" {
		t.Errorf("kill result does not name S:\n%s", screen(m))
	}
	if press(m, "S"); m.sudo.open || !m.kill.active() {
		t.Error("S in the kill report opened the confirmation or closed the report")
	}
	press(m, "esc")
	if !strings.Contains(screen(m), sudoFooter) {
		t.Errorf("footer does not offer S after the kill:\n%s", screen(m))
	}
	press(m, "S")
	if !strings.Contains(screen(m), sudoConfirm) {
		t.Errorf("S after a denied kill did not confirm:\n%s", screen(m))
	}

	m = killDenied(t, false)
	if strings.Contains(screen(m), "S rerun") {
		t.Errorf("kill result names S without sudo:\n%s", screen(m))
	}
	press(m, "esc", "S")
	if m.sudo.open || strings.Contains(screen(m), sudoFooter) {
		t.Errorf("S offered without sudo:\n%s", screen(m))
	}
}

func TestSudoIgnoredInModals(t *testing.T) {
	m := newSudoTest(t, withWarnings(sudoWarning), true)
	press(m, "?", "S")
	if m.sudo.open || m.help {
		t.Errorf("S in the help overlay: confirmation %v, help %v; want it to only close help", m.sudo.open, m.help)
	}

	m = newSudoTest(t, withWarnings(sudoWarning), true)
	press(m, "/", "S")
	if m.sudo.open || m.filter != "S" {
		t.Errorf("S in the filter prompt: confirmation %v, filter %q; want it typed", m.sudo.open, m.filter)
	}

	s := withWarnings(sudoWarning)
	fp := &fakePlanner{}
	m, _ = newTest(t, 80, 24, func(o *Options) { o.Plan, o.Sudo = fp.plan, true })
	feed(m, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "S")
	if m.sudo.open || !m.kill.active() {
		t.Errorf("S in the kill modal: confirmation %v, modal %v", m.sudo.open, m.kill.active())
	}
}
