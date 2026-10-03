package tui

import (
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// Fixture rows in display order (80x24, default view, with other opened: it starts collapsed):
//
//	0 api            7   claude
//	1   api 200      8 shop (compose)
//	2   go 201       9   shop-web-1
//	3 shop          10   shop-db-1 (300)
//	4   zsh 100     11 other
//	5     node 101  12   unknown
//	6       esbuild 13   sshd 1

var (
	rowsAPIHeader     = model.RowKey{Header: model.GroupProject, Group: apiID}
	rowsShopHeader    = model.RowKey{Header: model.GroupProject, Group: shopID}
	rowsComposeHeader = model.RowKey{Header: model.GroupCompose, Group: "shop"}
	rowsOtherHeader   = model.RowKey{Header: model.GroupOther}
	webKey            = model.RowKey{ContainerID: "4e5d6c7b8a90"}
)

// drop returns s without the processes with the given pids (0 drops the unknown owner).
func drop(s model.Snapshot, pids ...int) model.Snapshot {
	s.Processes = slices.DeleteFunc(slices.Clone(s.Processes), func(p model.Process) bool {
		return slices.Contains(pids, p.PID)
	})
	return s
}

// notHere returns s with no Here project, so the groups follow the activity order alone.
func notHere(s model.Snapshot) model.Snapshot {
	s.Projects = slices.Clone(s.Projects)
	for i := range s.Projects {
		s.Projects[i].Here = false
	}
	return s
}

// withoutAPI returns s without the api project and its processes.
func withoutAPI(s model.Snapshot) model.Snapshot {
	s = drop(s, 200, 201, 202)
	s.Projects = slices.DeleteFunc(slices.Clone(s.Projects), func(p model.Project) bool { return p.ID == apiID })
	return s
}

// rowsKeys returns the keys of the rows the table shows.
func rowsKeys(m *Model) []model.RowKey {
	ks := make([]model.RowKey, len(m.rows))
	for i, r := range m.rows {
		ks[i] = r.Key
	}
	return ks
}

// rowsSelect moves the selection onto k with the arrow keys.
func rowsSelect(t *testing.T, m *Model, k model.RowKey) {
	t.Helper()
	i := slices.Index(rowsKeys(m), k)
	if i < 0 {
		t.Fatalf("rowsSelect: %+v is not a row", k)
	}
	for range m.rows {
		press(m, "up")
	}
	for range i {
		press(m, "down")
	}
	if m.sel != k || m.selIdx != i {
		t.Fatalf("rowsSelect: selected %+v at %d, want %+v at %d", m.sel, m.selIdx, k, i)
	}
}

// wantSel checks that the selection is k at index i, and that sel and selIdx agree.
func wantSel(t *testing.T, m *Model, k model.RowKey, i int) {
	t.Helper()
	if m.sel != k || m.selIdx != i {
		t.Errorf("selected %+v at %d, want %+v at %d", m.sel, m.selIdx, k, i)
	}
	if r, ok := m.selected(); !ok || r.Key != m.sel {
		t.Errorf("selected() = %+v, %v; sel is %+v", r.Key, ok, m.sel)
	}
}

func TestSelectionFirstSnapshot(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	if _, ok := m.selected(); ok || m.selIdx != -1 {
		t.Fatalf("before the first snapshot: selIdx %d, want -1 and nothing selected", m.selIdx)
	}
	feed(m, fixture())
	wantSel(t, m, rowsAPIHeader, 0)
}

func TestSelectionHoldsAcrossRefresh(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	goTest := keyOf(s, 201)
	rowsSelect(t, m, goTest)

	// The row above exits: the index changes, the selection does not.
	feed(m, drop(fixture(), 200))
	wantSel(t, m, goTest, 1)

	// A new listener in the same project sorts above it.
	s = fixture()
	srv := s.Processes[4] // api 200
	srv.PID, srv.StartTime, srv.Listeners = 210, at(time.Second), []model.Listener{lis("tcp4", "127.0.0.1", 9000)}
	s.Processes = append(s.Processes, srv)
	feed(m, s)
	wantSel(t, m, goTest, 3)

	// shop becomes the most recently active group and moves above api, once api is not the
	// here project.
	s = notHere(fixture())
	s.Processes[3].StartTime = at(time.Second) // claude
	feed(m, s)
	if m.rows[0].Key != rowsShopHeader {
		t.Fatalf("shop is not first: %+v", m.rows[0].Key)
	}
	wantSel(t, m, goTest, 7)
}

// When the selected row is gone the selection moves to the nearest row above it (in the rows
// before the refresh) that is still shown, wherever that row now is; the first row when none is.
func TestSelectedProcessExits(t *testing.T) {
	s := fixture()
	for _, tc := range []struct {
		name    string
		sel     model.RowKey
		next    model.Snapshot
		idx     int          // the selection's index before the refresh
		want    model.RowKey // the nearest row above it that is still shown
		wantIdx int          // where that row is now
	}{
		{"middle row: its parent", keyOf(s, 200), drop(fixture(), 200), 1, rowsAPIHeader, 0},
		{"last row: the row above it", keyOf(s, 1), drop(fixture(), 1), 13, keyOf(s, 0), 12},
		{"its whole group exits: the last row of the group above", keyOf(s, 103), dropShop(fixture()), 7, keyOf(s, 201), 2},
		{"a header whose group exits: nothing above, the first row", rowsAPIHeader, withoutAPI(fixture()), 0, rowsShopHeader, 0},
		// api loses its newest process and shop sorts first: the selection stays in api.
		{"the group order changes: its sibling, now further down", keyOf(s, 201), notHere(drop(fixture(), 201)), 2, keyOf(s, 200), 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 80, 24)
			feed(m, fixture())
			openOther(m)
			rowsSelect(t, m, tc.sel)
			if m.selIdx != tc.idx {
				t.Fatalf("fixture changed: %+v at %d, want %d", tc.sel, m.selIdx, tc.idx)
			}
			feed(m, tc.next)
			wantSel(t, m, tc.want, tc.wantIdx)
			// The new selection is a key like any other: the next refresh keeps it.
			feed(m, tc.next)
			wantSel(t, m, tc.want, tc.wantIdx)
		})
	}
}

