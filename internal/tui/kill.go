package tui

import tea "charm.land/bubbletea/v2"

// DEV-34 owns this file: the kill modal over engine plans (p process, t tree, f force, esc
// cancel), the second confirmation outside every project, refusals, and survivors with the
// offer to force-kill them. Kill runs as a tea.Cmd, off the UI goroutine, and its result
// comes back as an action.

// killState is the kill modal's state; the zero value is closed.
type killState struct{}

// active reports whether the kill modal has the keyboard.
func (k *killState) active() bool { return false }

// startKill opens the kill modal for the selected row (x).
//
// ponytail: placeholder until DEV-34.
func (m *Model) startKill() tea.Cmd { return nil }

// killKey handles keys while the modal is open.
func (m *Model) killKey(tea.KeyPressMsg) tea.Cmd { return nil }

// killView draws the modal in w by h.
func (m *Model) killView(w, h int) string { return "" }
