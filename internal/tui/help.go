package tui

import tea "charm.land/bubbletea/v2"

// DEV-33 owns this file: the help overlay with every key of the spec's key table.

// helpView draws the help overlay in w by h.
//
// ponytail: placeholder until DEV-33.
func (m *Model) helpView(w, h int) string { return footerHints }

// helpKey handles keys while the help overlay is open: any key closes it.
func (m *Model) helpKey(tea.KeyPressMsg) tea.Cmd {
	m.help = false
	return nil
}
