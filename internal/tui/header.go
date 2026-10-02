package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// staleAfter is the number of consecutive missed ticks from which the header says "stale".
const staleAfter = 2

// headerSep separates the parts of the header line.
const headerSep = " · "

// headerView is one line of at most w cells: hostname, snapshot age, counts of projects,
// listeners and containers, and the active filter (spec "TUI design", Header). The filter gets
// its room first: when the line does not fit, the counts go whole (containers, then listeners,
// then projects), then the hostname is cut with "…" down to one letter, and then the query
// shows its tail, so the prompt's cursor stays on screen (DEV-84).
func (m *Model) headerView(w int) string {
	s := m.upd.Snapshot
	host := clean(s.Host.Hostname)
	if host == "" {
		host = "devdash"
	}
	age, ageStyle := "collecting", lipgloss.NewStyle()
	var counts []string
	if m.have {
		a := max(m.o.Now().Sub(s.TakenAt), 0)
		if m.upd.Missed >= staleAfter {
			age, ageStyle = "stale "+ago(a), styleWarn
		} else {
			age = ago(a) + " ago"
		}
		listeners := 0
		for _, p := range s.Processes {
			listeners += len(p.Listeners)
		}
		counts = []string{plural(len(s.Projects), "project"), plural(listeners, "listener"), plural(len(s.Containers), "container")}
	}
	var prefix, query, cursor string
	switch {
	case m.filtering:
		prefix, query, cursor = "/", clean(m.filter), "_"
	case m.filter != "":
		prefix, query = "filter: ", clean(m.filter)
	}
	filter := prefix + query + cursor

	sepW := ansi.StringWidth(headerSep)
	width := func() int {
		n := ansi.StringWidth(host) + sepW + ansi.StringWidth(age)
		for _, c := range counts {
			n += sepW + ansi.StringWidth(c)
		}
		if filter != "" {
			n += sepW + ansi.StringWidth(filter)
		}
		return n
	}
	for len(counts) > 0 && width() > w {
		counts = counts[:len(counts)-1]
	}
	if over := width() - w; over > 0 {
		hw := ansi.StringWidth(host)
		host = ansi.Truncate(host, max(hw-over, 2), "…")
	}
	if over := width() - w; over > 0 && filter != "" {
		room := ansi.StringWidth(query) - over - 1 // the "…"
		filter = prefix + "…" + tail(query, max(room, 0)) + cursor
	}

	parts := []string{styleBold.Render(host), ageStyle.Render(age)}
	parts = append(parts, counts...)
	if filter != "" {
		parts = append(parts, styleAccent.Render(filter))
	}
	return strings.Join(parts, headerSep)
}

// tail is the end of s that is at most n cells wide.
func tail(s string, n int) string {
	r := []rune(s)
	i, cells := len(r), 0
	for i > 0 {
		c := ansi.StringWidth(string(r[i-1]))
		if cells+c > n {
			break
		}
		cells += c
		i--
	}
	return string(r[i:])
}

// ago formats a snapshot age as "2 s", "3 m" or "1 h".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d m", int(d/time.Minute))
	}
	return fmt.Sprintf("%d h", int(d/time.Hour))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// footerKeys are the key hints, most useful first; footerTail always ends the line, so help
// and quit can be found at any width (the help overlay lists every key).
var footerKeys = []string{"↑↓ move", "←→ fold", "enter detail", "/ filter", "x kill", "o open", "a all", "d docker", "s sort", "r refresh"}

const footerTail = "? help  q quit"

// footerHints is the key hint line for width w: footerKeys in order as long as they fit
// before footerTail, never a hint cut in the middle.
func footerHints(w int) string {
	s := ""
	for _, k := range footerKeys {
		if ansi.StringWidth(s+k+"  "+footerTail) > w {
			break
		}
		s += k + "  "
	}
	return s + footerTail
}

// footerWarnLines is how many lines the warnings may take before the rest is cut.
const footerWarnLines = 2

// footerView is the one-shot status message, then the warnings (collector, Build and engine
// hints, deduplicated by hint text) wrapped to at most two lines when there are any, then the
// key hints.
// Status and warnings embed snapshot text (names, paths, errors), so both are cleaned.
func (m *Model) footerView(w int) string {
	var lines []string
	if m.status != "" {
		lines = append(lines, styleWarn.Render(clean(m.status)))
	}
	if ws := m.warnings(); len(ws) > 0 {
		wl := detailWrap(strings.Fields(clean(strings.Join(ws, " · "))), " ", w)
		if len(wl) > footerWarnLines {
			last := footerWarnLines - 1
			wl = append(wl[:last], ansi.Truncate(strings.Join(wl[last:], " "), w-1, "")+"…")
		}
		for _, l := range wl {
			lines = append(lines, styleWarn.Render(l))
		}
	}
	return strings.Join(append(lines, styleDim.Render(footerHints(w))), "\n")
}

// warnings returns the distinct hints of the snapshot's and the engine's warnings, and the
// error of a failed tick, in order.
func (m *Model) warnings() []string {
	var hints []string
	add := func(ws []model.Warning) {
		for _, w := range ws {
			h := w.Hint
			if h == "" {
				h = w.Code
			}
			if !slices.Contains(hints, h) {
				hints = append(hints, h)
			}
		}
	}
	add(m.upd.Snapshot.Warnings)
	add(m.upd.Warnings)
	if m.upd.Err != nil {
		hints = append(hints, "refresh failed: "+m.upd.Err.Error())
	}
	return hints
}