// A view change that hides the selected row and the row above it: the selection moves to the
// nearest row above that is still shown, not into the other group.
func TestSelectionHiddenByView(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	rowsSelect(t, m, webKey) // index 9, under the shop compose header
	m.view.HideContainers = true
	m.rebuild()
	wantSel(t, m, keyOf(s, 103), 7) // claude
}

// dropShop returns s without the shop project and its processes.
func dropShop(s model.Snapshot) model.Snapshot {
	s = drop(s, 100, 101, 102, 103)
	s.Projects = slices.DeleteFunc(slices.Clone(s.Projects), func(p model.Project) bool { return p.ID == shopID })
	return s
}

func TestSelectionHeaderHolds(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	feed(m, fixture())
	rowsSelect(t, m, rowsShopHeader)
	feed(m, withoutAPI(fixture()))
	wantSel(t, m, rowsShopHeader, 0)
	feed(m, fixture())
	wantSel(t, m, rowsShopHeader, 3)
}

// A reused pid with a new start time is another process: it does not get the selection, even
// though a match by pid alone would find it.
func TestSelectionPIDReuse(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	old := keyOf(s, 201)
	rowsSelect(t, m, old)
	m.view.Collapsed[old] = true

	s = notHere(fixture())
	reused := &s.Processes[5] // go test 201
	reused.StartTime, reused.ProjectID, reused.Cwd, reused.Argv = at(time.Second), shopID, shopID, []string{"go", "run", "."}
	feed(m, s)
	if m.rows[0].Key != rowsShopHeader {
		t.Fatalf("shop is not first: %+v", m.rows[0].Key)
	}
	if m.sel == old || m.sel == reused.Key() {
		t.Errorf("selection followed the pid: %+v", m.sel)
	}
	wantSel(t, m, keyOf(s, 200), slices.Index(rowsKeys(m), keyOf(s, 200))) // the row that was above it
	if m.view.Collapsed[old] {
		t.Error("the exited process's collapsed key was not pruned")
	}
}

