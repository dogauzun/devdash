package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-33 owns this file: the help overlay with every key of the spec's key table.

// helpKeys is the spec's key table ("TUI design"), one line per row, plus the keys the
// dashboard takes beyond it (pgup/pgdown, g/G, ctrl+u in the filter prompt, Y in the kill
// modal); every line fits 80 columns.
var helpKeys = [][2]string{
	{"↑ ↓ j k", "move the selection"},
	{"pgup pgdown", "page the table or the open detail pane; g/home first, G/end last row"},
	{"← → h l", "collapse or expand a project group or a tree node"},
	{"enter", "open or close the detail pane (esc closes it too)"},
	{"/", "filter by port, name, argv, project, container or tag; esc clears"},
	{"0-9", "port search: opens the filter with the digit, selects the holder"},
	{"x", "kill modal: p process, t tree, f force, Y second confirm, esc cancel"},
	{"o", "open http://localhost:<port> (the lowest port)"},
	{"a", "show or hide shells and editors"},
	{"d", "show or hide container rows"},
	{"s", "cycle sort within groups: default, port, cpu, start time"},
	{"r", "refresh now"},
	{"?", "this help"},
	{"q ctrl-c", "quit"},
}

// helpKeyWidth is the key column of the help overlay.
const helpKeyWidth = 12

// helpPos is the help overlay's scroll position, as the last render laid it out.
type helpPos struct {
	top   int // first overlay line shown
	page  int // overlay lines shown; 0 when everything fit, so no key scrolls
	total int // overlay lines
}

// helpLines is the overlay below its title: every key with its action, the action wrapped to
// w with continuation lines indented to the action column (as the detail pane's values), then,
// when there are any, every current warning in full, one per line, wrapped to w with
// continuation lines indented, with its count when above 1.
func (m *Model) helpLines(w int) []string {
	lines := make([]string, 0, len(helpKeys)+8)
	pad := strings.Repeat(" ", helpKeyWidth)
	for _, k := range helpKeys {
		for i, l := range detailWrap(strings.Fields(k[1]), " ", w-helpKeyWidth) {
			prefix := pad
			if i == 0 {
				prefix = k[0] + strings.Repeat(" ", max(helpKeyWidth-ansi.StringWidth(k[0]), 1))
			}
			lines = append(lines, prefix+l)
		}
	}
	ws := m.warnings()
	if len(ws) == 0 {
		return lines
	}
	lines = append(lines, "", styleBold.Render("Warnings"))
	for _, wn := range ws {
		words := strings.Fields(model.Clean(wn.Hint))
		if wn.Count > 1 {
			words = append(words, fmt.Sprintf("(count %d)", wn.Count))
		}
		for i, l := range detailWrap(words, " ", w-2) {
			if i > 0 {
				l = "  " + l
			}
			lines = append(lines, styleWarn.Render(l))
		}
	}
	return lines
}

// helpView draws the help overlay in w by h: a title, then helpLines. When they do not fit,
// the title names the scroll keys and the lines scroll under it from m.hpos.top (clamped
// here, and the page kept for the keys), above a position line like the kill modal's; with
// one line left that line shows an overlay line instead.
func (m *Model) helpView(w, h int) string {
	lines := m.helpLines(w)
	p := &m.hpos
	p.total = len(lines)
	if 2+len(lines) <= h {
		p.top, p.page = 0, 0
		title := styleBold.Render("Keys") + styleDim.Render("  (any key closes)")
		return strings.Join(append([]string{title, ""}, lines...), "\n")
	}
	room := h - 1
	n := max(room-1, 0)
	if room == 1 {
		n = 1 // an overlay line beats a position line
	}
	p.page = max(n, 1)
	p.top = max(min(p.top, len(lines)-n), 0)
	out := []string{styleBold.Render("Keys") + styleDim.Render("  (↑↓ j k pgup pgdown scroll, any other key closes)")}
	out = append(out, lines[p.top:p.top+n]...)
	if n < room {
		out = append(out, fmt.Sprintf("lines %d-%d of %d, ↑↓ to scroll", p.top+1, p.top+n, len(lines)))
	}
	return strings.Join(out, "\n")
}

// helpKey handles keys while the help overlay is open: when it does not fit, up, down, j, k,
// pgup and pgdown scroll it; any other key closes it and does nothing else.
func (m *Model) helpKey(k tea.KeyPressMsg) tea.Cmd {
	if p := &m.hpos; p.page > 0 {
		d := 0
		switch k.String() {
		case "up", "k":
			d = -1
		case "down", "j":
			d = 1
		case "pgup":
			d = -p.page
		case "pgdown":
			d = p.page
		}
		if d != 0 {
			p.top = max(min(p.top+d, p.total-p.page), 0)
			return nil
		}
	}
	m.help, m.hpos = false, helpPos{}
	return nil
}
