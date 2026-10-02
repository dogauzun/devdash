package tui

import (
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// Fixture rows in display order (80x24, default view):
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

	// shop becomes the most recently active group and moves above api.
	s = fixture()
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
		{"the group order changes: its sibling, now further down", keyOf(s, 201), drop(fixture(), 201), 2, keyOf(s, 200), 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 80, 24)
			feed(m, fixture())
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

	s = fixture()
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
