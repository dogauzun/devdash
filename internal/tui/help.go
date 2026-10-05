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
	{"← → h l", "collapse or expand a group or node; → unfolds a chain, ← refolds"},
	{"enter", "open or close the detail pane (esc closes it too)"},
	{"/", "filter by port, name, argv, project, container or tag; esc clears"},
	{"0-9", "port search: opens the filter with the digit, selects the holder"},
	{"x", "kill modal: p process, t tree, f force, Y second confirm, esc cancel"},
	{"o", "open http://localhost:<port> (the lowest port)"},
	{"a", "show or hide shells and editors"},
	{"d", "show or hide container rows"},
	{"s", "cycle sort within groups: default, port, cpu, start time, name"},
	{"r", "refresh now"},
	{"S", "rerun under sudo (asks first) when a warning or a kill needs root"},
	{"?", "this help"},
	{"q ctrl-c", "quit"},
}

// helpKeyWidth is the key column of the help overlay.
const helpKeyWidth = 12

// helpPos is the help overlay's scroll position, as the last render laid it out.
// Its page is 0 when everything fit, so no key scrolls.
type helpPos struct {
	pager
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
	shown, pos := p.cut(lines, h-1, "lines %d-%d of %d, ↑↓ to scroll")
	out := []string{styleBold.Render("Keys") + styleDim.Render("  (↑↓ j k pgup pgdown scroll, any other key closes)")}
	out = append(out, shown...)
	if pos != "" {
		out = append(out, pos)
	}
	return strings.Join(out, "\n")
}

// helpKey handles keys while the help overlay is open: when it does not fit, up, down, j, k,
// pgup and pgdown scroll it; any other key closes it and does nothing else.
func (m *Model) helpKey(k tea.KeyPressMsg) tea.Cmd {
	if p := &m.hpos; p.page > 0 {
		if d := scrollStep(k.String(), p.page); d != 0 {
			p.move(d, p.total)
			return nil
		}
	}
	m.help, m.hpos = false, helpPos{}
	return nil
}
