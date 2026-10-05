package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Folded chains (DEV-157). chainFixture is GitHub issue #107's tree in project lte: bash 10 →
// claude 11 → bash 12 → bash 13 → guard.sh 14 → run55.sh 15 → xargs 16, whose children are
// sh 17 (→ time 18 → timeout 19 → lte_scanner 20, and grep 21 under time) and sleep 22. The
// tests draw at 160 columns, where the name column fits the whole chain.

const lteID = "/src/lte"

func chainFixture() model.Snapshot {
	proc := func(pid, ppid int, name string, kind model.Kind) model.Process {
		return model.Process{PID: pid, PPID: ppid, StartTime: at(time.Hour - time.Duration(pid)*time.Second), UID: 501,
			User: "me", Name: name, Argv: []string{name}, Cwd: lteID, CPUPercent: 0.5, RSSBytes: 1 << 20, Kind: kind, ProjectID: lteID}
	}
	return model.Snapshot{
		SchemaVersion: 1,
		TakenAt:       at(2 * time.Second),
		Host:          model.Host{OS: "darwin", Arch: "arm64", Hostname: "mbp", UID: 501},
		Projects:      []model.Project{{ID: lteID, Root: lteID, Name: "lte-scanner", Branch: "master"}},
		Processes: []model.Process{
			proc(10, 9, "bash", model.KindShell), proc(11, 10, "claude", model.KindAgent),
			proc(12, 11, "bash", model.KindShell), proc(13, 12, "bash", model.KindShell),
			proc(14, 13, "guard.sh", model.KindShell), proc(15, 14, "run55.sh", model.KindShell),
			proc(16, 15, "xargs", model.KindOther), proc(17, 16, "sh", model.KindOther),
			proc(18, 17, "time", model.KindOther), proc(19, 18, "timeout", model.KindOther),
			proc(20, 19, "lte_scanner", model.KindOther), proc(21, 18, "grep", model.KindOther),
			proc(22, 16, "sleep", model.KindOther),
		},
	}
}

// chainLine is the folded row as drawn without `a`, which leaves the shells out of the label
// (DEV-160); fullChain with `a`.
const (
	chainLine = "claude › xargs"
	fullChain = "bash › claude › bash › bash › guard.sh › run55.sh › xargs"
)

func TestFoldLabel(t *testing.T) {
	s := chainFixture()
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	if !strings.HasPrefix(line(m, chainLine), "  ▾ "+chainLine+" ") {
		t.Errorf("no folded row at depth 1:\n%s", screen(m))
	}
	press(m, "a")
	if !strings.HasPrefix(line(m, fullChain), "  ▾ "+fullChain+" ") {
		t.Errorf("with a, no full folded row at depth 1:\n%s", screen(m))
	}
	press(m, "a")
	for _, want := range []string{"    ▾ sh › time ", "        timeout › lte_scanner ", "        grep ", "      sleep "} {
		if !strings.HasPrefix(line(m, strings.TrimSpace(want)), want) {
			t.Errorf("no row %q:\n%s", want, screen(m))
		}
	}
	// The header counts processes, not rows.
	if line(m, "▾ lte-scanner @ master · 13 processes · 0 ports") == "" {
		t.Errorf("header counts changed:\n%s", screen(m))
	}

	// At 80 columns the name column is 45 cells: leading links give way to `… › `, counting
	// only the links drawn. Without `a` the label fits; with a long claude it does not.
	m, _ = newTest(t, 80, 24)
	feed(m, s)
	if got := line(m, "xargs"); !strings.HasPrefix(got, "  ▾ "+chainLine+" ") {
		t.Errorf("narrow folded row %q", got)
	}
	press(m, "a")
	if got := line(m, "xargs"); !strings.HasPrefix(got, "  ▾ … › bash › guard.sh › run55.sh › xargs ") {
		t.Errorf("narrow folded row with a %q", got)
	}
	long := strings.Repeat("c", 38)
	s.Processes[1].Name, s.Processes[1].Argv = long, []string{long}
	m, _ = newTest(t, 80, 24)
	feed(m, s)
	if got := line(m, "xargs"); !strings.HasPrefix(got, "  ▾ … › xargs ") {
		t.Errorf("narrow folded row with a long link %q", got)
	}
}

