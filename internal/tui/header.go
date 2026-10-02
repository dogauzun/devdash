package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

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

// footerCut ends the last warning line when the warnings do not fit: the help overlay lists
// them all.
const footerCut = "… (? for all)"

// footerView is the one-shot status message, then the warnings' hints (m.warnings) joined by
// " · " and wrapped to at most two lines when there are any, the second ending in footerCut
// after the last whole word that fits when more is left, then the key hints.
// Status and warnings embed snapshot text (names, paths, errors), so both are cleaned.
func (m *Model) footerView(w int) string {
	var lines []string
	if m.status != "" {
		lines = append(lines, styleWarn.Render(clean(m.status)))
	}
	if ws := m.warnings(); len(ws) > 0 {
		hints := make([]string, len(ws))
		for i, x := range ws {
			hints[i] = x.Hint
		}
		wl := detailWrap(strings.Fields(clean(strings.Join(hints, " · "))), " ", w)
		if len(wl) > footerWarnLines {
			last := footerWarnLines - 1
			rest := strings.Fields(strings.Join(wl[last:], " "))
			cut := detailWrap(rest, " ", w-ansi.StringWidth(footerCut))[0]
			cut = strings.TrimRight(cut, " ·") // never end on a separator
			wl = append(wl[:last], ansi.Truncate(cut+footerCut, w, ""))
		}
		for _, l := range wl {
			lines = append(lines, styleWarn.Render(l))
		}
	}
	return strings.Join(append(lines, styleDim.Render(footerHints(w))), "\n")
}

// warnings returns the snapshot's and then the engine's warnings, one per distinct hint (the
// code stands in for an empty hint) in order of first appearance, with the counts of a
// repeated hint summed, as Build sums a repeated code; then the error of a failed tick as
// refresh_failed. Hints are raw: callers clean them.
func (m *Model) warnings() []model.Warning {
	var out []model.Warning
	at := map[string]int{}
	for _, ws := range [][]model.Warning{m.upd.Snapshot.Warnings, m.upd.Warnings} {
		for _, w := range ws {
			if w.Hint == "" {
				w.Hint = w.Code
			}
			if i, ok := at[w.Hint]; ok {
				out[i].Count += w.Count
				continue
			}
			at[w.Hint] = len(out)
			out = append(out, w)
		}
	}
	if m.upd.Err != nil {
		out = append(out, model.Warning{Code: "refresh_failed", Count: 1, Hint: "refresh failed: " + m.upd.Err.Error()})
	}
	return out
}
