package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-31 owns this file: columns by priority and width, project headers, container rows,
// dimmed rows, scrolling (m.top), movement, collapse and expand, and the a, d and s toggles.

// tableView draws the rows that fit in h lines of width w, keeping the selection on screen.
//
// ponytail: placeholder until DEV-31; one indented name per row, no columns.
func (m *Model) tableView(w, h int) string {
	if m.selIdx >= 0 {
		m.top = max(min(m.top, m.selIdx), m.selIdx-h+1)
	}
	var b strings.Builder
	for i := m.top; i < min(len(m.rows), m.top+h); i++ {
		r := m.rows[i]
		line := strings.Repeat("  ", r.Depth) + rowName(r)
		if i == m.selIdx {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// rowName is a row's label: the project or group name for a header, the container name for a
// container row, else the process name.
func rowName(r model.Row) string {
	switch {
	case r.Project != nil:
		return r.Project.Name
	case r.Key.Header != 0:
		return r.Key.Group
	case r.Container != nil:
		return r.Container.Name
	case r.Process != nil:
		return r.Process.Name
	}
	return ""
}

// tableKey handles the keys the global switch in key does not.
//
// ponytail: placeholder until DEV-31; only up and down.
func (m *Model) tableKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		m.moveTo(m.selIdx - 1)
	case "down", "j":
		m.moveTo(m.selIdx + 1)
	}
	return nil
}

// moveTo selects the row at index i, clamped to the rows.
func (m *Model) moveTo(i int) {
	if len(m.rows) == 0 {
		return
	}
	m.selIdx = max(0, min(i, len(m.rows)-1))
	m.sel = m.rows[m.selIdx].Key
}