// TestFoldLabelHiddenLinks (DEV-160): a link the view hides (a shell or editor, without `a`)
// is left out of a folded row's label, unless a filter is set and it matches; the row's own
// process always keeps its label. Nothing else about the row changes.
//
//	zsh 30 → vite 31 (5173)                       drawn `vite`, unfolds to zsh (dim) and vite
//	disclaimer 40 → claude 41 → zsh 42 → x 43, y 44 drawn `disclaimer › claude › zsh` (dim)
func TestFoldLabelHiddenLinks(t *testing.T) {
	s := chainFixture()
	proc := func(pid, ppid int, name string, kind model.Kind) model.Process {
		p := s.Processes[0]
		p.PID, p.PPID, p.Name, p.Argv, p.Kind = pid, ppid, name, []string{name}, kind
		p.StartTime = at(time.Hour - time.Duration(pid)*time.Second)
		return p
	}
	vite := proc(31, 30, "vite", model.KindServer)
	vite.Listeners = []model.Listener{lis("tcp4", "0.0.0.0", 5173)}
	s.Processes = []model.Process{
		proc(30, 9, "zsh", model.KindShell), vite,
		proc(40, 9, "disclaimer", model.KindOther), proc(41, 40, "claude", model.KindAgent),
		proc(42, 41, "zsh", model.KindShell), proc(43, 42, "x", model.KindOther), proc(44, 42, "y", model.KindOther),
	}
	zsh, viteKey := keyOf(s, 30), keyOf(s, 31)

	m, _ := newTest(t, 160, 30)
	feed(m, s)
	if got := line(m, "vite"); !strings.HasPrefix(got, "    vite ") || strings.Contains(got, "zsh") {
		t.Errorf("hidden link drawn: %q\n%s", got, screen(m))
	}
	if !strings.HasPrefix(line(m, "claude"), "  ▾ disclaimer › claude › zsh ") {
		t.Errorf("chain ending in a hidden shell:\n%s", screen(m))
	}
	if r := m.rows[1+slices.IndexFunc(m.rows[1:], func(r model.Row) bool { return r.Key == viteKey })]; len(r.Links) != 1 || r.Depth != 1 {
		t.Errorf("vite row: links %v depth %d", r.Links, r.Depth)
	}
	press(m, "a")
	if !strings.HasPrefix(line(m, "vite"), "    zsh › vite ") {
		t.Errorf("with a, no zsh › vite:\n%s", screen(m))
	}
	press(m, "a")

	// → on the row that shows one name has nothing to unfold: the view hides zsh (DEV-193).
	selectRow(t, m, viteKey)
	press(m, "right")
	if len(m.unfolded) != 0 || line(m, "▾ zsh") != "" || !strings.HasPrefix(line(m, "vite"), "    vite ") || m.sel != viteKey {
		t.Fatalf("right on a one-name row (unfolded %v):\n%s", m.unfolded, screen(m))
	}
	// With `a` it unfolds as any chain, and ← on its first row folds it again.
	press(m, "a", "right")
	if !m.unfolded[zsh] || !strings.HasPrefix(line(m, "zsh"), "  ▾ zsh ") || !strings.HasPrefix(line(m, "vite"), "      vite ") {
		t.Fatalf("with a, right did not unfold the chain:\n%s", screen(m))
	}
	selectRow(t, m, zsh)
	press(m, "left", "a")
	if m.unfolded[zsh] || m.sel != viteKey || !strings.HasPrefix(line(m, "vite"), "    vite ") {
		t.Fatalf("left did not fold the chain (sel %+v):\n%s", m.sel, screen(m))
	}

	// Name sort orders by the label of the plain view, with a filter set too: the search
	// flattens with every row shown, yet `vite` still sorts before `watch`.
	w := proc(50, 9, "watch", model.KindOther)
	sorted := s
	sorted.Processes = append([]model.Process{w}, s.Processes[:2]...)
	for _, query := range []string{"", "lte"} {
		m, _ := newTest(t, 160, 30)
		m.view.Sort = model.SortName
		feed(m, sorted)
		if query != "" {
			press(m, "/")
			typeText(m, query)
		}
		if v, w := lineIndex(m, "*5173"), lineIndex(m, "watch"); v < 0 || w < 0 || v > w {
			t.Errorf("/%s: name sort puts vite after watch:\n%s", query, screen(m))
		}
	}

	// A filter draws a hidden link that matches it.
	for _, tc := range []struct{ query, want string }{
		{"zsh", "    zsh › vite "},
		{"vite", "    vite "},
		{"5173", "    vite "},
	} {
		m, _ := newTest(t, 160, 30)
		feed(m, s)
		press(m, "/")
		typeText(m, tc.query)
		if !strings.HasPrefix(line(m, "*5173"), tc.want) {
			t.Errorf("/%s: want %q\n%s", tc.query, tc.want, screen(m))
		}
	}
}

