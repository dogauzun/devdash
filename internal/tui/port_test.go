package tui

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// fakeProbe is a scripted Prober: every port binds but those in taken, and err, when set, is
// the answer for the ports in failAt, or for every port when failAt is nil. It records the
// ports asked, so a test sees whether, and for what, the probe ran. No TUI test binds a socket.
type fakeProbe struct {
	taken  map[uint16]bool
	err    error
	failAt map[uint16]bool
	calls  []uint16
}

func (f *fakeProbe) probe(p uint16) (bool, error) {
	f.calls = append(f.calls, p)
	if f.err != nil && (f.failAt == nil || f.failAt[p]) {
		return false, f.err
	}
	return !f.taken[p], nil
}

// takeRange marks from..to taken.
func (f *fakeProbe) takeRange(from, to int) *fakeProbe {
	if f.taken == nil {
		f.taken = map[uint16]bool{}
	}
	for p := from; p <= to; p++ {
		f.taken[uint16(p)] = true
	}
	return f
}

// newPortTest returns a model at w by h showing s, with fp as its probe. The source's channel
// is closed, so running the command an update returns ends its wait at once (closedMsg, which
// runAll drops) instead of blocking.
func newPortTest(t *testing.T, w, h int, s model.Snapshot, fp *fakeProbe) *Model {
	t.Helper()
	m, src := newTest(t, w, h, func(o *Options) { o.Probe = fp.probe })
	feed(m, s)
	close(src.ch)
	return m
}

// runAll executes cmd as the program would, synchronously: each message goes back through Update
// and the command Update returns runs in turn; a batch runs its commands in order. closedMsg is
// dropped, so runAll is only given an update's command when the source's channel is closed.
func runAll(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runAll(m, c)
		}
	case closedMsg, nil:
	default:
		_, next := m.Update(msg)
		runAll(m, next)
	}
}

// search types q in the table, key by key, and runs every command a key returns, so the probe
// answers each query as it is typed.
func search(m *Model, q string) {
	for _, r := range q {
		_, cmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		runAll(m, cmd)
	}
}

// portLine is the screen line under the header (line 2 of the screen).
func portLine(m *Model) string {
	return strings.Split(screen(m), "\n")[1]
}

// withProcess adds a process with a listener on port to s.
func withProcess(s model.Snapshot, pid int, project string, kind model.Kind, port uint16) model.Snapshot {
	p := model.Process{PID: pid, PPID: 90, StartTime: at(time.Hour), UID: 501, User: "me", Name: "srv",
		Argv: []string{"srv"}, Cwd: project, Kind: kind, ProjectID: project}
	if port != 0 {
		p.Listeners = []model.Listener{lis("tcp4", "127.0.0.1", port)}
	}
	s.Processes = append(s.Processes, p)
	return s
}

func TestPortQuery(t *testing.T) {
	for q, want := range map[string]uint16{
		"5173": 5173, "1": 1, "80": 80, "65535": 65535,
		"": 0, "0": 0, "05173": 0, "65536": 0, "99999999999999999999": 0, "5173a": 0, "-1": 0, "+80": 0,
		" 80": 0, "８０": 0, "vite": 0,
	} {
		if got := portQuery(q); got != want {
			t.Errorf("portQuery(%q) = %d, want %d", q, got, want)
		}
	}
}

