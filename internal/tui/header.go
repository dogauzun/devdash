package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// staleAfter is the number of consecutive missed ticks from which the header says "stale".
const staleAfter = 2

// headerView is one line: hostname, snapshot age, counts of projects, listeners and
// containers, and the active filter (spec "TUI design", Header).
func (m *Model) headerView(w int) string {
	s := m.upd.Snapshot
	host := clean(s.Host.Hostname)
	if host == "" {
		host = "devdash"
	}
	parts := []string{styleBold.Render(host)}
	if !m.have {
		parts = append(parts, "collecting")
	} else {
		age := max(m.o.Now().Sub(s.TakenAt), 0)
		if m.upd.Missed >= staleAfter {
			parts = append(parts, styleWarn.Render("stale "+ago(age)))
		} else {
			parts = append(parts, ago(age)+" ago")
		}
		listeners := 0
		for _, p := range s.Processes {
			listeners += len(p.Listeners)
		}
		parts = append(parts,
			plural(len(s.Projects), "project"), plural(listeners, "listener"), plural(len(s.Containers), "container"))
	}
	switch {
	case m.filtering:
		parts = append(parts, styleAccent.Render("/"+clean(m.filter)+"_"))
	case m.filter != "":
		parts = append(parts, styleAccent.Render("filter: "+clean(m.filter)))
	}
	return strings.Join(parts, " · ")
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

// footerHints are the key hints, most useful first; the footer cuts them at the width.
const footerHints = "↑↓ move  ←→ fold  enter detail  / filter  x kill  o open  a all  d docker  s sort  r refresh  ? help  q quit"

// footerView is the one-shot status message or the warnings (collector, Build and engine
// hints, deduplicated by hint text) on their own line when there are any, then the key hints.
// Status and warnings embed snapshot text (names, paths, errors), so both are cleaned.
func (m *Model) footerView(w int) string {
	var lines []string
	if m.status != "" {
		lines = append(lines, styleWarn.Render(clean(m.status)))
	}
	if ws := m.warnings(); len(ws) > 0 {
		lines = append(lines, styleWarn.Render(clean(strings.Join(ws, " · "))))
	}
	return strings.Join(append(lines, styleDim.Render(footerHints)), "\n")
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