func TestChainLabel(t *testing.T) {
	s := chainFixture()
	links := []*model.Process{&s.Processes[0], &s.Processes[1]}
	for _, tt := range []struct {
		room int
		want string
	}{
		{100, "bash › claude › xargs"},
		{21, "bash › claude › xargs"},
		{20, "… › claude › xargs"},
		{9, "… › xargs"},
		{3, "… › xargs"}, // the last label is kept; pad cuts it
	} {
		if got := chainLabel(links, "xargs", tt.room); got != tt.want {
			t.Errorf("chainLabel(room %d) = %q, want %q", tt.room, got, tt.want)
		}
	}
}

func TestFoldKeys(t *testing.T) {
	s := chainFixture()
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	xargs, bash, claude := keyOf(s, 16), keyOf(s, 10), keyOf(s, 11)

	// → on a folded row that is collapsed expands it first; a second → unfolds it.
	selectRow(t, m, xargs)
	press(m, "left")
	if !m.view.Collapsed[xargs] || line(m, "▸ "+chainLine) == "" || line(m, "sleep") != "" {
		t.Fatalf("left did not collapse the folded row:\n%s", screen(m))
	}
	press(m, "right")
	if m.view.Collapsed[xargs] || line(m, "▾ "+chainLine) == "" {
		t.Fatalf("right did not expand the folded row:\n%s", screen(m))
	}
	press(m, "right") // the row for each process the label names, from claude (DEV-193)
	if !m.unfolded[claude] || line(m, chainLine) != "" || !strings.HasPrefix(line(m, "claude"), "  ▾ claude ") ||
		!strings.HasPrefix(line(m, "xargs"), "    ▾ xargs ") || m.sel != xargs {
		t.Fatalf("right did not unfold the chain with xargs selected (%+v):\n%s", m.sel, screen(m))
	}
	press(m, "right") // nothing more to unfold
	if m.sel != xargs || len(m.unfolded) != 1 {
		t.Errorf("right on an unfolded row: sel %+v, unfolded %v", m.sel, m.unfolded)
	}

	// ← on the first row folds the chain again and selects the folded row; elsewhere it collapses.
	selectRow(t, m, claude)
	press(m, "left")
	if len(m.unfolded) != 0 || line(m, "▾ "+chainLine) == "" || m.sel != xargs {
		t.Fatalf("left on the first row did not fold the chain (sel %+v):\n%s", m.sel, screen(m))
	}
	press(m, "a", "right")
	selectRow(t, m, claude)
	press(m, "left")
	if !m.view.Collapsed[claude] || !m.unfolded[bash] || line(m, "▸ claude") == "" {
		t.Fatalf("left on a link of an unfolded chain did not collapse it:\n%s", screen(m))
	}
	press(m, "left") // claude is collapsed: ← moves to its parent, the first row
	if m.sel != bash {
		t.Fatalf("left on collapsed claude: %+v, want bash", m.sel)
	}
	// A collapsed process ends a chain: folded again, the row stops at claude.
	press(m, "left")
	if got := line(m, "claude"); !strings.HasPrefix(got, "  ▸ bash › claude ") || m.sel != claude {
		t.Errorf("refolded with claude collapsed: %q, sel %+v", got, m.sel)
	}
}

