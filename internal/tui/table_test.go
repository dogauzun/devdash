package tui

import (
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// rawLine returns the first line of the styled view whose stripped text contains s.
func rawLine(m *Model, s string) string {
	for l := range strings.SplitSeq(m.View().Content, "\n") {
		if strings.Contains(ansi.Strip(l), s) {
			return l
		}
	}
	return ""
}

// reverse starts a styleSel line.
const reverse = "\x1b[7"

// selLine returns the text of the line drawn in reverse video, or "".
func selLine(m *Model) string {
	for l := range strings.SplitSeq(m.View().Content, "\n") {
		if strings.HasPrefix(l, reverse) {
			return ansi.Strip(l)
		}
	}
	return ""
}

// titles returns the table's column-title line.
func titles(m *Model) string { return line(m, "PORTS") }

// rowKeys returns the keys of the model's rows, in order.
func rowKeys(m *Model) []model.RowKey {
	var ks []model.RowKey
	for _, r := range m.rows {
		ks = append(ks, r.Key)
	}
	return ks
}

// selectKey moves the selection to the row with key k.
func selectKey(t *testing.T, m *Model, k model.RowKey) {
	t.Helper()
	for i, r := range m.rows {
		if r.Key == k {
			m.moveTo(i)
			return
		}
	}
	t.Fatalf("no row %+v", k)
}

var (
	apiHeader     = model.RowKey{Header: model.GroupProject, Group: apiID}
	shopHeader    = model.RowKey{Header: model.GroupProject, Group: shopID}
	composeHeader = model.RowKey{Header: model.GroupCompose, Group: "shop"}
	otherHeader   = model.RowKey{Header: model.GroupOther}
)

func TestTableWidths(t *testing.T) {
	all := []string{"NAME", "KIND", "PORTS", "PID", "UP", "CPU", "MEM", "USER", "COMMAND"}
	cases := []struct {
		w    int
		want []string
	}{
		{79, []string{"NAME", "PORTS", "PID"}},
		{80, []string{"NAME", "KIND", "PORTS", "PID", "UP"}},
		{89, []string{"NAME", "KIND", "PORTS", "PID", "UP"}},
		{90, []string{"NAME", "KIND", "PORTS", "PID", "UP", "COMMAND"}},
		{99, []string{"NAME", "KIND", "PORTS", "PID", "UP", "COMMAND"}},
		{100, all},
		{120, all},
	}
	for _, c := range cases {
		m, _ := newTest(t, c.w, 24)
		feed(m, fixture())
		openOther(m) // sshd's USER cell
		got := strings.Fields(titles(m))
		if !strings.HasPrefix(titles(m), "NAME") || strings.Join(got[len(got)-len(c.want)+1:], " ") != strings.Join(c.want[1:], " ") {
			t.Errorf("w=%d: titles %q, want %v", c.w, titles(m), c.want)
		}
		has := func(name string) bool { return strings.Contains(strings.Join(c.want, " "), name) }
		vite := line(m, "*5173")
		checks := []struct {
			col, row, val string
		}{
			{"KIND", vite, "server"},
			{"UP", vite, "3h"},
			{"CPU", vite, "12.3"},
			{"MEM", vite, "179M"},
			{"USER", line(m, "*22"), "root"},
			{"COMMAND", vite, "node node_"},
			{"COMMAND", line(m, "8080"), "api -addr"}, // an absolute argv[0] by its basename (DEV-146)
		}
		for _, ch := range checks {
			if strings.Contains(ch.row, ch.val) != has(ch.col) {
				t.Errorf("w=%d: %s shown=%v, but row %q", c.w, ch.col, has(ch.col), ch.row)
			}
		}
		if !strings.Contains(vite, " 101") || !strings.Contains(vite, "node") {
			t.Errorf("w=%d: name, ports and pid always shown: %q", c.w, vite)
		}
		for i, l := range strings.Split(m.View().Content, "\n") {
			if ansi.StringWidth(l) != c.w {
				t.Errorf("w=%d: line %d is %d cells", c.w, i, ansi.StringWidth(l))
			}
		}
		// Columns line up: every title starts where its values start.
		if has("KIND") {
			if i, j := cellIndex(titles(m), "KIND"), cellIndex(vite, "server"); i != j {
				t.Errorf("w=%d: KIND at %d, value at %d", c.w, i, j)
			}
		}
		if i, j := cellIndex(titles(m), "PORTS"), cellIndex(vite, "*5173"); i != j {
			t.Errorf("w=%d: PORTS at %d, value at %d", c.w, i, j)
		}
	}
}

func TestTableNarrow(t *testing.T) {
	// The table renders without panicking down to one column.
	for w := 1; w <= 40; w++ {
		for _, h := range []int{1, 2, 3, 6} {
			m, _ := newTest(t, w, h)
			feed(m, fixture())
			press(m, "G")
			if got := m.tableView(w, h); lipglossHeight(got) > h {
				t.Errorf("%dx%d: %d lines", w, h, lipglossHeight(got))
			}
			for _, l := range strings.Split(m.tableView(w, h), "\n") {
				if ansi.StringWidth(l) > w {
					t.Errorf("%dx%d: line %q wider than %d", w, h, ansi.Strip(l), w)
				}
			}
		}
	}
}

// cellIndex is the cell column at which sub starts in s, or -1.
func cellIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return ansi.StringWidth(s[:i])
}

