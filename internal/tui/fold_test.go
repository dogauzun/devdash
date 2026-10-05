package tui

import (
	"slices"
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

	// → unfolds the row that shows one name; ← on its first row folds it again.
	selectKey(t, m, viteKey)
	press(m, "right")
	if !m.view.Unfolded[zsh] || !strings.HasPrefix(line(m, "zsh"), "  ▾ zsh ") || !strings.HasPrefix(line(m, "vite"), "      vite ") {
		t.Fatalf("right did not unfold the chain:\n%s", screen(m))
	}
	selectKey(t, m, zsh)
	press(m, "left")
	if m.view.Unfolded[zsh] || m.sel != viteKey || !strings.HasPrefix(line(m, "vite"), "    vite ") {
		t.Fatalf("left did not fold the chain (sel %+v):\n%s", m.sel, screen(m))
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
	selectKey(t, m, xargs)
	press(m, "left")
	if !m.view.Collapsed[xargs] || line(m, "▸ "+chainLine) == "" || line(m, "sleep") != "" {
		t.Fatalf("left did not collapse the folded row:\n%s", screen(m))
	}
	press(m, "right")
	if m.view.Collapsed[xargs] || line(m, "▾ "+chainLine) == "" {
		t.Fatalf("right did not expand the folded row:\n%s", screen(m))
	}
	press(m, "right")
	if !m.view.Unfolded[bash] || line(m, chainLine) != "" || !strings.HasPrefix(line(m, "claude"), "    ▾ claude ") ||
		!strings.HasPrefix(line(m, "xargs"), "              ▾ xargs ") || m.sel != xargs {
		t.Fatalf("right did not unfold the chain with xargs selected (%+v):\n%s", m.sel, screen(m))
	}
	press(m, "right") // nothing more to unfold
	if m.sel != xargs || len(m.view.Unfolded) != 1 {
		t.Errorf("right on an unfolded row: sel %+v, unfolded %v", m.sel, m.view.Unfolded)
	}

	// ← on the first row folds the chain again and selects the folded row; elsewhere it collapses.
	selectKey(t, m, bash)
	press(m, "left")
	if m.view.Unfolded[bash] || line(m, "▾ "+chainLine) == "" || m.sel != xargs {
		t.Fatalf("left on the first row did not fold the chain (sel %+v):\n%s", m.sel, screen(m))
	}
	press(m, "right")
	selectKey(t, m, claude)
	press(m, "left")
	if !m.view.Collapsed[claude] || !m.view.Unfolded[bash] || line(m, "▸ claude") == "" {
		t.Fatalf("left on a link of an unfolded chain did not collapse it:\n%s", screen(m))
	}
	press(m, "left") // claude is collapsed: ← moves to its parent, the first row
	if m.sel != bash {
		t.Fatalf("left on collapsed claude: %+v, want bash", m.sel)
	}
	// A collapsed process ends a chain: folded again, the row stops at claude.
	press(m, "left")
	if got := line(m, "claude"); !strings.HasPrefix(got, "  ▸ claude ") || m.sel != claude {
		t.Errorf("refolded with claude collapsed: %q, sel %+v", got, m.sel)
	}
}

func TestFoldUnfoldedSurvivesRefresh(t *testing.T) {
	s := chainFixture()
	m, _ := newTest(t, 160, 30)
	feed(m, s)
	selectKey(t, m, keyOf(s, 16))
	press(m, "right")
	feed(m, chainFixture())
	if line(m, chainLine) != "" || line(m, "▾ claude") == "" || m.sel != keyOf(s, 16) {
		t.Errorf("unfolded chain refolded on refresh:\n%s", screen(m))
	}
	// Pruned once its first process is gone, as collapsed keys are.
	feed(m, drop(chainFixture(), 10))
	if len(m.view.Unfolded) != 0 {
		t.Errorf("unfolded keys after bash exited: %v", m.view.Unfolded)
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
	selectKey(t, m, claude)
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