// TestFoldUnfoldHiddenLinks (DEV-193): → unfolds a folded row into one row for each process its
// label names, each a level below the one before: a link the view hides gets no row, and while
// a filter is set one that matches it does (DEV-160). The chain stays unfolded while `a` or a
// filter changes which link comes first, and ← on that first row folds it.
func TestFoldUnfoldHiddenLinks(t *testing.T) {
	s := chainFixture()
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	// rows are the process rows as "depth label", a folded row by its processes' names.
	rows := func() []string {
		var out []string
		for _, r := range m.rows {
			if r.Process != nil {
				names := []string{}
				for _, l := range r.Links {
					names = append(names, l.Name)
				}
				out = append(out, strconv.Itoa(r.Depth)+" "+strings.Join(append(names, r.Process.Name), " › "))
			}
		}
		return out
	}
	below := []string{"sh › time", "timeout › lte_scanner", "grep", "sleep"} // xargs's subtree, depths added below
	tree := func(top ...string) []string {
		d := len(top)
		return append(top, strconv.Itoa(d+1)+" "+below[0], strconv.Itoa(d+2)+" "+below[1], strconv.Itoa(d+2)+" "+below[2], strconv.Itoa(d+1)+" "+below[3])
	}
	selectRow(t, m, keyOf(s, 16))
	press(m, "right")
	for _, step := range []struct {
		name string
		keys []string
		want []string
	}{
		{"plain", nil, tree("1 claude", "2 xargs")},
		{"show all", []string{"a"}, tree("1 bash", "2 claude", "3 bash", "4 bash", "5 guard.sh", "6 run55.sh", "7 xargs")},
		{"plain again", []string{"a"}, tree("1 claude", "2 xargs")},
		{"filter matching the shells", []string{"/", "b", "a", "s", "h", "enter"}, []string{"1 bash", "2 claude", "3 bash", "4 bash"}},
		{"filter cleared", []string{"/", "esc"}, tree("1 claude", "2 xargs")},
	} {
		press(m, step.keys...)
		if got := rows(); !slices.Equal(got, step.want) {
			t.Errorf("%s: rows %q, want %q\n%s", step.name, got, step.want, screen(m))
		}
	}
	selectRow(t, m, keyOf(s, 11))
	press(m, "left")
	if got := rows(); len(m.unfolded) != 0 || len(got) == 0 || got[0] != "1 bash › claude › bash › bash › guard.sh › run55.sh › xargs" || m.sel != keyOf(s, 16) {
		t.Errorf("left on claude did not fold the chain: rows %q, unfolded %v, sel %+v", got, m.unfolded, m.sel)
	}
}

// TestFoldUnfoldHiddenSelection (DEV-193): a selected link of an unfolded chain that `a` then
// hides has no row any more; the selection goes to the chain's first drawn row, not up to the
// group header.
func TestFoldUnfoldHiddenSelection(t *testing.T) {
	s := chainFixture()
	for _, pid := range []int{10, 13} { // the first link, and one inside the chain
		m, _ := newTest(t, 160, 30)
		feed(m, s)
		press(m, "a")
		selectRow(t, m, keyOf(s, 16))
		press(m, "right")
		selectRow(t, m, keyOf(s, pid))
		press(m, "a")
		if m.sel != keyOf(s, 11) {
			t.Errorf("bash %d hidden: selection %+v, want claude 11\n%s", pid, m.sel, screen(m))
		}
	}
}

// TestFoldUnfoldOneName (DEV-198): a chain unfolded under `a` whose label is one name without
// `a` (zsh 30 › vite 31) keeps its key while it reads `vite`, so pressing `a` twice, or a filter
// that draws zsh (/vite matches its argv), shows it unfolded again. → on a one-name row does
// nothing: there is nothing to unfold, and `a` later shows the chain folded.
func TestFoldUnfoldOneName(t *testing.T) {
	s := chainFixture()
	zsh, vite := s.Processes[0], s.Processes[1]
	zsh.PID, zsh.PPID, zsh.Name, zsh.Argv, zsh.Kind = 30, 9, "zsh", []string{"zsh", "-c", "vite; :"}, model.KindShell
	vite.PID, vite.PPID, vite.Name, vite.Argv, vite.Kind = 31, 30, "vite", []string{"vite"}, model.KindServer
	s.Processes = []model.Process{zsh, vite}
	unfolded := func(m *Model) bool { return line(m, "zsh") != "" && line(m, "zsh › vite") == "" }

	m, _ := newTest(t, 160, 30)
	feed(m, s)
	press(m, "a")
	selectRow(t, m, keyOf(s, 31))
	press(m, "right")
	for _, step := range []struct {
		name string
		keys []string
		want bool // zsh has a row of its own
	}{
		{"unfolded", nil, true},
		{"a off", []string{"a"}, false},
		{"a on again", []string{"a"}, true},
		{"a off, filter drawing zsh", []string{"a", "/", "v", "i", "t", "e", "enter"}, true},
		{"filter cleared, a on", []string{"/", "esc", "a"}, true},
	} {
		press(m, step.keys...)
		if got := unfolded(m); got != step.want || line(m, "vite") == "" {
			t.Errorf("%s: zsh row %v, want %v\n%s", step.name, got, step.want, screen(m))
		}
	}

	m, _ = newTest(t, 160, 30)
	feed(m, s)
	selectRow(t, m, keyOf(s, 31))
	press(m, "right", "a")
	if unfolded(m) || len(m.unfolded) != 0 {
		t.Errorf("→ on the one-name row unfolded the chain: unfolded %v\n%s", m.unfolded, screen(m))
	}
}