// When there are no rows nothing is selected, but the key is kept and found again.
func TestSelectionSurvivesEmptySnapshot(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	rowsSelect(t, m, keyOf(s, 201))
	feed(m, model.Snapshot{SchemaVersion: 1, TakenAt: at(time.Second), Host: s.Host})
	if _, ok := m.selected(); ok || m.selIdx != -1 {
		t.Fatalf("empty snapshot: selIdx %d, want -1", m.selIdx)
	}
	feed(m, fixture())
	wantSel(t, m, keyOf(s, 201), 2)
}

func TestCollapseSurvivesRefresh(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	vite := keyOf(s, 101)
	m.view.Collapsed[rowsShopHeader] = true
	m.view.Collapsed[vite] = true // absent from the rows while shop is collapsed, and kept
	m.rebuild()

	for i, next := range []model.Snapshot{fixture(), drop(fixture(), 200), fixture()} {
		next.TakenAt = at(time.Duration(i) * time.Second)
		feed(m, next)
		if i := slices.Index(rowsKeys(m), rowsShopHeader); i < 0 || i+1 < len(m.rows) && m.rows[i+1].Depth != 0 {
			t.Fatalf("snapshot %d: shop is not a collapsed header: %+v", i, rowsKeys(m))
		}
		if line(m, "claude") != "" || line(m, "node") != "" {
			t.Errorf("snapshot %d: a collapsed group's rows are on screen:\n%s", i, screen(m))
		}
		if !m.view.Collapsed[rowsShopHeader] || !m.view.Collapsed[vite] {
			t.Errorf("snapshot %d: collapsed keys lost: %v", i, m.view.Collapsed)
		}
	}

	delete(m.view.Collapsed, rowsShopHeader)
	m.rebuild()
	if line(m, "node") == "" || line(m, "esbuild") != "" {
		t.Errorf("shop expanded: node is collapsed, so esbuild is hidden and node shown:\n%s", screen(m))
	}
}

func TestCollapsedPruned(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	containersHeader := model.RowKey{Header: model.GroupContainers}
	zsh, vite := keyOf(s, 100), keyOf(s, 101)
	m.view.Collapsed[vite] = true
	m.rebuild()
	if !m.view.Collapsed[vite] {
		t.Fatal("pruned before the first snapshot")
	}

	feed(m, s)
	for _, k := range []model.RowKey{rowsShopHeader, zsh, rowsAPIHeader, rowsComposeHeader, webKey, rowsOtherHeader, containersHeader} {
		m.view.Collapsed[k] = true
	}
	m.rebuild()

	next := drop(withoutAPI(fixture()), 101)
	next.Containers = nil
	feed(m, next)
	want := map[model.RowKey]bool{
		rowsShopHeader: true, zsh: true, // zsh is under a collapsed header: not a row, still in the snapshot
		rowsOtherHeader: true, containersHeader: true, // fixed keys
	}
	if len(m.view.Collapsed) != len(want) {
		t.Errorf("collapsed after pruning: %v, want %v", m.view.Collapsed, want)
	}
	for k := range want {
		if !m.view.Collapsed[k] {
			t.Errorf("%+v was pruned", k)
		}
	}
}

