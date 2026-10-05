package tui

import (
	"maps"
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-32 owns this file: rebuilding the rows after a snapshot or a view change with the
// selection and expansion kept by row key, and the filter prompt.

// rebuild flattens the latest snapshot with the view options, applies the filter and finds
// the selected row again by its key. It runs after every snapshot, view change and filter
// change. While a filter is set the rows are flattened as if nothing were collapsed, so a
// match inside a folded group or under a folded tree node is found (DEV-126); the collapsed
// keys are kept and apply again once the filter is cleared. They are also flattened with
// every row the view toggles hide shown (shells and editors without `a`, container rows with
// `d`), so `/zsh` finds a shell the view hides (Release 1.1, "Search"); searchHidden then keeps
// only the hidden rows the query reaches.
//
// A selected process folded into a chain's row (model.ViewOptions.Fold) selects that row, and
// the row's key becomes the selection (DEV-157). Otherwise, when the key is gone the selection
// moves to the nearest row above it, in the rows as they
// were, that is still shown (spec: "the nearest previous index"), wherever that row now is: a
// process that exits leaves the selection on its sibling or parent, even when its group moves.
// When no row above it is left, the first row is selected. With no rows at all nothing is
// selected (selIdx -1) but the key is kept, so the row is selected again when it comes back (a
// filter that matched nothing is relaxed, an empty snapshot is followed by a full one).
func (m *Model) rebuild() {
	prev, prevIdx := m.rows, m.selIdx
	m.pruneCollapsed()
	view := m.view
	if m.filter != "" {
		view.Collapsed, view.ShowAll, view.HideContainers = nil, true, false
	}
	m.all = model.Flatten(m.upd.Snapshot, view)
	m.rows = model.Filter(m.all, m.filter)
	if m.filter != "" && (!m.view.ShowAll || m.view.HideContainers) {
		m.rows = searchHidden(m.rows, m.filter, m.view)
	}
	if len(m.rows) == 0 {
		m.selIdx = -1
		return
	}
	at := make(map[model.RowKey]int, len(m.rows))
	for i, r := range m.rows {
		at[r.Key] = i
		for _, l := range r.Links { // a process folded into a chain's row selects that row (DEV-157)
			at[l.Key()] = i
		}
	}
	if i, ok := at[m.sel]; ok {
		m.moveTo(i)
		return
	}
	// prev[prevIdx] is the selected row, or the stand-in for it a filter change starts from.
	for j := min(prevIdx, len(prev)-1); j >= 0; j-- {
		if i, ok := at[prev[j].Key]; ok {
			m.moveTo(i)
			return
		}
	}
	m.moveTo(0) // also replaces the stale key
}

// searchHidden takes rows flattened with every row shown and filtered by query, and draws
// them as view would (Release 1.1, "Search"): a row view hides (hiddenBy) is kept when it
// matches query itself, drawn normally, or when a kept row is below it, drawn dimmed as
// Flatten dims a hidden row that connects a visible one; any other is left out, and so is a
// header left with no row under it, as Flatten omits a group with nothing to show. So a query
// that matches a group header shows that group without its idle shells. The rows are in
// preorder, so a backward walk sees a row's descendants before the row.
//
// A folded chain (DEV-157) is judged by its last process, as it is drawn: when the view hides
// it and it neither matches itself nor leads to a match, the row is cut back to its last link
// the view shows or that matches (cutChain), so `claude › bash`, an idle bash, is drawn as the
// view draws claude; with no such link it is left out.
func searchHidden(rows []model.Row, query string, view model.ViewOptions) []model.Row {
	keep := make([]bool, len(rows))
	var below []bool // below[d]: a kept row at depth d since the last row at a smaller depth, walking back
	for i := len(rows) - 1; i >= 0; i-- {
		r := &rows[i]
		below = append(below, make([]bool, max(r.Depth+1-len(below), 0))...)
		desc := slices.Contains(below[r.Depth+1:], true)
		clear(below[r.Depth+1:])
		switch {
		case r.Key.Header != model.GroupNone:
			keep[i] = desc
		case hiddenBy(*r, view) && !desc && r.Links != nil && !model.Match(model.Row{Process: r.Process, Container: r.Container}, query):
			keep[i] = cutChain(r, query, view)
		case hiddenBy(*r, view) && !model.Match(*r, query):
			keep[i], r.Dimmed = desc, true
		default:
			keep[i] = true
		}
		below[r.Depth] = below[r.Depth] || keep[i]
	}
	out := rows[:0:0]
	for i, r := range rows {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// cutChain makes folded row r the row of its last link that view shows or that matches query,
// with the links before it, and reports whether there was one. Links are never container
// rows, so the row loses no container.
func cutChain(r *model.Row, query string, view model.ViewOptions) bool {
	for j := len(r.Links) - 1; j >= 0; j-- {
		l := model.Row{Key: r.Links[j].Key(), Process: r.Links[j]}
		if !hiddenBy(l, view) || model.Match(l, query) {
			l.Depth, l.Links = r.Depth, r.Links[:j:j]
			if j == 0 {
				l.Links = nil
			}
			*r = l
			return true
		}
	}
	return false
}

// hiddenBy reports whether view leaves row r out: a shell or editor without `a`
// (model.Process.Hideable, Flatten's rule), or a container row with `d` (a container with no
// process behind it, or a process holding a container's ports).
func hiddenBy(r model.Row, view model.ViewOptions) bool {
	if view.HideContainers && (r.Container != nil || r.Process != nil && r.Process.ContainerID != "") {
		return true
	}
	return !view.ShowAll && r.Process != nil && r.Process.Hideable()
}

// pruneCollapsed drops the collapsed and unfolded keys of rows that are no longer in the
// snapshot, so the maps do not grow over hours of processes coming and going. Membership is
// the snapshot's (processes, projects, compose projects, containers), not the rows': the
// children of a collapsed row are absent from the rows yet keep their own state. The
// containers and other headers are two fixed keys and always kept. Nothing is pruned before
// the first snapshot.
func (m *Model) pruneCollapsed() {
	if !m.have || len(m.view.Collapsed)+len(m.view.Unfolded) == 0 {
		return
	}
	s := m.upd.Snapshot
	live := make(map[model.RowKey]bool, len(s.Processes)+len(s.Projects)+2*len(s.Containers))
	for _, p := range s.Processes {
		live[p.Key()] = true
	}
	for _, p := range s.Projects {
		live[model.RowKey{Header: model.GroupProject, Group: p.ID}] = true
	}
	for _, c := range s.Containers {
		live[model.RowKey{ContainerID: c.ID}] = true
		if c.ComposeProject != "" {
			live[model.RowKey{Header: model.GroupCompose, Group: c.ComposeProject}] = true
		}
	}
	maps.DeleteFunc(m.view.Collapsed, func(k model.RowKey, _ bool) bool {
		return !live[k] && k.Header != model.GroupContainers && k.Header != model.GroupOther
	})
	maps.DeleteFunc(m.view.Unfolded, func(k model.RowKey, _ bool) bool { return !live[k] })
}

// filterSel keeps the row the user chose while filter changes hide it.
type filterSel struct {
	chosen model.RowKey // the selection the user (or a refresh) made
	shown  model.RowKey // the selection the last filter change left: chosen, or a stand-in
}

// setFilter sets the filter query and rebuilds the rows. The selection goes back to the row
// chosen before the filter hid it as soon as that row is shown again, whether the query is
// cleared, shortened or retyped; a selection moved since the last filter change (by a key or
// a refresh) is the new choice. A query that changes to a port number selects the port's
// holder instead (portSearch), a stand-in like any other, and the command returned probes for
// the port line.
func (m *Model) setFilter(q string) tea.Cmd {
	if m.sel != m.fsel.shown {
		m.fsel.chosen = m.sel
	}
	changed := q != m.filter
	m.filter = q
	m.sel = m.fsel.chosen
	m.rebuild()
	var cmd tea.Cmd
	if changed {
		cmd = m.portSearch()
	}
	m.fsel.shown = m.sel
	return cmd
}

// paste appends pasted text to the filter query while the prompt is open, without its
// newlines and other control characters; it is ignored otherwise.
func (m *Model) paste(text string) tea.Cmd {
	text = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, text)
	if m.filtering && text != "" {
		return m.setFilter(m.filter + text)
	}
	return nil
}

// filterKey handles keys while the filter prompt is open; the rows are filtered as the query
// is typed. Printable text is added to the query, backspace deletes its last character,
// ctrl+u clears it, enter closes the prompt and keeps the filter, esc closes it and clears
// the filter. Every other key is ignored, so letters such as q and j are typed, not obeyed.
func (m *Model) filterKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "enter":
		m.filtering = false
	case "esc":
		m.filtering = false
		return m.setFilter("")
	case "backspace":
		if q := []rune(m.filter); len(q) > 0 {
			return m.setFilter(string(q[:len(q)-1]))
		}
	case "ctrl+u":
		return m.setFilter("")
	default:
		if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			return m.setFilter(m.filter + k.Text)
		}
	}
	return nil
}
