package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-32 owns this file: rebuilding the rows after a snapshot or a view change with the
// selection and expansion kept by row key, and the filter prompt.

// rebuild flattens the latest snapshot with the view options, applies the filter and finds
// the selected row again by its key.
//
// ponytail: placeholder until DEV-32; a selection whose key is gone falls back to the same
// index, not the nearest previous row.
func (m *Model) rebuild() {
	m.all = model.Flatten(m.upd.Snapshot, m.view)
	m.rows = model.Filter(m.all, m.filter)
	for i, r := range m.rows {
		if r.Key == m.sel {
			m.selIdx = i
			return
		}
	}
	m.moveTo(m.selIdx)
	if len(m.rows) == 0 {
		m.selIdx = -1
	}
}

// setFilter sets the filter query and rebuilds the rows.
func (m *Model) setFilter(q string) {
	m.filter = q
	m.rebuild()
}

// filterKey handles keys while the filter prompt is open.
//
// ponytail: placeholder until DEV-32; esc and enter close the prompt, nothing is typed.
func (m *Model) filterKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc", "enter":
		m.filtering = false
	}
	return nil
}
