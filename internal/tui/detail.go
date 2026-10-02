package tui

// DEV-33 owns this file: the detail pane (full argv wrapped, cwd, project and branch,
// listeners with bind address, parent chain, start time, user, Docker socket for containers).

// detailWidth is the width of the right split at w columns (w >= splitWidth).
func detailWidth(w int) int { return w * 2 / 5 }

// detailView draws the selected row's details in w by h.
//
// ponytail: placeholder until DEV-33.
func (m *Model) detailView(w, h int) string {
	r, ok := m.selected()
	if !ok {
		return "nothing selected"
	}
	return "detail: " + rowName(r)
}