// TestPortDigitKey: a digit in the table does what / and then the digit do, with the detail
// pane open too; modals, the help overlay and the prompt take it as they take any key.
func TestPortDigitKey(t *testing.T) {
	fp := &fakeProbe{}
	m := newPortTest(t, 80, 24, fixture(), fp)
	cmd := press(m, "5")
	if !m.filtering || m.filter != "5" || !strings.HasSuffix(line(m, "mbp ·"), " · /5_") {
		t.Fatalf("5 did not open the prompt with 5 typed (filtering %v, filter %q):\n%s", m.filtering, m.filter, screen(m))
	}
	if cmd == nil || len(fp.calls) != 0 {
		t.Errorf("the key probed on the UI goroutine (calls %v) or returned no command", fp.calls)
	}
	typeText(m, "173")
	if m.filter != "5173" {
		t.Errorf("digits in the prompt typed %q", m.filter)
	}

	// An applied query is kept, as / keeps it.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	press(m, "/")
	typeText(m, "vite")
	press(m, "enter", "5")
	if !m.filtering || m.filter != "vite5" {
		t.Errorf("after an applied filter: filtering %v, filter %q", m.filtering, m.filter)
	}

	// The detail pane may be open, and stays open.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	press(m, "enter", "8")
	if !m.filtering || m.filter != "8" || !m.detail {
		t.Errorf("with the detail pane: filtering %v, filter %q, detail %v", m.filtering, m.filter, m.detail)
	}

	// The help overlay closes on the digit and nothing else happens.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	press(m, "?", "5")
	if m.help || m.filtering || m.filter != "" {
		t.Errorf("with help: help %v, filtering %v, filter %q", m.help, m.filtering, m.filter)
	}

	// The kill modal ignores it.
	s := fixture()
	m, _, _, _ = newKillTest(t, 80, 24, s)
	selectRow(t, m, keyOf(s, 101))
	press(m, "x", "5")
	if !m.kill.active() || m.filtering || m.filter != "" {
		t.Errorf("with the kill modal: kill %v, filtering %v, filter %q", m.kill.active(), m.filtering, m.filter)
	}

	// Alt+5 is not a digit.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	m.Update(tea.KeyPressMsg{Code: '5', Text: "5", Mod: tea.ModAlt})
	if m.filtering || m.filter != "" {
		t.Errorf("alt+5: filtering %v, filter %q", m.filtering, m.filter)
	}
}

// TestPortSelectsHolder: a query that becomes a port number selects its exact holder, in a
// collapsed group and under a hidden shell; clearing the filter brings back the row chosen
// before and the fold.
func TestPortSelectsHolder(t *testing.T) {
	s := fixture()
	m := newPortTest(t, 80, 24, s, &fakeProbe{})
	shop := model.RowKey{Header: model.GroupProject, Group: shopID}
	selectRow(t, m, shop)
	press(m, "left")
	if !strings.Contains(line(m, "shop @"), "▸ shop") {
		t.Fatalf("shop did not collapse:\n%s", screen(m))
	}
	search(m, "5173")
	if m.sel != keyOf(s, 101) {
		t.Fatalf("5173 selected %+v, want vite 101:\n%s", m.sel, screen(m))
	}
	if l := line(m, "*5173"); !strings.Contains(l, "node") {
		t.Errorf("vite not shown: %q", l)
	}
	press(m, "esc")
	if m.sel != shop || m.filter != "" || !strings.Contains(line(m, "shop @"), "▸ shop") {
		t.Errorf("after esc: selected %+v, filter %q:\n%s", m.sel, m.filter, screen(m))
	}

	// A prefix shows its matches but keeps the selection rules; the exact holder wins once typed.
	m = newPortTest(t, 80, 24, s, &fakeProbe{})
	before := m.sel
	search(m, "80")
	if l := line(m, "8080"); l == "" || line(m, "*8000") == "" {
		t.Errorf("80 does not list 8000 and 8080:\n%s", screen(m))
	}
	if m.sel != before {
		t.Errorf("80, held by no row, moved the selection to %+v", m.sel)
	}
	search(m, "80")
	if m.sel != keyOf(s, 200) {
		t.Errorf("8080 selected %+v, want api 200", m.sel)
	}
	press(m, "backspace") // 808: a port number no row holds; back to the row chosen before
	if m.sel != before {
		t.Errorf("808 selected %+v, want %+v", m.sel, before)
	}

	// A container publishing the port with no process behind it, and the unknown owner.
	m = newPortTest(t, 80, 24, s, &fakeProbe{})
	search(m, "8000")
	if r, ok := m.selected(); !ok || r.Container == nil || r.Container.Name != "shop-web-1" {
		t.Errorf("8000 selected %+v", m.sel)
	}
	// Both are in the other group, which starts collapsed.
	m = newPortTest(t, 80, 24, s, &fakeProbe{})
	if !strings.Contains(line(m, "other ·"), "▸ other") {
		t.Fatalf("other does not start collapsed:\n%s", screen(m))
	}
	search(m, "631")
	if r, ok := m.selected(); !ok || r.Process == nil || r.Process.PID != 0 {
		t.Errorf("631 selected %+v", m.sel)
	}
	m = newPortTest(t, 80, 24, s, &fakeProbe{})
	search(m, "22")
	if m.sel != keyOf(s, 1) || portLine(m) != "port 22 · 1 holder · next free 23" {
		t.Errorf("22 selected %+v, line %q", m.sel, portLine(m))
	}
	press(m, "esc")
	if !strings.Contains(line(m, "other ·"), "▸ other") {
		t.Errorf("other is not collapsed again after esc:\n%s", screen(m))
	}

	// Two holders: the first in display order (api's group comes first).
	s2 := withProcess(fixture(), 204, apiID, model.KindServer, 5173)
	m = newPortTest(t, 80, 24, s2, &fakeProbe{})
	search(m, "5173")
	if m.sel != keyOf(s2, 204) {
		t.Errorf("two holders: selected %+v, want 204", m.sel)
	}
}

