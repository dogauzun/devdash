package tui

import "fmt"

// pager is a scroll position over lines that do not fit, shared by the help overlay, the kill
// modal and the detail pane. Each pane decides itself when its lines fit, and what page and top
// are then, and records its total there; cut is the paged case.
type pager struct {
	top   int // first line shown
	page  int // lines the last render showed: the pgup/pgdown step
	total int // lines the last render laid out: move clamps on it
}

// cut lays out lines that do not fit in room: the lines from p.top (clamped here, and the page
// and total kept for the keys) and a position line from format (first, last, total), or, with one line of
// room, a line instead of the position. The lines returned have no spare capacity, so appending
// the position to them does not write into lines.
func (p *pager) cut(lines []string, room int, format string) (shown []string, pos string) {
	n := max(room-1, 0)
	if room == 1 {
		n = 1 // a line beats a position line
	}
	p.page, p.total = max(n, 1), len(lines)
	p.top = max(min(p.top, len(lines)-n), 0)
	if n < room {
		pos = fmt.Sprintf(format, p.top+1, p.top+n, len(lines))
	}
	return lines[p.top : p.top+n : p.top+n], pos
}

// move scrolls by d lines within the lines the last render laid out, as it paged them.
func (p *pager) move(d int) { p.top = max(min(p.top+d, p.total-max(p.page, 1)), 0) }

// scrollStep is how far key scrolls with page lines shown: a line for up, k, down and j, a page
// (at least one line) for pgup and pgdown, and 0 for any other key. Each pane picks which of
// these keys it takes.
func scrollStep(key string, page int) int {
	switch key {
	case "up", "k":
		return -1
	case "down", "j":
		return 1
	case "pgup":
		return -max(page, 1)
	case "pgdown":
		return max(page, 1)
	}
	return 0
}
