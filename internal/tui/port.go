package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// DEV-134 owns this file: digits start a port search, a query that becomes a port number
// selects the port's exact holder, and the port line under the header answers it (spec
// "Release 1.1", Port search and The port line). The free-port probe runs as a tea.Cmd, off
// the UI goroutine, and its answer comes back as an action.

// portState is the port line's state: the query's port, its holders and the probe's answer for
// n, kept until the next snapshot's arrives.
type portState struct {
	n       uint16 // the query's port number; 0 when the query is not one
	holders int    // len(model.Holders) of n in the latest snapshot
	probeRun
}

// probeRun is the latest run of a free-port search (the port line's, the detail pane's next
// free) and its answer. The search runs off the UI goroutine, so answers can arrive late or out
// of order: only the latest run's is kept.
type probeRun struct {
	seq  uint64     // tags the latest run
	ans  portAnswer // the latest run's answer
	have bool       // ans is set
}

// start tags a new run as the latest and returns its tag; an earlier run's answer is dropped
// from now on.
func (r *probeRun) start() uint64 {
	r.seq++
	return r.seq
}

// take keeps ans when seq tags the latest run.
func (r *probeRun) take(seq uint64, ans portAnswer) {
	if seq == r.seq {
		r.ans, r.have = ans, true
	}
}

// portAnswer is what one probe run found for port n.
type portAnswer struct {
	self    bool   // no row held n, so n itself was probed first
	free    bool   // self: n binds
	refused bool   // self: the bind of n failed (another user's listener, or a port below 1024)
	next    uint16 // the first free port from n+1, when found is set
	found   bool   // n+1..freeport.Last(n+1) has a free port
	err     error  // the probe failed; the search stopped there
}

// portProbedMsg is a probe run's answer, tagged with its run.
type portProbedMsg struct {
	seq uint64
	ans portAnswer
}

// apply keeps the answer when it is the latest run's, for the current port.
func (msg portProbedMsg) apply(m *Model) {
	if m.port.n != 0 {
		m.port.take(msg.seq, msg.ans)
	}
}

// portQuery is the port number q stands for: digits only, no leading zero, 1 to 65535; 0 when q
// is not one.
func portQuery(q string) uint16 {
	if q == "" || q[0] == '0' || strings.Trim(q, "0123456789") != "" {
		return 0
	}
	n, err := strconv.ParseUint(q, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// portKey starts a port search from the table: exactly what / and then the digit d do.
func (m *Model) portKey(d string) tea.Cmd {
	m.filtering = true
	return m.setFilter(m.filter + d)
}

// portSearch follows a change of the query (setFilter, after the rows are rebuilt): when it is a
// port number that a shown row holds, the first such row in display order is selected, and the
// probe runs for it. A new port drops the answer for the old one.
func (m *Model) portSearch() tea.Cmd {
	n := portQuery(m.filter)
	if n != m.port.n {
		m.port = portState{n: n, seq: m.port.seq + 1} // drops the old port's run
	}
	if n == 0 {
		return nil
	}
	if i := slices.IndexFunc(m.rows, func(r model.Row) bool { return holds(r, n) }); i >= 0 {
		m.moveTo(i)
	}
	return m.portProbe()
}

// portProbe counts the holders of the query's port in the latest snapshot, as model.Holders
// does for `port N` and `kill N`, and returns the
// command that probes for it, tagged as the latest run; nil when the query is not a port
// number or there is no snapshot yet. Update calls it on each new snapshot.
func (m *Model) portProbe() tea.Cmd {
	p := &m.port
	if p.n == 0 || !m.have {
		return nil
	}
	s, n, probe := m.upd.Snapshot, p.n, m.o.Probe
	p.holders = len(model.Holders(s, n))
	search, seq := nextFree, p.start()
	if p.holders == 0 {
		search = probeSelf
	}
	return func() tea.Msg { return portProbedMsg{seq: seq, ans: search(s, n, probe)} }
}

// probeSelf is the port line's search when no row holds n: n itself is probed first and a free
// n ends it; then nextFree.
func probeSelf(s model.Snapshot, n uint16, probe freeport.Prober) portAnswer {
	if free, err := probe(n); free || err != nil {
		return portAnswer{self: true, free: free, err: err}
	}
	a := nextFree(s, n, probe)
	a.self, a.refused = true, true
	return a
}

// nextFree searches, below 65535, from n+1 with freeport.Find, as `devdash free n+1` does.
func nextFree(s model.Snapshot, n uint16, probe freeport.Prober) (a portAnswer) {
	if n < 65535 {
		a.next, a.found, a.err = freeport.Find(s, n+1, probe)
	}
	return a
}

// holds reports whether r holds TCP port n: a listener on it of any proto but UDP, or, for a
// row drawn as a container, with or without a process behind it, a port the container
// publishes over tcp. A row holds n exactly when it stands for one of model.Holders, so the
// search selects a row whenever the port line counts a holder. A listener reconciled to a
// container holds n only on a process row drawn as that container: a forwarder that keeps its
// own row (several containers' ports, or one next to a port of its own) leaves n to the
// container's row, as model.Holders does (DEV-154), even when that row is a process drawn as
// the container that does not hold n itself (DEV-166).
func holds(r model.Row, n uint16) bool {
	if r.Container != nil && slices.ContainsFunc(r.Container.Ports, func(pm model.PortMapping) bool {
		return pm.HostPort == n && pm.TCP()
	}) {
		return true
	}
	return r.Process != nil && slices.ContainsFunc(r.Process.Listeners, func(l model.Listener) bool {
		return l.Port == n && l.TCP() && l.ContainerID == r.Process.ContainerID
	})
}

// portShown reports whether the port line is shown: the query, typed or applied, is a port
// number and there is a snapshot to answer it from.
func (m *Model) portShown() bool { return m.port.n != 0 && m.have }

// portLine is the line under the header while the query is a port number N, cut with "…" to w
// (spec "Release 1.1", The port line): `port N`, the holders when there are any, then the
// probe's answer once it has come for the current query: `next free P`, `no free port in A-B`
// or `next free: <error>` in the warning colour, none for 65535; with no holder, `free` when N
// binds, else, when its bind failed, that part and then `bind refused` (a probe error on N
// itself shows only `probe failed: <error>`, for 65535 too). An answer that was for holders while there
// are none now, or the other way round, waits for the next one. "" when the line is not shown.
func (m *Model) portLine(w int) string {
	if !m.portShown() {
		return ""
	}
	p := m.port
	parts := []string{"port " + strconv.Itoa(int(p.n))}
	if p.holders > 0 {
		parts = append(parts, model.Count(p.holders, "holder", "holders"))
	}
	if a := p.ans; p.have && a.self == (p.holders == 0) {
		switch {
		case a.self && a.free:
			parts = append(parts, "free")
		case a.err != nil && a.self && !a.refused: // the probe of n itself failed
			parts = append(parts, styleWarn.Render("probe failed: "+model.Clean(a.err.Error())))
		case a.err != nil:
			parts = append(parts, styleWarn.Render("next free: "+model.Clean(a.err.Error())))
		case p.n == 65535:
		case a.found:
			parts = append(parts, "next free "+strconv.Itoa(int(a.next)))
		default:
			parts = append(parts, "no free port in "+strconv.Itoa(int(p.n)+1)+"-"+strconv.Itoa(int(freeport.Last(p.n+1))))
		}
		if a.refused {
			parts = append(parts, "bind refused")
		}
	}
	return ansi.Truncate(strings.Join(parts, headerSep), w, "…")
}