// TestPortRefreshKeepsSelection: a refresh never moves the selection to the holder, so a user
// who moved off it stays where they moved.
func TestPortRefreshKeepsSelection(t *testing.T) {
	s := fixture()
	m := newPortTest(t, 80, 24, s, &fakeProbe{})
	search(m, "5173")
	press(m, "enter", "up")
	if m.sel != shopHeader { // zsh is folded into the holder's row (DEV-157)
		t.Fatalf("up selected %+v, want the shop header", m.sel)
	}
	_, cmd := m.Update(updateMsg(engine.Update{Snapshot: fixture(), Interval: 2 * time.Second}))
	runAll(m, cmd)
	if m.sel != shopHeader {
		t.Errorf("a refresh moved the selection to %+v", m.sel)
	}
}

// TestPortLine: every case of the spec's port line table.
func TestPortLine(t *testing.T) {
	errMFILE := errors.New("socket: too many open files")
	at65535 := withProcess(fixture(), 204, apiID, model.KindServer, 65535)
	for _, c := range []struct {
		name  string
		s     model.Snapshot
		probe *fakeProbe
		query string
		want  string
		calls int // probe calls, -1 for any
	}{
		{"holder, next free", fixture(), &fakeProbe{}, "5173", "port 5173 · 1 holder · next free 5174", 1},
		{"holders, next free past taken", withProcess(fixture(), 204, apiID, model.KindServer, 5173),
			(&fakeProbe{}).takeRange(5174, 5175), "5173", "port 5173 · 2 holders · next free 5176", 3},
		{"holder, a snapshot port is skipped", withProcess(fixture(), 204, apiID, model.KindServer, 5174),
			&fakeProbe{}, "5173", "port 5173 · 1 holder · next free 5175", 1},
		{"holder, nothing free", fixture(), (&fakeProbe{}).takeRange(5174, 5273), "5173",
			"port 5173 · 1 holder · no free port in 5174-5273", 100},
		{"free", fixture(), &fakeProbe{}, "3000", "port 3000 · free", 1},
		{"bind refused", fixture(), (&fakeProbe{}).takeRange(3000, 3000), "3000",
			"port 3000 · next free 3001 · bind refused", 2},
		{"bind refused, nothing free", fixture(), (&fakeProbe{}).takeRange(3000, 3100), "3000",
			"port 3000 · no free port in 3001-3100 · bind refused", 101},
		{"probe failed", fixture(), &fakeProbe{err: errMFILE}, "5173",
			"port 5173 · 1 holder · next free: socket: too many open files", 1},
		{"probe failed, no holder", fixture(), &fakeProbe{err: errMFILE}, "3000",
			"port 3000 · next free: socket: too many open files", 1},
		{"bind refused, then the probe failed", fixture(),
			&fakeProbe{taken: map[uint16]bool{3000: true}, err: errMFILE, failAt: map[uint16]bool{3001: true}}, "3000",
			"port 3000 · next free: socket: too many open files · bind refused", 2},
		{"65535 held", at65535, &fakeProbe{}, "65535", "port 65535 · 1 holder", 0},
		{"65535 free", fixture(), &fakeProbe{}, "65535", "port 65535 · free", 1},
		{"65535 refused", fixture(), (&fakeProbe{}).takeRange(65535, 65535), "65535", "port 65535 · bind refused", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newPortTest(t, 120, 40, c.s, c.probe)
			typeText(m, c.query[:len(c.query)-1]) // the prefixes' probe commands are not run
			search(m, c.query[len(c.query)-1:])
			if got := portLine(m); got != c.want {
				t.Errorf("port line %q, want %q\n%s", got, c.want, screen(m))
			}
			if c.calls >= 0 && len(c.probe.calls) != c.calls {
				t.Errorf("%d probe calls, want %d: %v", len(c.probe.calls), c.calls, c.probe.calls)
			}
			if c.probe.err != nil && !warned(t, m, "port "+c.query, "next free: "+c.probe.err.Error()) {
				t.Errorf("the error is not in the warning colour:\n%q", styled(t, m, colorprofile.ANSI))
			}
		})
	}
}

