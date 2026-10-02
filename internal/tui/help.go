package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// DEV-33 owns this file: the help overlay with every key of the spec's key table.

// helpKeys is the spec's key table ("TUI design"), one line per row; every line fits 80
// columns.
var helpKeys = [][2]string{
	{"↑ ↓ j k", "move the selection"},
	{"← → h l", "collapse or expand a project group or a tree node"},
	{"enter", "open or close the detail pane (esc closes it too)"},
	{"/", "filter by port, name, argv, project or container; esc clears"},
	{"x", "kill modal: p process, t tree, f force, esc cancel"},
	{"o", "open http://localhost:<port> (the lowest port)"},
	{"a", "show or hide shells and editors"},
	{"d", "show or hide container rows"},
	{"s", "cycle sort within groups: default, port, cpu, start time"},
	{"r", "refresh now"},
	{"?", "this help"},
	{"q ctrl-c", "quit"},
}

// helpKeyWidth is the key column of the help overlay.
const helpKeyWidth = 11

// helpView draws the help overlay in w by h: a title, then every key with its action. It
// fits 80 by 24; the caller cuts it to a smaller screen.
func (m *Model) helpView(w, h int) string {
	lines := []string{styleBold.Render("Keys") + styleDim.Render("  (any key closes)"), ""}
	for _, k := range helpKeys {
		lines = append(lines, k[0]+strings.Repeat(" ", max(helpKeyWidth-ansi.StringWidth(k[0]), 1))+k[1])
	}
	return strings.Join(lines, "\n")
}

// helpKey handles keys while the help overlay is open: any key closes it and does nothing else.
func (m *Model) helpKey(tea.KeyPressMsg) tea.Cmd {
	m.help = false
	return nil
}