func TestFoldUnfoldedSurvivesRefresh(t *testing.T) {
	s := chainFixture()
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	selectRow(t, m, keyOf(s, 16))
	press(m, "right")
	feed(m, chainFixture())
	if line(m, chainLine) != "" || line(m, "▾ claude") == "" || m.sel != keyOf(s, 16) {
		t.Errorf("unfolded chain refolded on refresh:\n%s", screen(m))
	}
	// Kept by the first process the label names (DEV-193), and pruned once it is gone, as
	// collapsed keys are.
	feed(m, drop(chainFixture(), 11))
	if len(m.unfolded) != 0 {
		t.Errorf("unfolded keys after claude exited: %v", m.unfolded)
	}
}

func TestFoldFilter(t *testing.T) {
	m, _ := newTest(t, 160, 30)
	feed(m, chainFixture())
	press(m, "/")
	typeText(m, "claude")
	if line(m, chainLine) == "" || line(m, "sleep") != "" {
		t.Errorf("/claude does not show the folded row alone:\n%s", screen(m))
	}
}

// TestFoldSearchHiddenLast: a search flattens with every row shown, so a chain can end in a
// process the view hides. With nothing kept below it and no match of its own, the row is cut
// back to its last process the view shows or that matches, drawn as that process; with a kept
// row below it, it is dimmed as the plain view draws it.
//
//	claude 11 → bash 12 (idle)      the view shows claude
//	zsh 13 → bash 14 (idle)         the view shows neither
//	node 20 → sh 21 → x 22, y 23    the view shows node › sh, dimmed, then x and y
func TestFoldSearchHiddenLast(t *testing.T) {
	s := chainFixture()
	proc := func(pid, ppid int, name string, kind model.Kind) model.Process {
		p := s.Processes[0]
		p.PID, p.PPID, p.Name, p.Argv, p.Kind = pid, ppid, name, []string{name}, kind
		p.StartTime = at(time.Hour - time.Duration(pid)*time.Second)
		return p
	}
	s.Processes = []model.Process{
		proc(11, 9, "claude", model.KindAgent), proc(12, 11, "bash", model.KindShell),
		proc(13, 9, "zsh", model.KindShell), proc(14, 13, "bash", model.KindShell),
		proc(20, 9, "node", model.KindOther), proc(21, 20, "sh", model.KindShell),
		proc(22, 21, "x", model.KindOther), proc(23, 21, "y", model.KindOther),
	}
	// draw is a row as the test reads it: its processes' names, and "(dim)" when dimmed.
	draw := func(r model.Row) string {
		if r.Process == nil {
			return "[" + r.Project.Name + "]"
		}
		var names []string
		for _, l := range r.Links {
			names = append(names, l.Name)
		}
		d := strings.Join(append(names, r.Process.Name), " › ")
		if r.Dimmed {
			d += " (dim)"
		}
		return d
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"[lte-scanner]", "claude", "node › sh (dim)", "x", "y"}},
		{"lte", []string{"[lte-scanner]", "claude", "node › sh (dim)", "x", "y"}}, // the header alone matches
		{"claude", []string{"[lte-scanner]", "claude"}},                           // a link the view shows
		{"zsh", []string{"[lte-scanner]", "zsh"}},                                 // a link the view hides
		{"bash", []string{"[lte-scanner]", "claude › bash", "zsh › bash"}},        // the last process itself
		{"node", []string{"[lte-scanner]", "node"}},
		{"x", []string{"[lte-scanner]", "node › sh (dim)", "x"}},
	} {
		m, _ := newTest(t, 160, 30)
		feed(m, s)
		if tc.query != "" {
			press(m, "/")
			typeText(m, tc.query)
		}
		var got []string
		for _, r := range m.rows {
			got = append(got, draw(r))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("/%s: rows %q, want %q\n%s", tc.query, got, tc.want, screen(m))
		}
	}
}

func TestFoldSelection(t *testing.T) {
	// watch 23 is claude's second child, so the first chain ends at claude.
	s := chainFixture()
	watch := s.Processes[1]
	watch.PID, watch.PPID, watch.Name, watch.Argv = 23, 11, "watch", []string{"watch"}
	s.Processes = append(s.Processes, watch)
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	claude := keyOf(s, 11)
	selectRow(t, m, claude)
	if r, _ := m.selected(); len(r.Links) != 1 || line(m, "  ▾ claude ") == "" {
		t.Fatalf("selected %+v:\n%s", r, screen(m))
	}
	// watch exits: claude is folded into the xargs row, which takes the selection.
	feed(m, chainFixture())
	if m.sel != keyOf(s, 16) {
		t.Errorf("selection after claude folded: %+v, want the xargs row", m.sel)
	}
}

