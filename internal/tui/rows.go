package tui

import (
	"maps"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-32 owns this file: rebuilding the rows after a snapshot or a view change with the
// selection and expansion kept by row key, and the filter prompt.

// rebuild flattens the latest snapshot with the view options, applies the filter and finds
// the selected row again by its key. It runs after every snapshot, view change and filter
// change.
//
// When the key is gone the selection moves to the row now at the index it had, clamped to
// the last row; the very first rows select the first one. With no rows at all nothing is
// selected (selIdx -1) but the key is kept, so the row is selected again when it comes back
// (a filter that matched nothing is relaxed, an empty snapshot is followed by a full one).
func (m *Model) rebuild() {
	m.pruneCollapsed()
	m.all = model.Flatten(m.upd.Snapshot, m.view)
	m.rows = model.Filter(m.all, m.filter)
	for i, r := range m.rows {
		if r.Key == m.sel {
			m.selIdx = i
			return
		}
	}
	if len(m.rows) == 0 {
		m.selIdx = -1
		return
	}
	m.moveTo(m.selIdx) // also replaces the stale key; -1 (no rows before) clamps to the first row
}

// pruneCollapsed drops the collapsed keys of rows that are no longer in the snapshot, so the
// map does not grow over hours of processes coming and going. Membership is the snapshot's
// (processes, projects, compose projects, containers), not the rows': the children of a
// collapsed row are absent from the rows yet keep their own state. The containers and other
// headers are two fixed keys and always kept. Nothing is pruned before the first snapshot.
func (m *Model) pruneCollapsed() {
	if !m.have || len(m.view.Collapsed) == 0 {
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
}

// setFilter sets the filter query and rebuilds the rows.
func (m *Model) setFilter(q string) {
	m.filter = q
	m.rebuild()
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
		m.setFilter("")
	case "backspace":
		if q := []rune(m.filter); len(q) > 0 {
			m.setFilter(string(q[:len(q)-1]))
		}
	case "ctrl+u":
		m.setFilter("")
	default:
		if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			m.setFilter(m.filter + k.Text)
		}
	}
	return nil
}