// TestPortLineBeforeAnswer: until the probe answers, the line says what the snapshot says.
func TestPortLineBeforeAnswer(t *testing.T) {
	fp := &fakeProbe{}
	m := newPortTest(t, 80, 24, fixture(), fp)
	typeText(m, "5173")
	if got, want := portLine(m), "port 5173 · 1 holder"; got != want {
		t.Errorf("held: %q, want %q", got, want)
	}
	press(m, "esc")
	typeText(m, "3000")
	if got, want := portLine(m), "port 3000"; got != want {
		t.Errorf("not held: %q, want %q", got, want)
	}
	if len(fp.calls) != 0 {
		t.Errorf("probed without a command run: %v", fp.calls)
	}
}

// TestPortHoldersTCPOnly: a UDP listener or a container publishing the port over UDP only does
// not hold it, as freeport.Find, port N and kill N count, so N itself is probed and the row is
// not selected.
func TestPortHoldersTCPOnly(t *testing.T) {
	s := fixture()
	s.Containers = append(s.Containers, model.Container{ID: "a1b2c3d4e5f6", Name: "statsd-1", Image: "statsd:1",
		State: "running", Ports: []model.PortMapping{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 8125, ContainerPort: 8125, Proto: "udp"}}})
	s = withProcess(s, 204, apiID, model.KindServer, 0)
	s.Processes[len(s.Processes)-1].Listeners = []model.Listener{lis("udp4", "0.0.0.0", 8126)}
	for q, want := range map[string]string{"8125": "port 8125 · free", "8126": "port 8126 · free"} {
		fp := &fakeProbe{}
		m := newPortTest(t, 80, 24, s, fp)
		typeText(m, q[:len(q)-1])
		search(m, q[len(q)-1:])
		if got := portLine(m); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
		if len(fp.calls) != 1 || fp.calls[0] != portQuery(q) {
			t.Errorf("%s: probe calls %v, want the port itself", q, fp.calls)
		}
		if m.sel.ContainerID == "a1b2c3d4e5f6" || m.sel.PID == 204 {
			t.Errorf("%s selected the UDP row %+v", q, m.sel)
		}
	}
}