func TestFoldKill(t *testing.T) {
	s := chainFixture()
	m, _, fp, _ := newKillTest(t, 120, 30, s)
	selectRow(t, m, keyOf(s, 16))
	press(m, "x")
	if want := []planCall{{keyOf(s, 16), engine.KillOptions{}}}; !slices.Equal(fp.calls, want) {
		t.Errorf("Plan calls %+v, want %+v", fp.calls, want)
	}
	if got, want := line(m, "kill xargs"), "kill xargs (pid 16): process mode, SIGTERM to 1 process"; !strings.Contains(got, want) {
		t.Errorf("title %q, want %q", got, want)
	}
	if got := strings.Fields(line(m, "16 ")); len(got) < 2 || got[0] != "16" || got[1] != "xargs" {
		t.Errorf("plan line %q, want pid 16 alone", got)
	}
}

// planPIDs are the pids the kill modal lists, in order.
func planPIDs(m *Model) []int {
	var pids []int
	for l := range strings.SplitSeq(screen(m), "\n") {
		if f := strings.Fields(l); strings.HasPrefix(l, "  ") && len(f) > 1 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// lteTree is chainFixture's tree below claude 11, as fakePlanner plans it.
var lteTree = []int{11, 12, 13, 14, 15, 16, 17, 22, 18, 19, 21, 20}

// TestFoldKillTree: on a folded row, t plans from the chain's first process as its label draws
// it, so the whole chain is signalled; the hidden bash 10 before claude is neither planned nor
// listed. p goes back to the row's own process, t and f replan from the root (DEV-179).
func TestFoldKillTree(t *testing.T) {
	s := chainFixture()
	m, _, fp, _ := newKillTest(t, 120, 40, s)
	selectRow(t, m, keyOf(s, 16))
	for _, step := range []struct {
		key   string
		call  planCall
		title string
		pids  []int
	}{
		{"t", planCall{keyOf(s, 11), engine.KillOptions{Tree: true}}, "kill claude (pid 11): tree mode, SIGTERM to 12 processes", lteTree},
		{"f", planCall{keyOf(s, 11), engine.KillOptions{Tree: true, Force: true}}, "kill claude (pid 11): tree mode, force, SIGKILL to 12 processes", lteTree},
		{"p", planCall{keyOf(s, 16), engine.KillOptions{Force: true}}, "kill xargs (pid 16): process mode, force, SIGKILL to 1 process", []int{16}},
		{"t", planCall{keyOf(s, 11), engine.KillOptions{Tree: true, Force: true}}, "kill claude (pid 11): tree mode, force, SIGKILL to 12 processes", lteTree},
	} {
		if step.key == "t" && len(fp.calls) == 0 {
			press(m, "x")
		}
		press(m, step.key)
		if got := fp.calls[len(fp.calls)-1]; got != step.call {
			t.Errorf("%s: Plan call %+v, want %+v", step.key, got, step.call)
		}
		if got := line(m, "kill "); got != step.title {
			t.Errorf("%s: title %q, want %q", step.key, got, step.title)
		}
		if got := planPIDs(m); !slices.Equal(got, step.pids) {
			t.Errorf("%s: listed pids %v, want %v\n%s", step.key, got, step.pids, screen(m))
		}
	}
}

// TestFoldKillTreeRoot: the tree's root is the first link the view shows: bash 10 with `a`,
// claude 11 otherwise, a filter that matches the shell included (DEV-191); the row's own
// process on an unfolded chain.
func TestFoldKillTreeRoot(t *testing.T) {
	s := chainFixture()
	for _, tc := range []struct {
		name string
		do   func(m *Model)
		root int
	}{
		{"plain", func(*Model) {}, 11},
		{"show all", func(m *Model) { press(m, "a") }, 10},
		{"filter matching the shell", func(m *Model) { press(m, "/"); typeText(m, "bash"); press(m, "enter") }, 11},
		{"filter matching claude", func(m *Model) { press(m, "/"); typeText(m, "claude"); press(m, "enter") }, 11},
		{"unfolded", func(m *Model) { selectRow(t, m, keyOf(s, 16)); press(m, "right") }, 16},
	} {
		m, _, fp, _ := newKillTest(t, 160, 40, s)
		tc.do(m)
		selectRow(t, m, keyOf(s, 16))
		press(m, "x", "t")
		if got := fp.calls[len(fp.calls)-1].key; got != keyOf(s, tc.root) {
			t.Errorf("%s: tree planned from pid %d, want %d\n%s", tc.name, got.PID, tc.root, screen(m))
		}
		if pids := planPIDs(m); len(pids) == 0 || pids[0] != tc.root || slices.Contains(pids, 10) != (tc.root == 10) {
			t.Errorf("%s: listed pids %v, want from %d\n%s", tc.name, pids, tc.root, screen(m))
		}
	}
}

// TestFoldKillTreeRootFilter (DEV-191): a hidden shell is never the root of a tree kill on a
// folded row, whatever a filter draws: a filter that matches the shell by its argv (drive.py)
// or its name (sh) draws it in the label, and `x`, `t` still plans the same processes as
// without the filter.
func TestFoldKillTreeRootFilter(t *testing.T) {
	s := chainFixture()
	s.Processes[0].Argv = []string{"bash", "-c", "python3 drive.py"} // bash 10
	for _, query := range []string{"", "drive.py", "sh"} {
		m, _, fp, _ := newKillTest(t, 160, 40, s)
		if query != "" {
			press(m, "/")
			typeText(m, query)
			press(m, "enter")
			if !strings.Contains(line(m, "xargs"), " bash › claude › ") {
				t.Fatalf("/%s does not draw the shell in the label:\n%s", query, screen(m))
			}
		}
		selectRow(t, m, keyOf(s, 16))
		press(m, "x", "t")
		if got := fp.calls[len(fp.calls)-1].key; got != keyOf(s, 11) {
			t.Errorf("/%s: tree planned from pid %d, want 11\n%s", query, got.PID, screen(m))
		}
		if got := planPIDs(m); !slices.Equal(got, lteTree) {
			t.Errorf("/%s: listed pids %v, want %v", query, got, lteTree)
		}
	}
}

// TestFoldKillTreeRootRefold (DEV-198): a filter flattens with every process shown (Search), so
// an idle shell the view hides counts as a child again and a chain can fold differently. The
// root of a tree kill is the first link the row names in the view at hand: claude 11 → zsh 12
// → xargs 13 is `claude › xargs` without a filter. When zsh 12 also has an idle zsh 14, /xargs
// splits it into `claude › zsh` and `xargs`, and the xargs row, which names xargs alone, plans
// from xargs; without the sibling the chain folds the same and plans from claude both ways.
func TestFoldKillTreeRootRefold(t *testing.T) {
	proc := func(pid, ppid int, name string, kind model.Kind, argv ...string) model.Process {
		p := chainFixture().Processes[0]
		p.PID, p.PPID, p.Name, p.Argv, p.Kind = pid, ppid, name, append([]string{name}, argv...), kind
		p.StartTime = at(time.Hour - time.Duration(pid)*time.Second)
		return p
	}
	chain := []model.Process{proc(11, 9, "claude", model.KindAgent), proc(12, 11, "zsh", model.KindShell, "mid.zsh"),
		proc(13, 12, "xargs", model.KindOther)}
	idle := proc(14, 12, "zsh", model.KindShell, "-c", "zselect -t 60000")
	for _, tc := range []struct {
		name, query string
		procs       []model.Process
		label       string // the xargs row's name cell
		root        int
		pids        []int
	}{
		{"one child", "", chain, "claude › xargs", 11, []int{11, 12, 13}},
		{"one child", "xargs", chain, "claude › xargs", 11, []int{11, 12, 13}},
		{"idle sibling", "", append(slices.Clip(chain), idle), "claude › xargs", 11, []int{11, 12, 13, 14}},
		{"idle sibling", "xargs", append(slices.Clip(chain), idle), "xargs", 13, []int{13}},
	} {
		s := chainFixture()
		s.Processes = tc.procs
		m, _, fp, _ := newKillTest(t, 160, 40, s)
		if tc.query != "" {
			press(m, "/")
			typeText(m, tc.query)
			press(m, "enter")
		}
		selectRow(t, m, keyOf(s, 13))
		if got := strings.Fields(line(m, " 13 ")); !strings.HasPrefix(strings.Join(got, " "), "▾ "+tc.label+" other") &&
			!strings.HasPrefix(strings.Join(got, " "), tc.label+" other") {
			t.Errorf("%s /%s: xargs row %q, want label %q\n%s", tc.name, tc.query, got, tc.label, screen(m))
		}
		press(m, "x", "t")
		if got := fp.calls[len(fp.calls)-1].key; got != keyOf(s, tc.root) {
			t.Errorf("%s /%s: tree planned from pid %d, want %d\n%s", tc.name, tc.query, got.PID, tc.root, screen(m))
		}
		if got := planPIDs(m); !slices.Equal(got, tc.pids) {
			t.Errorf("%s /%s: listed pids %v, want %v\n%s", tc.name, tc.query, got, tc.pids, screen(m))
		}
	}
}

// TestFoldKillTreeRootCut (DEV-190): when the name column cuts the label to `… › xargs`, the
// root tree mode plans from is not on the row; the modal's title and pid list name it before
// the kill is confirmed.
func TestFoldKillTreeRootCut(t *testing.T) {
	s := chainFixture()
	long := "claude_" + strings.Repeat("c", 31)
	s.Processes[1].Name, s.Processes[1].Argv = long, []string{long}
	m, _, _, _ := newKillTest(t, 80, 24, s)
	if got := line(m, "xargs"); !strings.HasPrefix(got, "  ▾ … › xargs ") {
		t.Fatalf("label not cut: %q", got)
	}
	selectRow(t, m, keyOf(s, 16))
	press(m, "x", "t")
	if got := line(m, "kill "); !strings.HasPrefix(got, "kill claude_c") || !strings.HasSuffix(got, "… (pid 11): tree mode, SIGTERM to 12 processes") {
		t.Errorf("title %q does not name the root", got)
	}
	if got := strings.Fields(line(m, "11 ")); len(got) < 2 || got[0] != "11" || !strings.HasPrefix(got[1], "claude_c") {
		t.Errorf("first pid line %q does not name the root\n%s", got, screen(m))
	}
}

// TestFoldKillTreeRefused: a refusal of the tree from the chain's root shows as the options'
// refusal, and p gets back to the row's own process.
func TestFoldKillTreeRefused(t *testing.T) {
	s := chainFixture()
	m, _, fp, fk := newKillTest(t, 120, 40, s)
	fp.refuse = func(o engine.KillOptions) error {
		if o.Tree {
			return &engine.Refusal{Reason: "process group 11 contains pid 90, which runs devdash"}
		}
		return nil
	}
	selectRow(t, m, keyOf(s, 16))
	press(m, "x", "t")
	if got, want := line(m, "kill "), "kill claude (pid 11): tree mode"; got != want || line(m, "refused: process group 11") == "" {
		t.Errorf("title %q, want %q with the refusal:\n%s", got, want, screen(m))
	}
	if cmd := press(m, "enter"); cmd != nil {
		t.Fatal("confirm on a refused tree returned a command")
	}
	press(m, "p")
	if got, want := line(m, "kill "), "kill xargs (pid 16): process mode, SIGTERM to 1 process"; got != want {
		t.Errorf("after p: title %q, want %q", got, want)
	}
	run(t, m, press(m, "enter"))
	if len(fk.plans) != 1 || len(fk.plans[0].Procs) != 1 || fk.plans[0].Procs[0].PID != 16 {
		t.Errorf("Kill got %+v, want xargs alone", fk.plans)
	}
}

// TestFoldKillTreeResult: a tree killed from a folded row reports by its root, offers force on
// the survivors, and the status adds the ports of every process it stopped.
func TestFoldKillTreeResult(t *testing.T) {
	s := chainFixture()
	s.Processes[6].Listeners = []model.Listener{lis("tcp4", "127.0.0.1", 9000)} // xargs 16
	m, _, _, fk := newKillTest(t, 120, 40, s)
	fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
		return outcomes(p, func(proc model.Process) engine.Outcome {
			return engine.Outcome{Signalled: true, Exited: proc.PID != 11}
		}), nil
	})
	selectRow(t, m, keyOf(s, 16))
	press(m, "x", "t")
	run(t, m, press(m, "enter"))
	if got, want := line(m, "kill "), "kill claude (pid 11): 11 of 12 processes exited after SIGTERM"; got != want {
		t.Fatalf("report title %q, want %q\n%s", got, want, screen(m))
	}
	cmd := press(m, "f")
	if got, want := line(m, "kill "), "kill claude (pid 11): force-kill survivors, SIGKILL to 1 process"; got != want {
		t.Errorf("force title %q, want %q", got, want)
	}
	run(t, m, cmd)
	if m.kill.active() || status(m) != "killed 12 processes" {
		t.Fatalf("modal open or status %q:\n%s", status(m), screen(m))
	}
	feed(m, afterKill(drop(s, lteTree...), time.Second))
	if got, want := status(m), "killed 12 processes · 9000 free"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
}