func TestFilterPrompt(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	all := len(m.rows)
	vite := []model.RowKey{rowsShopHeader, keyOf(s, 100), keyOf(s, 101)}

	press(m, "/")
	if !m.filtering || line(m, "· /_") == "" {
		t.Fatalf("/ does not open the prompt:\n%s", screen(m))
	}
	typeText(m, "vite")
	if line(m, "· /vite_") == "" {
		t.Errorf("header does not show the query being typed:\n%s", screen(m))
	}
	if got := rowsKeys(m); !slices.Equal(got, vite) {
		t.Errorf("rows while typing %q: %+v, want %+v", m.filter, got, vite)
	}
	press(m, "backspace")
	if m.filter != "vit" || !slices.Equal(rowsKeys(m), vite) {
		t.Errorf("backspace: filter %q, rows %+v", m.filter, rowsKeys(m))
	}
	press(m, "enter")
	if m.filtering || m.filter != "vit" || line(m, "· filter: vit") == "" {
		t.Errorf("enter must close the prompt and keep the filter: filtering %v, filter %q\n%s", m.filtering, m.filter, screen(m))
	}
	// The filter applies to every new snapshot.
	feed(m, fixture())
	if !slices.Equal(rowsKeys(m), vite) {
		t.Errorf("filter lost on refresh: %+v", rowsKeys(m))
	}
	press(m, "esc")
	if m.filter != "" || len(m.rows) != all || line(m, "filter:") != "" {
		t.Errorf("esc after the prompt: filter %q, %d rows, want none and %d", m.filter, len(m.rows), all)
	}

	// esc in the prompt closes it and clears the filter.
	press(m, "/")
	typeText(m, "zz")
	press(m, "esc")
	if m.filtering || m.filter != "" || len(m.rows) != all {
		t.Errorf("esc in the prompt: filtering %v, filter %q, %d rows", m.filtering, m.filter, len(m.rows))
	}

	// ctrl+u clears the query and keeps the prompt; modified keys are not typed; letters
	// that are keys elsewhere are typed.
	press(m, "/")
	typeText(m, "abc")
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if !m.filtering || m.filter != "" || len(m.rows) != all {
		t.Errorf("ctrl+u: filtering %v, filter %q, %d rows", m.filtering, m.filter, len(m.rows))
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x", Mod: tea.ModAlt})
	press(m, "backspace") // on an empty query: nothing
	typeText(m, "sQ")
	if m.filter != "sQ" {
		t.Errorf("filter %q, want %q", m.filter, "sQ")
	}
	if cmd := press(m, "enter"); cmd != nil {
		t.Errorf("enter returned a command")
	}
}

func TestFilterKeepsAncestors(t *testing.T) {
	s := fixture()
	for _, tc := range []struct {
		query  string
		header string // the group header's name, on screen
		want   []model.RowKey
	}{
		{"51", "shop", []model.RowKey{rowsShopHeader, keyOf(s, 100), keyOf(s, 101)}}, // port prefix of 5173
		{"808", "api", []model.RowKey{rowsAPIHeader, keyOf(s, 200)}},                 // 8080 and 8081
		{"ESBUILD", "shop", []model.RowKey{rowsShopHeader, keyOf(s, 100), keyOf(s, 101), keyOf(s, 102)}},
		{"--service", "shop", []model.RowKey{rowsShopHeader, keyOf(s, 100), keyOf(s, 101), keyOf(s, 102)}}, // argv
		{"api", "api", []model.RowKey{rowsAPIHeader, keyOf(s, 200), keyOf(s, 201)}},                        // a project keeps its group
		{"postgres", "shop", []model.RowKey{rowsComposeHeader, keyOf(s, 300)}},                             // container image
		{"shop-web", "shop", []model.RowKey{rowsComposeHeader, webKey}},                                    // container name
	} {
		t.Run(tc.query, func(t *testing.T) {
			m, _ := newTest(t, 80, 24)
			feed(m, fixture())
			press(m, "/")
			typeText(m, tc.query)
			press(m, "enter")
			if got := rowsKeys(m); !slices.Equal(got, tc.want) {
				t.Errorf("rows %+v, want %+v", got, tc.want)
			}
			for i, r := range m.rows {
				if i == 0 && r.Depth != 0 || i > 0 && r.Depth > m.rows[i-1].Depth+1 {
					t.Errorf("row %d (depth %d) has no parent above it", i, r.Depth)
				}
			}
			if line(m, tc.header) == "" {
				t.Errorf("the group header %q is not on screen:\n%s", tc.header, screen(m))
			}
		})
	}
}

func TestFilterSelection(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	vite := keyOf(s, 101)
	rowsSelect(t, m, vite)

	// The selection stays on its row while it matches.
	press(m, "/")
	typeText(m, "nod")
	wantSel(t, m, vite, 2)
	// Nothing matches: nothing is selected, the key is kept.
	typeText(m, "x")
	if _, ok := m.selected(); ok || m.selIdx != -1 || m.sel != vite {
		t.Errorf("no match: sel %+v at %d, want %+v kept at -1", m.sel, m.selIdx, vite)
	}
	press(m, "backspace")
	wantSel(t, m, vite, 2)
	press(m, "esc")
	wantSel(t, m, vite, 5)

	// The filter hides the selected row: the nearest row above it that matches is selected
	// while the filter is set, and the row comes back when the filter is cleared.
	claude := keyOf(s, 103) // index 7
	rowsSelect(t, m, claude)
	press(m, "/")
	typeText(m, "api")
	wantSel(t, m, keyOf(s, 201), 2)
	press(m, "enter", "esc")
	wantSel(t, m, claude, 7)

	// Typed, narrowed to nothing, then cancelled in the prompt.
	press(m, "/")
	typeText(m, "apz")
	press(m, "esc")
	wantSel(t, m, claude, 7)

	// Backspace back past the queries that hid it, and ctrl+u.
	press(m, "/")
	typeText(m, "api")
	press(m, "backspace", "backspace", "backspace")
	wantSel(t, m, claude, 7)
	typeText(m, "api")
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	wantSel(t, m, claude, 7)
	press(m, "esc")

	// A selection made while the filter is set is the one kept when it is cleared.
	press(m, "/")
	typeText(m, "api")
	press(m, "enter", "up")
	wantSel(t, m, keyOf(s, 200), 1)
	press(m, "esc")
	wantSel(t, m, keyOf(s, 200), 1)

	// The chosen row exits while the filter hides it: clearing the filter keeps the row shown.
	rowsSelect(t, m, claude)
	press(m, "/")
	typeText(m, "api")
	press(m, "enter")
	feed(m, drop(fixture(), 103))
	press(m, "esc")
	wantSel(t, m, keyOf(s, 201), 2)
}

// Pasted text goes into the filter query, without its newlines; outside the prompt it is
// ignored.
func TestFilterPaste(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	feed(m, fixture())
	m.Update(tea.PasteMsg{Content: "vite"})
	if m.filter != "" {
		t.Fatalf("paste outside the prompt set the filter to %q", m.filter)
	}
	press(m, "/")
	typeText(m, "v")
	m.Update(tea.PasteMsg{Content: "it\r\ne\n"})
	if m.filter != "vite" || line(m, "· /vite_") == "" {
		t.Errorf("filter %q after paste, want %q:\n%s", m.filter, "vite", screen(m))
	}
}

func TestFilterTags(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	api := []model.RowKey{rowsAPIHeader, keyOf(s, 200)} // api 200 is orphaned and its cwd deleted
	for _, q := range []string{"orphaned", "cwd deleted", "cwd_deleted"} {
		press(m, "/")
		typeText(m, q)
		press(m, "enter")
		if got := rowsKeys(m); !slices.Equal(got, api) {
			t.Errorf("/%s: rows %+v, want %+v", q, got, api)
		}
		press(m, "/", "esc")
	}
}

// The filter searches every row, folded or not (DEV-126): a match inside a collapsed group or
// under a collapsed tree node is shown with its ancestors, drawn open, and clearing the filter
// brings the folds back as they were.
func TestFilterSeesCollapsed(t *testing.T) {
	s := fixture()
	zsh, vite := keyOf(s, 100), keyOf(s, 101)
	for _, tc := range []struct {
		name      string
		collapsed []model.RowKey
		query     string
		want      []model.RowKey
		open      string // a line drawn with the open marker while the filter is set
	}{
		{"port in a collapsed group", []model.RowKey{rowsShopHeader}, "5173",
			[]model.RowKey{rowsShopHeader, zsh, vite}, "▾ shop @ feat/cart"},
		{"port under a collapsed node", []model.RowKey{zsh}, "5173",
			[]model.RowKey{rowsShopHeader, zsh, vite}, "▾ zsh"},
		{"port under nested folds", []model.RowKey{rowsShopHeader, zsh, vite}, "esbuild",
			[]model.RowKey{rowsShopHeader, zsh, vite, keyOf(s, 102)}, "▾ node"},
		{"tag in a collapsed group", []model.RowKey{rowsAPIHeader}, "orphaned",
			[]model.RowKey{rowsAPIHeader, keyOf(s, 200)}, "▾ api @ main (here)"},
		{"a collapsed group that matches keeps its rows", []model.RowKey{rowsAPIHeader}, "api",
			[]model.RowKey{rowsAPIHeader, keyOf(s, 200), keyOf(s, 201)}, "▾ api @ main (here)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 120, 30)
			feed(m, fixture())
			for _, k := range tc.collapsed {
				m.view.Collapsed[k] = true
			}
			m.rebuild()
			folded := rowsKeys(m)

			press(m, "/")
			typeText(m, tc.query)
			if got := rowsKeys(m); !slices.Equal(got, tc.want) {
				t.Fatalf("/%s: rows %+v, want %+v", tc.query, got, tc.want)
			}
			if line(m, tc.open) == "" || line(m, "▸") != "" {
				t.Errorf("/%s: want %q drawn open and no closed marker:\n%s", tc.query, tc.open, screen(m))
			}
			press(m, "esc")
			if got := rowsKeys(m); !slices.Equal(got, folded) {
				t.Errorf("filter cleared: rows %+v, want the folded rows %+v", got, folded)
			}
			for _, k := range tc.collapsed {
				if !m.view.Collapsed[k] {
					t.Errorf("filter cleared: %+v is no longer collapsed", k)
				}
			}
		})
	}
}