// TestPortHolders: holders are counted with everything shown, whatever the view toggles, the
// folds and the filter show.
func TestPortHolders(t *testing.T) {
	// An editor, hidden without a, listens on 9000: it holds the port, and the search shows
	// and selects its row, so the count is the rows the search can select.
	s := withProcess(fixture(), 204, apiID, model.KindEditor, 9000)
	m := newPortTest(t, 80, 24, s, &fakeProbe{})
	search(m, "9000")
	if got, want := portLine(m), "port 9000 · 1 holder · next free 9001"; got != want {
		t.Errorf("hidden editor: %q, want %q", got, want)
	}
	if m.sel != keyOf(s, 204) {
		t.Errorf("hidden editor not selected: %+v\n%s", m.sel, screen(m))
	}

	// Containers hidden with d still hold their ports, and the search shows them.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	press(m, "d")
	search(m, "8000")
	if got, want := portLine(m), "port 8000 · 1 holder · next free 8001"; got != want {
		t.Errorf("containers hidden: %q, want %q", got, want)
	}
	if r, ok := m.selected(); !ok || r.Container == nil || r.Container.Name != "shop-web-1" {
		t.Errorf("containers hidden: 8000 selected %+v", m.sel)
	}

	// The unknown owner and a docker-proxy holding a container's port are one row each.
	for q, want := range map[string]string{
		"631":  "port 631 · 1 holder · next free 632",
		"5432": "port 5432 · 1 holder · next free 5433",
		"22":   "port 22 · 1 holder · next free 23",
	} {
		m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
		search(m, q)
		if got := portLine(m); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}

// TestPortHoldersForwarder: a forwarder that keeps its own row (several containers' ports, or
// one next to a port of its own) does not hold a container's published port; the container
// row does, once, and the search selects it, as model.Holders counts (DEV-154).
func TestPortHoldersForwarder(t *testing.T) {
	published := func(id, name string, port uint16) model.Container {
		return model.Container{ID: id, Name: name, Image: "nginx", State: "running",
			Ports: []model.PortMapping{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: port, ContainerPort: 80, Proto: "tcp"}}}
	}
	forwarder := func(name string, ports ...uint16) model.Process {
		p := model.Process{PID: 400, PPID: 1, StartTime: at(time.Hour), UID: 501, Name: name, Argv: []string{name}}
		for _, port := range ports {
			p.Listeners = append(p.Listeners, lis("tcp6", "::", port))
		}
		return p
	}
	backend := fixture() // Docker Desktop with two containers publishing ports
	backend.Containers = append(backend.Containers, published("aaa", "db", 6000), published("bbb", "cache", 6001))
	backend.Processes = model.Reconcile(append(backend.Processes, forwarder("com.docker.backend", 6000, 6001)), backend.Containers)
	orb := fixture() // OrbStack: one container, next to the helper's own 32222
	orb.Containers = append(orb.Containers, published("aaa", "web", 6000))
	orb.Processes = model.Reconcile(append(orb.Processes, forwarder("OrbStack Helper", 6000, 32222)), orb.Containers)

	for _, c := range []struct {
		name string
		s    model.Snapshot
		q    string
		want string
		sel  model.RowKey
	}{
		{"backend", backend, "6000", "port 6000 · 1 holder · next free 6002", model.RowKey{ContainerID: "aaa"}},
		{"orbstack", orb, "6000", "port 6000 · 1 holder · next free 6001", model.RowKey{ContainerID: "aaa"}},
		{"orbstack own port", orb, "32222", "port 32222 · 1 holder · next free 32223", orb.Processes[len(orb.Processes)-1].Key()},
		{"forwarder drawn as the container", fixture(), "5432", "port 5432 · 1 holder · next free 5433", keyOf(fixture(), 300)},
	} {
		m := newPortTest(t, 120, 40, c.s, &fakeProbe{})
		search(m, c.q)
		if got := portLine(m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if m.sel != c.sel {
			t.Errorf("%s: selected %+v, want %+v", c.name, m.sel, c.sel)
		}
		if hs := model.Holders(c.s, portQuery(c.q)); len(hs) != 1 {
			t.Errorf("%s: model.Holders %+v, want one", c.name, hs)
		}
	}
}

// TestPortLineShown: the line is shown while the query, typed or applied, is a port number,
// under the header, cut at the screen edge; the body is one line shorter.
func TestPortLineShown(t *testing.T) {
	m := newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	search(m, "5173")
	lines := strings.Split(screen(m), "\n")
	if len(lines) != 24 || !strings.HasPrefix(lines[0], "mbp · ") || lines[1] != "port 5173 · 1 holder · next free 5174" ||
		!strings.HasPrefix(lines[2], "NAME") {
		t.Errorf("layout:\n%s", screen(m))
	}
	press(m, "enter")
	if got := portLine(m); got != "port 5173 · 1 holder · next free 5174" {
		t.Errorf("applied: %q", got)
	}
	press(m, "esc")
	if !strings.HasPrefix(portLine(m), "NAME") {
		t.Errorf("after esc the line stays:\n%s", screen(m))
	}

	// Not a port number: no line.
	for _, q := range []string{"vite", "05173", "65536", "0"} {
		m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
		press(m, "/")
		typeText(m, q)
		if strings.HasPrefix(portLine(m), "port ") {
			t.Errorf("%q shows a port line:\n%s", q, screen(m))
		}
	}

	// ctrl+u in the prompt clears it.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	search(m, "5173")
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if strings.HasPrefix(portLine(m), "port ") {
		t.Errorf("ctrl+u kept the line:\n%s", screen(m))
	}

	// A pasted port number answers too.
	fp := &fakeProbe{}
	m = newPortTest(t, 80, 24, fixture(), fp)
	press(m, "/")
	_, cmd := m.Update(tea.PasteMsg{Content: "5173"})
	runAll(m, cmd)
	if got := portLine(m); got != "port 5173 · 1 holder · next free 5174" || m.sel != keyOf(fixture(), 101) {
		t.Errorf("paste: %q, selected %+v", got, m.sel)
	}

	// No match: the line is the answer, the table never says nothing to show.
	m = newPortTest(t, 80, 24, fixture(), &fakeProbe{})
	search(m, "3000")
	if strings.Contains(screen(m), "nothing to show") || !strings.HasPrefix(strings.Split(screen(m), "\n")[2], "NAME") {
		t.Errorf("3000:\n%s", screen(m))
	}

	// Cut at the screen edge.
	m = newPortTest(t, 30, 24, fixture(), &fakeProbe{})
	search(m, "5173")
	if got, want := portLine(m), "port 5173 · 1 holder · next f…"; got != want {
		t.Errorf("at 30 columns %q, want %q", got, want)
	}
	if n := ansi.StringWidth(portLine(m)); n > 30 {
		t.Errorf("%d cells at 30 columns", n)
	}
}

// TestPortProbeRuns: the probe runs in a command when the query changes to a port number and
// on each new snapshot while it is one; an answer for an older query or snapshot is dropped,
// and the last answer stays until the next one arrives.
// overlapProbe is a Prober that notices two calls in flight at once: each call waits up to
// 50 ms for another to start before it answers free. Every port binds.
type overlapProbe struct {
	in      atomic.Int32
	overlap atomic.Bool
	calls   atomic.Int32
}

func (o *overlapProbe) probe(uint16) (bool, error) {
	o.calls.Add(1)
	if o.in.Add(1) > 1 {
		o.overlap.Store(true)
	}
	for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline) && !o.overlap.Load(); {
		time.Sleep(time.Millisecond)
	}
	o.in.Add(-1)
	return true, nil
}