func lipglossHeight(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func TestTableCells(t *testing.T) {
	m, _ := newTest(t, 140, 30)
	feed(m, fixture())
	openOther(m) // sshd and the unknown owner
	cases := []struct {
		find string
		want []string
	}{
		{"*5173", []string{"  ▾ vite (node) ", " server ", " 101 ", " 3h ", " 12.3 ", " 179M ", " me ", " node node_modules/.bin/vite --port 5173"}},
		{"esbuild", []string{"      esbuild ", " other ", " 102 ", " 2h ", " 0.5 ", " 50M "}},
		{"claude", []string{"    claude ", " agent ", " 103 ", " 20m ", " – "}},
		{"8080", []string{"    api ", " 8080,8081 ", " 200 ", " 1d "}},
		{"go test", []string{"    go ", " test ", " 201 ", " 30s "}},
		{"*5432", []string{"    shop-db-1 (postgres:16) ", " container ", " 300 ", " 4h ", " – ", " root "}},
		{"*8000", []string{"    shop-web-1 (nginx:1.27) ", " container "}},
		{"*22", []string{"    sshd ", " server ", " 1 ", " 3d ", " root ", " sshd -D"}},
		{"*631", []string{"    unknown ", " - ", " – "}},
	}
	for _, c := range cases {
		l := line(m, c.find)
		for _, w := range c.want {
			if !strings.Contains(l+" ", w) {
				t.Errorf("row %s: missing %q in\n%q", c.find, w, l)
			}
		}
	}
	if l := line(m, "*8000"); strings.Contains(l, " 0 ") || strings.Contains(l, "–") {
		t.Errorf("a container row has no pid, cpu or mem: %q", l)
	}
	// zsh's argv is "-zsh"; an unreadable argv shows the name.
	s := fixture()
	s.Processes[1].Argv, s.Processes[1].Unknown = nil, model.FieldArgv
	feed(m, s)
	if l := line(m, "*5173"); !strings.HasSuffix(l, " node") {
		t.Errorf("unreadable argv shows the name as command: %q", l)
	}
}

func TestTableHeaders(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	s.Containers = append(s.Containers, model.Container{ID: "77", Name: "redis", Image: "redis:7",
		Ports: []model.PortMapping{{HostPort: 6379, ContainerPort: 6379, Proto: "tcp"}}})
	feed(m, s)
	openOther(m) // folded, its header reads ▸ other with the same counts (TestOtherStartsCollapsed)
	for _, want := range []string{
		"▾ api @ main (here) · 2 processes · 2 ports",
		"▾ shop @ feat/cart (worktree) · 4 processes · 1 port",
		"▾ shop (compose) · 2 containers · 2 ports",
		"▾ containers · 1 container · 1 port",
		"▾ other · 2 processes · 2 ports",
	} {
		if line(m, want) == "" {
			t.Errorf("no header %q in\n%s", want, screen(m))
		}
	}

	// Linux runs one docker-proxy per port and address family: two rows, one container.
	s = fixture()
	proxy6 := s.Processes[7]
	proxy6.PID, proxy6.Listeners = 301, []model.Listener{lis("tcp6", "::", 5432)}
	s.Processes = append(s.Processes, proxy6)
	feed(m, s)
	if line(m, "▾ shop (compose) · 2 containers · 2 ports") == "" {
		t.Errorf("two proxies for one container count once:\n%s", screen(m))
	}

	// Detached HEAD shows the short SHA in place of the branch.
	s = fixture()
	s.Projects[1].Branch, s.Projects[1].ShortSHA = "", "1a2b3c4"
	feed(m, s)
	if line(m, "▾ api @ 1a2b3c4 (here) · ") == "" {
		t.Errorf("detached header missing:\n%s", screen(m))
	}

	// Counts follow a, not collapse or the filter.
	press(m, "a")
	if line(m, "api @ 1a2b3c4 (here) · 3 processes · 2 ports") == "" {
		t.Errorf("with a, nvim counts:\n%s", screen(m))
	}
	selectKey(t, m, apiHeader)
	press(m, "left")
	if line(m, "▸ api @ 1a2b3c4 (here) · 3 processes · 2 ports") == "" {
		t.Errorf("collapsed header keeps its counts:\n%s", screen(m))
	}
}

func TestTableStyles(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	const faint = "\x1b[2m"
	if l := rawLine(m, "vite (node)"); strings.Contains(l, faint) {
		t.Errorf("zsh folded into node's row makes it faint: %q", l)
	}
	s.Processes[3].PPID = 100 // claude: zsh has two children, so it starts no chain and is a row of its own
	feed(m, s)
	if l := rawLine(m, "zsh"); !strings.Contains(l, faint) {
		t.Errorf("hidden-but-connected zsh is not faint: %q", l)
	}
	if l := rawLine(m, "claude"); strings.Contains(l, faint) {
		t.Errorf("claude is faint: %q", l)
	}
	selectKey(t, m, keyOf(fixture(), 103))
	l := rawLine(m, "claude")
	if !strings.HasPrefix(l, reverse) || ansi.StringWidth(l) != 120 {
		t.Errorf("selected row is not one reverse-video line across the width: %q", l)
	}
	if strings.Count(l, "\x1b[") > 2 {
		t.Errorf("selected row is styled in pieces: %q", l)
	}
}

func TestTableMovement(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	if m.sel != apiHeader {
		t.Fatalf("first row selected: %+v", m.sel)
	}
	press(m, "j", "down")
	if m.sel != keyOf(s, 201) {
		t.Errorf("j, down: %+v, want go test", m.sel)
	}
	press(m, "k")
	if m.sel != keyOf(s, 200) {
		t.Errorf("k: %+v, want api", m.sel)
	}
	press(m, "up", "up", "k")
	if m.sel != apiHeader {
		t.Errorf("up stops at the first row: %+v", m.sel)
	}
	press(m, "G")
	last := m.rows[len(m.rows)-1].Key
	if m.sel != last {
		t.Errorf("G: %+v, want the last row", m.sel)
	}
	press(m, "down")
	if m.sel != last {
		t.Errorf("down stops at the last row: %+v", m.sel)
	}
	press(m, "g")
	if m.sel != apiHeader {
		t.Errorf("g: %+v", m.sel)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.sel != last {
		t.Errorf("end: %+v", m.sel)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if m.sel != apiHeader {
		t.Errorf("home: %+v", m.sel)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.selIdx != len(m.rows)-1 {
		t.Errorf("pgdown on a 30-line screen: index %d, want the last", m.selIdx)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.selIdx != 0 {
		t.Errorf("pgup: index %d", m.selIdx)
	}
}

func TestTableScroll(t *testing.T) {
	// 80x8: header, two footer lines, the title: four rows of table.
	m, _ := newTest(t, 80, 8)
	feed(m, fixture())
	openOther(m) // the unknown owner is the last row
	rows := len(m.rows)
	for i := 1; i < rows; i++ {
		press(m, "j")
		r, _ := m.selected()
		if got := selLine(m); !strings.Contains(got, rowLabel(r)) {
			t.Fatalf("row %d (%s) selected, but the reverse-video line is %q:\n%s", i, rowLabel(r), got, screen(m))
		}
	}
	if line(m, "*631") == "" || line(m, "api @ main") != "" {
		t.Errorf("at the bottom the last row shows and the first does not:\n%s", screen(m))
	}
	press(m, "g")
	if line(m, "api @ main") == "" || line(m, "NAME") == "" {
		t.Errorf("back at the top:\n%s", screen(m))
	}
	// A page is the table's visible rows: four here.
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if screen(m); m.selIdx != 4 || m.top != 1 {
		t.Errorf("pgdown from the top: index %d, top %d, want 4 and 1", m.selIdx, m.top)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.selIdx != 0 {
		t.Errorf("pgup back: index %d", m.selIdx)
	}
	// The screen grows taller than the rows: the table scrolls back to the first row.
	press(m, "G")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if line(m, "api @ main") == "" || m.top != 0 { // render first: View scrolls
		t.Errorf("top %d on a screen that fits every row:\n%s", m.top, screen(m))
	}
}

func TestTableCollapse(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	openOther(m) // nothing collapsed

	// left on an expanded header collapses it; right expands it.
	selectKey(t, m, shopHeader)
	press(m, "left")
	if !m.view.Collapsed[shopHeader] || line(m, "claude") != "" || line(m, "▸ shop @ feat/cart") == "" {
		t.Errorf("left did not collapse shop:\n%s", screen(m))
	}
	press(m, "left")
	if m.sel != shopHeader {
		t.Errorf("left on a collapsed header moved to %+v", m.sel)
	}
	press(m, "l")
	if m.view.Collapsed[shopHeader] || line(m, "claude") == "" || line(m, "▾ shop @ feat/cart") == "" {
		t.Errorf("l did not expand shop:\n%s", screen(m))
	}

	// A tree node: h collapses node (esbuild disappears), h again goes to its parent, the
	// header: zsh is folded into node's row (DEV-157).
	vite := keyOf(s, 101)
	selectKey(t, m, vite)
	press(m, "h")
	if !m.view.Collapsed[vite] || line(m, "esbuild") != "" || line(m, "  ▸ vite (node)") == "" {
		t.Errorf("h did not collapse node:\n%s", screen(m))
	}
	press(m, "h")
	if m.sel != shopHeader {
		t.Errorf("h on a collapsed node: %+v, want its parent, the shop header", m.sel)
	}
	press(m, "down", "right")
	if m.view.Collapsed[vite] || line(m, "esbuild") == "" {
		t.Errorf("right did not expand node:\n%s", screen(m))
	}

	// left on a leaf moves to its parent; right on a leaf does nothing.
	selectKey(t, m, keyOf(s, 102))
	press(m, "right")
	if m.sel != keyOf(s, 102) || len(m.view.Collapsed) != 0 {
		t.Errorf("right on a leaf: sel %+v, collapsed %v", m.sel, m.view.Collapsed)
	}
	press(m, "left")
	if m.sel != vite || m.view.Collapsed[vite] {
		t.Errorf("left on a leaf: %+v, want node, not collapsed", m.sel)
	}
	selectKey(t, m, keyOf(s, 103))
	press(m, "left")
	if m.sel != shopHeader {
		t.Errorf("left on a root goes to its header: %+v", m.sel)
	}

	// A collapsed node whose children exited shows no marker.
	selectKey(t, m, vite)
	press(m, "h")
	gone := fixture()
	gone.Processes = slices.DeleteFunc(gone.Processes, func(p model.Process) bool { return p.PID == 102 })
	feed(m, gone)
	if l := line(m, "node node_modules"); strings.Contains(l, "▸") || strings.Contains(l, "▾") {
		t.Errorf("childless collapsed node keeps a marker: %q", l)
	}
	feed(m, s)
	if line(m, "  ▸ vite (node)") == "" {
		t.Errorf("children back: node is collapsed again:\n%s", screen(m))
	}
	press(m, "l")

	// The unknown-owner row's parent is the other header.
	selectKey(t, m, keyOf(s, 0))
	press(m, "h")
	if m.sel != otherHeader {
		t.Errorf("h on the unknown row: %+v, want other", m.sel)
	}

	// Collapse survives a refresh.
	selectKey(t, m, shopHeader)
	press(m, "left")
	feed(m, fixture())
	if !m.view.Collapsed[shopHeader] || line(m, "claude") != "" {
		t.Errorf("collapse lost on refresh:\n%s", screen(m))
	}
}

func TestTableToggles(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)

	// a shows shells and editors.
	if line(m, "nvim") != "" {
		t.Fatal("nvim shown by default")
	}
	press(m, "a")
	if !m.view.ShowAll || line(m, "nvim") == "" || strings.Contains(rawLine(m, "zsh"), "\x1b[2m") {
		t.Errorf("a: ShowAll=%v\n%s", m.view.ShowAll, screen(m))
	}
	if !strings.Contains(titles(m), "all") {
		t.Errorf("title does not say all: %q", titles(m))
	}
	press(m, "a")
	if m.view.ShowAll || line(m, "nvim") != "" {
		t.Errorf("a again: ShowAll=%v\n%s", m.view.ShowAll, screen(m))
	}

	// d hides container rows.
	press(m, "d")
	if !m.view.HideContainers || slices.Contains(rowKeys(m), composeHeader) || line(m, "*5432") != "" {
		t.Errorf("d: HideContainers=%v\n%s", m.view.HideContainers, screen(m))
	}
	if !strings.Contains(titles(m), "no containers") {
		t.Errorf("title does not say no containers: %q", titles(m))
	}
	press(m, "d")
	if m.view.HideContainers || line(m, "shop-web-1") == "" {
		t.Errorf("d again: HideContainers=%v\n%s", m.view.HideContainers, screen(m))
	}

	// With room, the name column widens to show every toggle.
	press(m, "s", "a", "d")
	if !strings.HasPrefix(titles(m), "NAME · sort: port · all · no containers ") {
		t.Errorf("toggles title %q", titles(m))
	}
	press(m, "s", "s", "s", "s", "a", "d")

	// s cycles default, port, cpu, start time, name, default.
	order := func() (api, test int) {
		for i, k := range rowKeys(m) {
			switch k {
			case keyOf(s, 200):
				api = i
			case keyOf(s, 201):
				test = i
			}
		}
		return
	}
	for _, c := range []struct {
		sort  model.SortMode
		title string
	}{
		{model.SortPort, "sort: port"},
		{model.SortCPU, "sort: cpu"},
		{model.SortStart, "sort: start"},
		{model.SortName, "sort: name"},
		{model.SortDefault, ""},
	} {
		press(m, "s")
		if m.view.Sort != c.sort {
			t.Errorf("s: sort %v, want %v", m.view.Sort, c.sort)
		}
		if c.title != "" && !strings.Contains(titles(m), c.title) || c.title == "" && strings.Contains(titles(m), "sort") {
			t.Errorf("sort %v: title %q", c.sort, titles(m))
		}
		api, test := order()
		if (api > test) != (c.sort == model.SortStart) {
			t.Errorf("sort %v: api at %d, go test at %d", c.sort, api, test)
		}
	}
}

func TestTableEmpty(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	if strings.Contains(screen(m), "NAME") {
		t.Errorf("titles before the first snapshot:\n%s", screen(m))
	}
	press(m, "j", "left", "right", "G")
	s := fixture()
	s.Processes, s.Containers, s.Projects = nil, nil, nil
	feed(m, s)
	if line(m, "nothing to show") == "" {
		t.Errorf("empty snapshot:\n%s", screen(m))
	}
	press(m, "j", "left", "right", "G", "a", "s")
}

func TestFormat(t *testing.T) {
	for b, want := range map[uint64]string{
		0: "0B", 1023: "1023B", 1024: "1K", 50 << 20: "50M", 187563008: "179M", 1023 << 20: "1023M",
		1 << 30: "1.0G", 1288490188: "1.2G", 15 << 30: "15G",
	} {
		if got := mem(b); got != want {
			t.Errorf("mem(%d) = %q, want %q", b, got, want)
		}
	}
	p := model.Process{PID: 1, CPUPercent: 12.345}
	if got := cpu(&p); got != "12.3" {
		t.Errorf("cpu = %q", got)
	}
	for v, want := range map[float64]string{99.94: "99.9", 99.96: "100", 1234.5: "1234", 99999: "99999"} {
		p.CPUPercent = v
		if got := cpu(&p); got != want {
			t.Errorf("cpu(%v) = %q, want %q", v, got, want)
		}
	}
	p.CPUPercent = math.NaN()
	if got := cpu(&p); got != "–" {
		t.Errorf("cpu NaN = %q", got)
	}
	p.CPUPercent, p.Unknown = 3, model.FieldCPU
	if got := cpu(&p); got != "–" {
		t.Errorf("cpu unknown = %q", got)
	}

	any4, any6 := netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("::")
	lo := netip.MustParseAddr("127.0.0.1")
	r := model.Row{Process: &model.Process{Listeners: []model.Listener{
		{Proto: "tcp6", Addr: any6, Port: 5173}, {Proto: "tcp4", Addr: lo, Port: 5173},
		{Proto: "tcp4", Addr: lo, Port: 3000}, {Proto: "tcp4", Addr: any4, Port: 9229}, {Proto: "tcp6", Addr: any6, Port: 9229},
	}}}
	if got := portsText(ports(r)); got != "3000,*5173,*9229" {
		t.Errorf("ports = %q", got)
	}
	c := model.Row{Container: &model.Container{Ports: []model.PortMapping{
		{HostIP: lo, HostPort: 8001}, {HostPort: 8000}, {ContainerPort: 9000}, {HostIP: any6, HostPort: 8000},
	}}}
	if got := portsText(ports(c)); got != "*8000,8001" {
		t.Errorf("container ports = %q", got)
	}
}

// The here project's header ends in (here) and comes first, whatever the activity order: in
// the fixture api is the here project and the most recently active, so shop takes its place.
func TestTableHere(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	s.Projects[0].Here, s.Projects[1].Here = true, false
	feed(m, s)
	if got := m.rows[0]; got.Key != shopHeader {
		t.Errorf("first row %+v, want the here project's header", got.Key)
	}
	if line(m, "▾ shop @ feat/cart (worktree) (here) · 4 processes · 1 port") == "" {
		t.Errorf("no (here) header:\n%s", screen(m))
	}
	if line(m, "▾ api @ main · 2 processes · 2 ports") == "" {
		t.Errorf("api's header still says (here):\n%s", screen(m))
	}
	// The label the port answer shares has no suffix: it says "this repo" instead.
	if got := rowLabel(m.rows[0]); got != "shop @ feat/cart (worktree)" {
		t.Errorf("rowLabel = %q, want no (here)", got)
	}
}

func TestTableTags(t *testing.T) {
	const full, short = "    api  orphaned, cwd deleted ", "    api  ! "
	for _, tc := range []struct {
		w     int
		want  string
		other string
	}{
		{80, short, full},
		{89, short, full},
		{90, full, short},
		{120, full, short},
	} {
		m, _ := newTest(t, tc.w, 30)
		feed(m, fixture())
		l := line(m, "8080")
		if !strings.Contains(l, tc.want) || strings.Contains(l, tc.other) {
			t.Errorf("%d columns: api's row %q, want %q", tc.w, l, tc.want)
		}
		if l := line(m, "esbuild"); strings.Contains(l, "!") || strings.Contains(l, "orphaned") {
			t.Errorf("%d columns: an untagged row shows tags: %q", tc.w, l)
		}
	}

	// The labels are faint, and the rest of the row keeps the row's style: plain, or reverse
	// video when selected.
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	const faint = "\x1b[2m"
	l := rawLine(m, "8080")
	if !strings.Contains(l, faint+"  orphaned, cwd deleted") || strings.HasPrefix(l, faint) {
		t.Errorf("only the tags are faint: %q", l)
	}
	selectKey(t, m, keyOf(s, 200))
	l = rawLine(m, "8080")
	for _, part := range []string{"api", "orphaned", "server", "-addr"} {
		if sgr := sgrBefore(l, part); !strings.Contains(sgr, "7") {
			t.Errorf("selected row: %q drawn with %q, not in reverse video: %q", part, sgr, l)
		}
	}
	if sgr := sgrBefore(l, "orphaned"); !strings.Contains(sgr, "2") {
		t.Errorf("selected row: the tags are drawn with %q, not faint: %q", sgr, l)
	}
	if ansi.StringWidth(l) != 120 {
		t.Errorf("selected row is %d cells wide, want 120", ansi.StringWidth(l))
	}

	// The widest name cell is measured as drawn, and measured again when the width crosses 90
	// with the same rows.
	if got, want := m.cache().longest, len(strings.TrimRight(full, " ")); got != want {
		t.Errorf("120 columns: longest name cell %d, want %d", got, want)
	}
	m.Update(tea.WindowSizeMsg{Width: 89, Height: 30})
	if got, want := m.cache().longest, len("    shop-db-1 (postgres:16)"); got != want {
		t.Errorf("89 columns: longest name cell %d, want %d", got, want)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if l := line(m, "8080"); !strings.Contains(l, full) {
		t.Errorf("back at 120 columns: the name column does not fit the tags: %q", l)
	}
}

// sgrBefore returns the last SGR sequence before the first occurrence of s in the styled line l.
func sgrBefore(l, s string) string {
	i := strings.Index(l, s)
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(l[:i], "\x1b[")
	if j < 0 {
		return ""
	}
	return l[j : j+strings.IndexByte(l[j:], 'm')+1]
}

// A process run by an interpreter is labelled `<tool> (<name>)` at every width; one with no
// tool keeps its name (spec "Release 1.1", tool labels). From 90 columns only the label
// changes: the name cell holds no arguments, and the widest name cell is measured without them.
func TestTableToolLabels(t *testing.T) {
	for _, w := range []int{80, 120} {
		m, _ := newTest(t, w, 30)
		feed(m, fixture())
		if l := line(m, "5173"); !strings.Contains(l, "  ▾ vite (node)") { // zsh folded into its row, not drawn (DEV-157, DEV-160)
			t.Errorf("%d columns: vite's row %q, want the label vite (node)", w, l)
		}
		for _, want := range []string{"    claude ", "    shop-db-1 (postgres:16) "} {
			if !strings.Contains(screen(m), want) {
				t.Errorf("%d columns: no row %q:\n%s", w, want, screen(m))
			}
		}
	}

	m, _ := newTest(t, 120, 30)
	feed(m, fixture())
	l := line(m, "5173")
	if name := l[:strings.Index(l, "server")]; strings.Contains(name, "--port") {
		t.Errorf("120 columns: the name cell holds arguments: %q", l)
	}
	if !strings.Contains(l, "node node_modules/.bin/vite") {
		t.Errorf("120 columns: the command column lost argv: %q", l)
	}
	if got, want := m.cache().longest, len("    api  orphaned, cwd deleted"); got != want {
		t.Errorf("120 columns: longest name cell %d, want %d", got, want)
	}

	// model.Process.Label is the one place the label is built: a module after -m keeps its case, a script
	// its extension, and a process with no tool (no argv, or not an interpreter) its name.
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"python3.12", []string{"python3.12", "-m", "uvicorn", "app:app"}, "uvicorn (python3.12)"},
		{"node", []string{"/usr/bin/node", "/src/app/server.js"}, "server.js (node)"},
		{"npx", []string{"npx", "vitest"}, "vitest (npx)"},
		{"node", []string{"node"}, "node"},
		{"node", nil, "node"},
		{"go", []string{"go", "test", "./..."}, "go"},
	} {
		if got := (model.Process{Name: tc.name, Argv: tc.argv}).Label(); got != tc.want {
			t.Errorf("Label(%q, %q) = %q, want %q", tc.name, tc.argv, got, tc.want)
		}
	}
}

// Below 90 columns a process row's name cell shows its arguments after the label and tags:
// what follows the tool for an interpreter, argv[1:] otherwise, faint, cut at the column's
// edge, and none when fewer than 6 cells are left (spec "Release 1.1", tool labels).
func TestTableArgs(t *testing.T) {
	m, _ := newTest(t, 80, 30)
	s := fixture()
	feed(m, s)
	for _, want := range []string{
		"    api  !  -addr :8080 ",
		"    go  test ./... ",
		"  ▾ vite (node)  --port 5173 ",
		"      esbuild  --service=0.21.5 --ping ",
	} {
		if !strings.Contains(screen(m), want) {
			t.Errorf("no row %q:\n%s", want, screen(m))
		}
	}
	selectKey(t, m, model.RowKey{Header: model.GroupOther})
	press(m, "right") // other starts collapsed
	if l := line(m, "sshd"); !strings.HasPrefix(l, "    sshd  -D ") {
		t.Errorf("sshd's row %q, want its arguments:\n%s", l, screen(m))
	}
	// Headers, container rows (the proxy's row is the container's) and the unknown owner have
	// none.
	if l, want := line(m, "shop (compose)"), "▾ shop (compose) · 2 containers · 2 ports"; l != want {
		t.Errorf("compose header %q, want %q", l, want)
	}
	for _, want := range []string{"    shop-web-1 (nginx:1.27)  ", "    shop-db-1 (postgres:16)  "} {
		if l := line(m, strings.TrimSpace(want)); !strings.HasPrefix(l, want) || strings.Contains(l, "-proto") {
			t.Errorf("row %q has arguments: %q", want, l)
		}
	}
	unknown := s.Processes[9]
	unknown.Argv = []string{"cupsd", "-l"} // never read for PID 0, but drawn as none if it were
	if got := argText(model.Row{Key: unknown.Key(), Process: &unknown}); got != "" {
		t.Errorf("the unknown owner's arguments %q, want none", got)
	}

	// Only the tags and the arguments are faint; a selected row keeps reverse video throughout.
	const faint = "\x1b[2m"
	if l := rawLine(m, "5173"); !strings.Contains(l, faint+"  --port 5173") || strings.Contains(sgrBefore(l, "vite"), "2") {
		t.Errorf("only the arguments are faint: %q", l)
	}
	if l := rawLine(m, "8080"); !strings.Contains(l, faint+"  !  -addr :8080") {
		t.Errorf("api's tags and arguments are not one faint piece: %q", l)
	}
	selectKey(t, m, keyOf(s, 101))
	l := rawLine(m, "5173")
	for _, part := range []string{"vite", "--port", "server"} {
		if sgr := sgrBefore(l, part); !strings.Contains(sgr, "7") {
			t.Errorf("selected row: %q drawn with %q, not in reverse video: %q", part, sgr, l)
		}
	}
	if sgr := sgrBefore(l, "--port"); !strings.Contains(sgr, "2") {
		t.Errorf("selected row: the arguments are drawn with %q, not faint: %q", sgr, l)
	}
	if ansi.StringWidth(l) != 80 {
		t.Errorf("selected row is %d cells wide, want 80", ansi.StringWidth(l))
	}

	// The name column is 45 cells at 80 columns, and a depth-1 cell starts with 4: with 6 cells
	// left after the name and two spaces the arguments show, cut; with 5 they are left out.
	for _, tc := range []struct {
		name string
		want string
	}{
		{strings.Repeat("n", 45-4-2-6), "  -addr…"},
		{strings.Repeat("n", 45-4-2-5), ""},
	} {
		s := fixture()
		s.Processes[5].Name = tc.name // go test
		s.Processes[5].Argv = []string{"go", "-addr", ":8080"}
		m, _ := newTest(t, 80, 30)
		feed(m, s)
		if l, want := line(m, tc.name), "    "+tc.name+tc.want+" "; !strings.HasPrefix(l, want) {
			t.Errorf("%d-cell name: row %q, want it to start %q", len(tc.name), l, want)
		}
	}

	// A process without argv has none, and arguments are cleaned.
	s = fixture()
	noArgv := s.Processes[5]
	noArgv.Argv = nil
	if got := argText(model.Row{Key: noArgv.Key(), Process: &noArgv}); got != "" {
		t.Errorf("a process without argv has arguments %q", got)
	}
	s.Processes[6].Argv = []string{"nvim", "a\x1b[2Jb"}
	m, _ = newTest(t, 80, 30)
	feed(m, s)
	press(m, "a") // show nvim
	if l := line(m, "202"); !strings.HasPrefix(l, "    nvim  a?[2Jb ") {
		t.Errorf("nvim's arguments are not cleaned: %q", l)
	}

	// At 89 columns they show, at 90 not: the command column holds them there.
	for _, tc := range []struct {
		w    int
		args bool
	}{{89, true}, {90, false}} {
		m, _ := newTest(t, tc.w, 30)
		feed(m, fixture())
		l := line(m, "201")
		if got := strings.HasPrefix(l, "    go  test ./..."); got != tc.args {
			t.Errorf("%d columns: arguments shown %v, want %v: %q", tc.w, got, tc.args, l)
		}
	}
}

// TestTableInlineCode: a process that runs inline code (`python3 -c`, `node -e`) has no tool, so
// its row keeps the interpreter's name and the code follows as its arguments, not as the label
// (DEV-137).
func TestTableInlineCode(t *testing.T) {
	s := fixture()
	s.Processes[5].Name = "python3" // go test
	s.Processes[5].Argv = []string{"python3", "-c", "from multiprocessing.spawn import x", "--fork"}
	m, _ := newTest(t, 80, 30)
	feed(m, s)
	if l, want := line(m, "multiprocessing"), "    python3  -c from multiprocessing.spawn i… "; !strings.HasPrefix(l, want) {
		t.Errorf("row %q, want it to start %q", l, want)
	}
	if got := s.Processes[5].Label(); got != "python3" {
		t.Errorf("Label = %q, want python3", got)
	}
}