// rowsNotes is the header of the notes project, which only an editor references.
var rowsNotes = model.RowKey{Header: model.GroupProject, Group: "/src/notes"}

// withHidden returns the fixture with an idle shell in shop (zsh 104, no children) and a notes
// project whose only process is an editor (nvim 400), so neither shows in the default view.
func withHidden() model.Snapshot {
	s := fixture()
	idle := s.Processes[0] // zsh 100
	idle.PID, idle.StartTime, idle.Argv = 104, at(time.Hour), []string{"-zsh"}
	notes := s.Processes[6] // nvim 202
	notes.PID, notes.ProjectID, notes.Cwd, notes.Argv = 400, "/src/notes", "/src/notes", []string{"nvim", "todo.md"}
	s.Processes = append(slices.Clone(s.Processes), idle, notes)
	s.Projects = append(slices.Clone(s.Projects), model.Project{ID: "/src/notes", Root: "/src/notes", Name: "notes", Branch: "main"})
	return s
}

// The filter searches the rows the view hides (Release 1.1, "Search"): shells and editors
// without `a`, container rows with `d`. One that matches is drawn normally, one that leads to
// a match is dimmed, and the others are left out, along with a group left with no row; with
// `a` on and `d` off every row is drawn as the view draws it.
func TestFilterSeesHiddenKinds(t *testing.T) {
	s := withHidden()
	zsh, idle, nvim, notesNvim, proxy := keyOf(s, 100), keyOf(s, 104), keyOf(s, 202), keyOf(s, 400), keyOf(s, 300)
	shopRows := []model.RowKey{rowsShopHeader, zsh, keyOf(s, 101), keyOf(s, 102), keyOf(s, 103)}
	for _, tc := range []struct {
		name   string
		keys   []string // view toggles pressed before the search
		query  string
		want   []model.RowKey
		dimmed []model.RowKey // the rows drawn dimmed; every other row is drawn normally
	}{
		{"a shell", nil, "zsh", []model.RowKey{rowsShopHeader, zsh, idle}, nil},
		{"an editor", nil, "nvim", []model.RowKey{rowsAPIHeader, nvim, rowsNotes, notesNvim}, nil},
		{"an editor by argv", nil, "todo", []model.RowKey{rowsNotes, notesNvim}, nil},
		{"a shell that leads to a match", nil, "5173", []model.RowKey{rowsShopHeader, zsh, keyOf(s, 101)}, []model.RowKey{zsh}},
		// The project name matches: the group as the view shows it, the idle shell left out.
		{"a group", nil, "shop", append(slices.Clone(shopRows), rowsComposeHeader, webKey, proxy), []model.RowKey{zsh}},
		// A group whose only rows are hidden is left out, as the view leaves it out.
		{"a group of hidden rows", nil, "notes", nil, nil},
		{"a on: a group", []string{"a"}, "shop",
			[]model.RowKey{rowsShopHeader, zsh, keyOf(s, 101), keyOf(s, 102), idle, keyOf(s, 103), rowsComposeHeader, webKey, proxy}, nil},
		{"a on: a group of hidden rows", []string{"a"}, "notes", []model.RowKey{rowsNotes, notesNvim}, nil},
		{"a on: a shell", []string{"a"}, "zsh", []model.RowKey{rowsShopHeader, zsh, idle}, nil},
		{"d on: a published port", []string{"d"}, "8000", []model.RowKey{rowsComposeHeader, webKey}, nil},
		{"d on: a container's process", []string{"d"}, "5432", []model.RowKey{rowsComposeHeader, proxy}, nil},
		{"d on: an image", []string{"d"}, "nginx", []model.RowKey{rowsComposeHeader, webKey}, nil},
		{"d on: a shell", []string{"d"}, "zsh", []model.RowKey{rowsShopHeader, zsh, idle}, nil},
		{"a and d on: a group", []string{"a", "d"}, "shop",
			[]model.RowKey{rowsShopHeader, zsh, keyOf(s, 101), keyOf(s, 102), idle, keyOf(s, 103), rowsComposeHeader, webKey, proxy}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 120, 30)
			feed(m, s)
			press(m, tc.keys...)
			press(m, "/")
			typeText(m, tc.query)
			if got := rowsKeys(m); !slices.Equal(got, tc.want) {
				t.Fatalf("/%s: rows %+v, want %+v\n%s", tc.query, got, tc.want, screen(m))
			}
			for _, r := range m.rows {
				if want := slices.Contains(tc.dimmed, r.Key); r.Dimmed != want {
					t.Errorf("/%s: %+v dimmed %v, want %v", tc.query, r.Key, r.Dimmed, want)
				}
			}
			if tc.want == nil && line(m, "nothing to show") == "" {
				t.Errorf("/%s: no rows, want the empty message:\n%s", tc.query, screen(m))
			}
			// Clearing the filter hides them again.
			press(m, "esc")
			for _, k := range []model.RowKey{idle, nvim, webKey, proxy} {
				if hidden := (k == idle || k == nvim) && !m.view.ShowAll || (k == webKey || k == proxy) && m.view.HideContainers; hidden && slices.Contains(rowsKeys(m), k) {
					t.Errorf("filter cleared: hidden row %+v still shown: %+v", k, rowsKeys(m))
				}
			}
		})
	}
}