// runConcurrently runs cmd as the program does, each command of a batch on its own goroutine,
// and returns the messages once every command has finished.
func runConcurrently(cmd tea.Cmd) []tea.Msg {
	var (
		mu    sync.Mutex
		msgs  []tea.Msg
		wg    sync.WaitGroup
		start func(tea.Cmd)
	)
	start = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Go(func() {
			msg := c()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, c := range b {
					start(c)
				}
				return
			}
			mu.Lock()
			msgs = append(msgs, msg)
			mu.Unlock()
		})
	}
	start(cmd)
	wg.Wait()
	return msgs
}

// TestProbeSerialized: the port line's and the detail pane's searches run concurrently, but the
// probe never binds for both at once (on macOS two binds of one port collide, so one search
// would skip a free port or read it as refused).
func TestProbeSerialized(t *testing.T) {
	op := &overlapProbe{}
	m, src := newTest(t, 100, 40, func(o *Options) { o.Probe = op.probe })
	feed(m, fixture())
	close(src.ch)
	search(m, "5173")
	press(m, "enter")            // apply the query; vite is selected
	runAll(m, press(m, "enter")) // open the pane on it
	if !m.detail || m.sel != keyOf(fixture(), 101) {
		t.Fatalf("pane open %v on %+v", m.detail, m.sel)
	}
	before := op.calls.Load()
	s := fixture()
	s.TakenAt = now
	_, cmd := m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 2 * time.Second}))
	for _, msg := range runConcurrently(cmd) {
		if _, closed := msg.(closedMsg); !closed {
			m.Update(msg)
		}
	}
	if n := op.calls.Load() - before; n != 2 {
		t.Fatalf("%d probe calls on the snapshot, want 2 (the port line's and the pane's)", n)
	}
	if op.overlap.Load() {
		t.Error("two probe calls were in flight at once")
	}
	if got := portLine(m); got != "port 5173 · 1 holder · next free 5174" {
		t.Errorf("port line %q", got)
	}
	hasLine(t, m, "next free 5174")
}