// A header that only the search shows (the view omits its whole group) counts its rows as
// the search finds them, not zero; a header the view shows keeps the view's counts (PR #96).
func TestFilterHiddenGroupCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		keys  []string
		query string
		want  string
	}{
		{"d on: a compose group", []string{"d"}, "8000", "▾ shop (compose) · 2 containers · 2 ports"},
		{"a group of editors", nil, "todo", "▾ notes @ main · 1 process · 0 ports"},
		{"a shown group keeps the view's counts", nil, "zsh", "▾ shop @ feat/cart (worktree) · 4 processes · 1 port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 120, 30)
			feed(m, withHidden())
			press(m, tc.keys...)
			press(m, "/")
			typeText(m, tc.query)
			if line(m, tc.want) == "" {
				t.Errorf("/%s: no header %q:\n%s", tc.query, tc.want, screen(m))
			}
		})
	}
}

// The other group starts collapsed (Release 1.1): its header still counts its rows, → opens
// it, and a search finds the rows inside it and leaves it folded once cleared.
func TestOtherStartsCollapsed(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	if got := rowsKeys(m); got[len(got)-1] != rowsOtherHeader {
		t.Fatalf("other is not a folded last row: %+v", got)
	}
	if line(m, "▸ other · 2 processes · 2 ports") == "" || line(m, "sshd") != "" || line(m, "*631") != "" {
		t.Errorf("other is not drawn folded with its counts:\n%s", screen(m))
	}

	press(m, "/")
	typeText(m, "sshd")
	if got, want := rowsKeys(m), []model.RowKey{rowsOtherHeader, keyOf(s, 1)}; !slices.Equal(got, want) {
		t.Errorf("/sshd: rows %+v, want %+v", got, want)
	}
	press(m, "esc")
	if line(m, "▸ other") == "" || line(m, "sshd") != "" {
		t.Errorf("filter cleared: other is not folded again:\n%s", screen(m))
	}

	rowsSelect(t, m, rowsOtherHeader)
	press(m, "right")
	if m.view.Collapsed[rowsOtherHeader] || line(m, "▾ other · 2 processes · 2 ports") == "" || line(m, "sshd") == "" || line(m, "*631") == "" {
		t.Errorf("→ did not open other:\n%s", screen(m))
	}
}

// While a filter is set ← only moves to the parent row and → does nothing: a fold would not
// show until the filter is cleared (DEV-126).
func TestFilterNoFolding(t *testing.T) {
	m, _ := newTest(t, 120, 30)
	s := fixture()
	feed(m, s)
	zsh, vite := keyOf(s, 100), keyOf(s, 101)
	m.view.Collapsed[rowsShopHeader] = true
	m.rebuild()

	press(m, "/")
	typeText(m, "esbuild")
	press(m, "enter")
	rowsSelect(t, m, vite)
	press(m, "left")
	wantSel(t, m, zsh, 1)
	press(m, "left", "left")
	wantSel(t, m, rowsShopHeader, 0)
	press(m, "right", "down", "right")
	if len(m.view.Collapsed) != 2 || !m.view.Collapsed[rowsShopHeader] || !m.view.Collapsed[rowsOtherHeader] {
		t.Errorf("collapsed under a filter: %v, want only the shop header and other (folded at start)", m.view.Collapsed)
	}
	if got := len(m.rows); got != 4 {
		t.Errorf("%d rows, want shop, zsh, node and esbuild:\n%s", got, screen(m))
	}

	// The selection made under the filter is inside the folded group: clearing the filter
	// selects its nearest shown ancestor, the shop header.
	press(m, "esc")
	wantSel(t, m, rowsShopHeader, slices.Index(rowsKeys(m), rowsShopHeader))
}