func TestPortProbeRuns(t *testing.T) {
	fp := &fakeProbe{}
	m := newPortTest(t, 80, 24, fixture(), fp)
	typeText(m, "517")
	stale := press(m, "3") // the answer for this snapshot: 5174
	newer := withProcess(fixture(), 204, apiID, model.KindServer, 5174)
	_, cmd := m.Update(updateMsg(engine.Update{Snapshot: newer, Interval: 2 * time.Second}))
	runAll(m, cmd)
	if got, want := portLine(m), "port 5173 · 1 holder · next free 5175"; got != want {
		t.Errorf("after the new snapshot: %q, want %q", got, want)
	}
	runAll(m, stale)
	if got, want := portLine(m), "port 5173 · 1 holder · next free 5175"; got != want {
		t.Errorf("the older snapshot's answer was not dropped: %q", got)
	}

	// The last answer stays while the next snapshot's is computed.
	_, pending := m.Update(updateMsg(engine.Update{Snapshot: fixture(), Interval: 2 * time.Second}))
	if got, want := portLine(m), "port 5173 · 1 holder · next free 5175"; got != want {
		t.Errorf("before the new answer: %q, want %q", got, want)
	}
	runAll(m, pending)
	if got, want := portLine(m), "port 5173 · 1 holder · next free 5174"; got != want {
		t.Errorf("after the new answer: %q, want %q", got, want)
	}

	// An answer for an older query is dropped, even when the query comes back to it.
	m = newPortTest(t, 80, 24, fixture(), fp)
	typeText(m, "517")
	old := press(m, "3")
	press(m, "backspace")
	again := press(m, "3")
	runAll(m, old)
	if got, want := portLine(m), "port 5173 · 1 holder"; got != want {
		t.Errorf("an older query's answer was kept: %q", got)
	}
	runAll(m, again)
	if got, want := portLine(m), "port 5173 · 1 holder · next free 5174"; got != want {
		t.Errorf("the current query's answer: %q, want %q", got, want)
	}

	// Not a port number: no probe on a key or a snapshot.
	fp = &fakeProbe{}
	m = newPortTest(t, 80, 24, fixture(), fp)
	press(m, "/")
	if cmd := press(m, "v"); cmd != nil {
		t.Error("a non-port query returned a command")
	}
	_, cmd = m.Update(updateMsg(engine.Update{Snapshot: fixture(), Interval: 2 * time.Second}))
	runAll(m, cmd)
	if len(fp.calls) != 0 {
		t.Errorf("probed for a non-port query: %v", fp.calls)
	}
}
