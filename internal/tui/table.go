package tui

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-31 owns this file: columns by priority and width, project headers, container rows,
// dimmed rows, scrolling (m.top), movement, collapse and expand, and the a, d and s toggles.

// Column breakpoints (spec "TUI design"); below minWidth only name, ports and pid are kept.
const (
	wideWidth    = 100 // cpu, mem and user from here
	commandWidth = 90  // command from here
	nameMin      = 20  // the name column is never narrower while the command is shown
	commandMin   = 16  // the name column grows up to its longest cell, but leaves this to the command
)

// col is one table column.
type col uint8

// Columns in priority order.
const (
	colName col = iota
	colKind
	colPorts
	colPID
	colUp
	colCPU
	colMem
	colUser
	colCommand
)

// colSpecs holds each column's title, fixed width (0: computed from the table's width) and
// alignment.
var colSpecs = [...]struct {
	title string
	width int
	right bool
}{
	colName:    {"NAME", 0, false},
	colKind:    {"KIND", 9, false}, // "container"
	colPorts:   {"PORTS", 11, false},
	colPID:     {"PID", 7, true}, // Linux pids go up to 4194304
	colUp:      {"UP", 4, true},  // "400d"
	colCPU:     {"CPU", 5, true}, // "812.5"
	colMem:     {"MEM", 5, true}, // "1023M"
	colUser:    {"USER", 8, false},
	colCommand: {"COMMAND", 0, false},
}

// columnsFor returns the columns shown at width w.
func columnsFor(w int) []col {
	switch {
	case w >= wideWidth:
		return []col{colName, colKind, colPorts, colPID, colUp, colCPU, colMem, colUser, colCommand}
	case w >= commandWidth:
		return []col{colName, colKind, colPorts, colPID, colUp, colCommand}
	case w >= minWidth:
		return []col{colName, colKind, colPorts, colPID, colUp}
	}
	return []col{colName, colPorts, colPID}
}

// layout returns the columns at width w and their widths, to be joined by one space. Name gets
// what the fixed columns leave; when the command is shown, name takes its longest cell
// (longest), but at least nameMin and leaving commandMin, and the command gets the rest.
func layout(w, longest int) ([]col, []int) {
	cols := columnsFor(w)
	widths := make([]int, len(cols))
	rest := w - (len(cols) - 1)
	for i, c := range cols {
		widths[i] = colSpecs[c].width
		rest -= widths[i]
	}
	widths[0] = max(rest, 0)
	if cols[len(cols)-1] == colCommand {
		widths[0] = max(nameMin, min(longest, rest-commandMin))
		widths[len(cols)-1] = max(rest-widths[0], 0)
	}
	return cols, widths
}

// tableView draws the column titles and the rows that fit in h lines of width w, keeping the
// selection on screen.
func (m *Model) tableView(w, h int) string {
	if h <= 0 || w <= 0 || !m.have {
		return ""
	}
	titled := h > 1 // with one line, the selected row matters more than the titles
	rh := h
	if titled {
		rh--
	}
	m.scroll(rh)

	name := m.nameTitle()
	c := m.cache()
	cols, widths := layout(w, max(ansi.StringWidth(name), c.longest))
	var lines []string
	if titled {
		cells := make([]string, len(cols))
		for j, c := range cols {
			t := colSpecs[c].title
			if c == colName {
				t = name
			}
			cells[j] = pad(t, widths[j], colSpecs[c].right)
		}
		lines = append(lines, styleBold.Render(pad(strings.Join(cells, " "), w, false)))
	}
	if len(m.rows) == 0 {
		return strings.Join(append(lines, "  nothing to show"), "\n")
	}

	for i := m.top; i < min(len(m.rows), m.top+rh); i++ {
		r := m.rows[i]
		var l string
		faint := [2]int{} // the cells the tags and arguments take, cut to the name column
		if r.Key.Header != model.GroupNone {
			l = pad(m.nameCell(i)+c.counts[r.Key].text(r.Key.Header), w, false)
		} else {
			cells := make([]string, len(cols))
			for j, c := range cols {
				cells[j] = pad(m.cell(i, c), widths[j], colSpecs[c].right)
			}
			name := m.nameCell(i)
			end := ansi.StringWidth(name)
			args := ""
			if c.short { // below commandWidth columns of the terminal (shortTags): no command column
				if a, room := argText(r), widths[0]-end-2; a != "" && room >= argsMin {
					args = "  " + ansi.Truncate(a, room, "…")
					cells[0] = pad(name+args, widths[0], false)
				}
			}
			l = pad(strings.Join(cells, " "), w, false)
			if t := ansi.StringWidth(tagText(r, c.short)); t > 0 || args != "" {
				faint = [2]int{min(end-t, widths[0]), min(end+ansi.StringWidth(args), widths[0])}
			}
		}
		st := lipgloss.NewStyle()
		switch {
		case i == m.selIdx:
			st = styleSel
		case r.Key.Header != model.GroupNone:
			st = styleBold
		case r.Dimmed:
			st = styleDim
		}
		if faint[0] < faint[1] { // in three pieces, since a style rendered inside another ends it
			l = st.Render(ansi.Cut(l, 0, faint[0])) + st.Faint(true).Render(ansi.Cut(l, faint[0], faint[1])) +
				st.Render(ansi.Cut(l, faint[1], w))
		} else {
			l = st.Render(l)
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// scroll moves m.top so that the selected row is among the rh rows on screen and the window
// does not end past the last row.
func (m *Model) scroll(rh int) {
	m.top = max(min(m.top, len(m.rows)-rh), 0)
	if m.selIdx >= 0 {
		m.top = max(min(m.top, m.selIdx), m.selIdx-rh+1, 0)
	}
}

// nameTitle is the name column's title, followed by the active view toggles.
func (m *Model) nameTitle() string {
	t := []string{"NAME"}
	if m.view.Sort != model.SortDefault {
		t = append(t, "sort: "+sortNames[m.view.Sort])
	}
	if m.view.ShowAll {
		t = append(t, "all")
	}
	if m.view.HideContainers {
		t = append(t, "no containers")
	}
	return strings.Join(t, " · ")
}

// sortNames are the sort modes as the title line names them, in the order s cycles them.
var sortNames = [...]string{
	model.SortDefault: "default", model.SortPort: "port", model.SortCPU: "cpu", model.SortStart: "start",
}

// pad cuts s to width cells, with an ellipsis, and pads it with spaces to exactly width.
func pad(s string, width int, right bool) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "…")
	fill := strings.Repeat(" ", width-ansi.StringWidth(s))
	if right {
		return fill + s
	}
	return s + fill
}

// Tree markers on headers and on rows with children.
const (
	markOpen   = "▾ "
	markClosed = "▸ "
	markNone   = "  "
)

// nameCell is row i's name: indented by depth, a marker when it has children, its label,
// cleaned, then `(here)` on the Here project's header or a process's tags (tagText), so the
// cached widest cell measures what is drawn.
// A row has children when the next row is deeper, or when it is collapsed and had children
// in the expanded rows (its children may have exited since it was collapsed). While a filter
// is set nothing is folded (rebuild), so a collapsed row is drawn by the rows shown.
func (m *Model) nameCell(i int) string {
	c := m.cache()
	return m.nameCellWith(i, c.kids, c.short)
}

// nameCellWith is nameCell with the rows that have children when expanded given, and whether
// tags are shortened.
func (m *Model) nameCellWith(i int, kids map[model.RowKey]bool, short bool) string {
	r := m.rows[i]
	mark := markNone
	switch {
	case m.filter == "" && m.view.Collapsed[r.Key] && kids[r.Key]:
		mark = markClosed
	case i+1 < len(m.rows) && m.rows[i+1].Depth > r.Depth:
		mark = markOpen
	}
	label := model.Clean(rowLabel(r))
	if r.Key.Header == model.GroupProject && r.Project != nil && r.Project.Here {
		label += hereSuffix
	}
	return strings.Repeat("  ", r.Depth) + mark + label + tagText(r, short)
}

// hereSuffix ends the header of the project devdash was run from (spec "Release 1.0", TUI). It
// is not part of rowLabel (model.Project.Label): the port answer shows that label without it and
// says "this repo".
const hereSuffix = " (here)"

// tagText is what follows a tagged process's name (spec "Release 1.0", TUI): two spaces and
// its tags as people read them ("  orphaned, cwd deleted"), or "  !" when short; "" for a row
// without tags. tableView draws it faint.
func tagText(r model.Row, short bool) string {
	switch {
	case r.Process == nil || r.Process.Tags == 0:
		return ""
	case short:
		return "  !"
	}
	return "  " + strings.Join(r.Process.Tags.Labels(), ", ")
}

// shortTags reports whether tags are shown as "!": below commandWidth columns of the terminal,
// whatever the table's own width (the detail split narrows it from 120 columns).
func (m *Model) shortTags() bool {
	w, _ := m.size()
	return w < commandWidth
}

// rowLabel is what the name column says about r: the group for a header (a project's is the
// model's Project.Label, which `port N` shows too, so the two read the same), the container name
// and image for a container row or a process holding a container's port, else the process name.
// It is snapshot text, not yet cleaned.
func rowLabel(r model.Row) string {
	switch r.Key.Header {
	case model.GroupProject:
		p := r.Project
		if p == nil {
			return r.Key.Group
		}
		return p.Label()
	case model.GroupCompose:
		return r.Key.Group + " (compose)"
	case model.GroupContainers:
		return "containers"
	case model.GroupOther:
		return "other"
	}
	switch {
	case r.Container != nil:
		return r.Container.Name + " (" + r.Container.Image + ")"
	case r.Process != nil && r.Process.PID == 0:
		return "unknown"
	case r.Process != nil:
		return procLabel(r.Process)
	}
	return ""
}

// procLabel is how the dashboard names process p (spec "Release 1.1", tool labels): `<tool>
// (<name>)` when it is an interpreter running a tool (model.Tool: `vite (node)`, `server.js
// (node)`), else its name. The table, the detail pane's title and the kill modal use it; JSON,
// `port N` and `kill N` keep the name. It is snapshot text, not yet cleaned.
func procLabel(p *model.Process) string {
	if tool, _, ok := model.Tool(*p); ok {
		return tool + " (" + p.Name + ")"
	}
	return p.Name
}

// argText is what follows a process row's label and tags below commandWidth columns, where the
// command column is not shown (spec "Release 1.1", tool labels): its arguments after the tool
// for an interpreter, else after argv[0], joined by single spaces and cleaned; "" for a header,
// a container's row (whose label is the container's), the unknown owner and a process without
// argv. tableView draws it faint, two spaces after the tags.
func argText(r model.Row) string {
	p := r.Process
	if r.Key.Header != model.GroupNone || r.Container != nil || p == nil || p.PID == 0 || len(p.Argv) == 0 {
		return ""
	}
	args := p.Argv[1:]
	if _, a, ok := model.Tool(*p); ok {
		args = a
	}
	return model.Clean(strings.Join(args, " "))
}

// argsMin is the fewest cells the arguments are drawn in; with fewer left in the name column
// they are left out, since a few letters and `…` say nothing.
const argsMin = 6

// cell is the text of column c for the process or container row i, unpadded and cleaned.
func (m *Model) cell(i int, c col) string {
	r := m.rows[i]
	p := r.Process
	switch c {
	case colName:
		return m.nameCell(i)
	case colPorts:
		return portsText(ports(r))
	case colKind:
		if p == nil {
			return model.KindContainer.String()
		}
		return p.Kind.String()
	}
	if p == nil { // a container with no process behind it has nothing more to show
		return ""
	}
	unknown := p.PID == 0 // the "unknown owner" pseudo-process
	switch c {
	case colPID:
		if unknown {
			return "-"
		}
		return strconv.Itoa(p.PID)
	case colUp:
		if unknown || p.StartTime.IsZero() {
			return "-"
		}
		return model.Uptime(m.o.Now().Sub(p.StartTime))
	case colCPU:
		return cpu(p)
	case colMem:
		if p.Unknown&model.FieldMem != 0 {
			return "–"
		}
		return mem(p.RSSBytes)
	case colUser:
		return cmp.Or(model.Clean(p.User), "-")
	case colCommand:
		switch {
		case unknown:
			return ""
		case len(p.Argv) == 0:
			return model.Clean(p.Name)
		}
		return model.Clean(strings.Join(p.Argv, " "))
	}
	return ""
}

// cpu formats a CPU percent with one decimal below 100 and none from there, or an en dash when it is not known (first sample).
func cpu(p *model.Process) string {
	if p.Unknown&model.FieldCPU != 0 || math.IsNaN(p.CPUPercent) {
		return "–"
	}
	if p.CPUPercent >= 99.95 { // from 100 the decimal goes, so up to 99999 fits the column
		return strconv.FormatFloat(p.CPUPercent, 'f', 0, 64)
	}
	return strconv.FormatFloat(p.CPUPercent, 'f', 1, 64)
}

// mem formats a byte count in binary units: 512B, 12K, 179M, 1.2G, 15G.
func mem(b uint64) string {
	const k, m, g = 1 << 10, 1 << 20, 1 << 30
	f := float64(b)
	switch {
	case b >= 10*g:
		return fmt.Sprintf("%.0fG", f/g)
	case b >= g:
		return fmt.Sprintf("%.1fG", f/g)
	case b >= m:
		return fmt.Sprintf("%.0fM", f/m)
	case b >= k:
		return fmt.Sprintf("%.0fK", f/k)
	}
	return fmt.Sprintf("%dB", b)
}

// port is one distinct port of a row; any is set when one of its sockets or mappings is bound
// to every interface.
type port struct {
	n   uint16
	any bool
}

// ports returns r's distinct listener ports, or a container row's published host ports,
// ascending.
func ports(r model.Row) []port {
	var ps []port
	add := func(n uint16, any bool) {
		if i := slices.IndexFunc(ps, func(p port) bool { return p.n == n }); i >= 0 {
			ps[i].any = ps[i].any || any
			return
		}
		ps = append(ps, port{n, any})
	}
	switch {
	case r.Process != nil:
		for _, l := range r.Process.Listeners {
			add(l.Port, l.Addr.IsUnspecified())
		}
	case r.Container != nil:
		for _, pm := range r.Container.Ports {
			if pm.HostPort != 0 {
				add(pm.HostPort, !pm.HostIP.IsValid() || pm.HostIP.IsUnspecified())
			}
		}
	}
	slices.SortFunc(ps, func(a, b port) int { return cmp.Compare(a.n, b.n) })
	return ps
}

// portsText joins ports with commas, with a leading * on those bound to every interface.
func portsText(ps []port) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(int(p.n))
		if p.any {
			s[i] = "*" + s[i]
		}
	}
	return strings.Join(s, ",")
}

// groupCount is what a header counts: its rows (or containers) and their distinct ports.
type groupCount struct{ rows, ports int }

// text is a header's counts: containers for compose and containers groups, processes for the
// others.
func (c groupCount) text(g model.GroupKind) string {
	one, many := "process", "processes"
	if g == model.GroupCompose || g == model.GroupContainers {
		one, many = "container", "containers"
	}
	return " · " + count(c.rows, one, many) + " · " + count(c.ports, "port", "ports")
}

// count is n with the singular or plural noun.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// tableCache holds what the table derives from all the rows, not just the visible ones, so a
// key press or a tick redraws without walking every row. It belongs to one m.rows: every
// rebuild flattens into a new slice, which makes the cache stale.
type tableCache struct {
	rows    []model.Row                 // the m.rows it was computed for
	counts  map[model.RowKey]groupCount // header counts
	kids    map[model.RowKey]bool       // rows with children when expanded and unfiltered
	longest int                         // widest name cell among the process and container rows
	short   bool                        // tags shown as "!" (shortTags) when longest was measured
}

// cache returns the table cache for the current rows, computing it when they changed.
func (m *Model) cache() *tableCache {
	c := &m.tcache
	short := m.shortTags()
	if c.counts != nil && len(c.rows) == len(m.rows) && (len(m.rows) == 0 || &c.rows[0] == &m.rows[0]) {
		if c.short != short { // a resize across commandWidth changes the tags drawn, not the rows
			c.short = short
			c.measure(m)
		}
		return c
	}
	c.rows, c.short = m.rows, short
	c.counts, c.kids = m.groupCounts()
	c.measure(m)
	return c
}

// measure sets longest from the name cells as they are drawn.
func (c *tableCache) measure(m *Model) {
	c.longest = 0
	for i, r := range m.rows {
		if r.Key.Header == model.GroupNone {
			c.longest = max(c.longest, ansi.StringWidth(m.nameCellWith(i, c.kids, c.short)))
		}
	}
}

// groupCounts counts the rows and distinct ports of each group as the a and d toggles show
// them, expanded and unfiltered, so a header's counts stay put when it is collapsed or
// filtered. Dimmed rows count, since they are drawn. Compose and containers groups count
// distinct containers, since Linux runs a docker-proxy per published port and address
// family. It also reports which rows have children in those rows.
func (m *Model) groupCounts() (map[model.RowKey]groupCount, map[model.RowKey]bool) {
	counts := map[model.RowKey]groupCount{}
	kids := map[model.RowKey]bool{}
	var header model.RowKey
	var seen map[uint16]bool
	var ids map[string]bool
	view := model.ViewOptions{ShowAll: m.view.ShowAll, HideContainers: m.view.HideContainers}
	all := model.Flatten(m.upd.Snapshot, view)
	for i, r := range all {
		if i+1 < len(all) && all[i+1].Depth > r.Depth {
			kids[r.Key] = true
		}
		if r.Key.Header != model.GroupNone {
			header, seen, ids = r.Key, map[uint16]bool{}, map[string]bool{}
			continue
		}
		c := counts[header]
		switch id := containerID(r); {
		case header.Header != model.GroupCompose && header.Header != model.GroupContainers:
			c.rows++
		case id != "" && !ids[id]:
			ids[id] = true
			c.rows++
		}
		for _, p := range ports(r) {
			if !seen[p.n] {
				seen[p.n] = true
				c.ports++
			}
		}
		counts[header] = c
	}
	return counts, kids
}

// containerID is the container a row stands for: its container, or the one its process holds
// ports for.
func containerID(r model.Row) string {
	switch {
	case r.Container != nil:
		return r.Container.ID
	case r.Process != nil:
		return r.Process.ContainerID
	}
	return ""
}

// tableKey handles the keys the global switch in key does not.
func (m *Model) tableKey(k tea.KeyPressMsg) tea.Cmd {
	page := max(m.height-4, 1) // about the table's rows: the header, titles and footer take the rest
	switch k.String() {
	case "up", "k":
		m.moveTo(m.selIdx - 1)
	case "down", "j":
		m.moveTo(m.selIdx + 1)
	case "pgup":
		m.moveTo(m.selIdx - page)
	case "pgdown":
		m.moveTo(m.selIdx + page)
	case "home", "g":
		m.moveTo(0)
	case "end", "G":
		m.moveTo(len(m.rows) - 1)
	case "left", "h":
		m.collapse()
	case "right", "l":
		if m.filter == "" && m.selIdx >= 0 && m.view.Collapsed[m.sel] {
			delete(m.view.Collapsed, m.sel)
			m.rebuild()
		}
	case "a":
		m.view.ShowAll = !m.view.ShowAll
		m.rebuild()
	case "d":
		m.view.HideContainers = !m.view.HideContainers
		m.rebuild()
	case "s":
		m.view.Sort = (m.view.Sort + 1) % model.SortMode(len(sortNames))
		m.rebuild()
	}
	return nil
}

// collapse collapses the selected header or tree node when it is expanded and has children,
// and otherwise moves the selection to its parent row. While a filter is set it only moves:
// the filter shows every match whatever is collapsed, so a fold would not show (DEV-126).
func (m *Model) collapse() {
	i := m.selIdx
	if i < 0 || i >= len(m.rows) {
		return
	}
	r := m.rows[i]
	if m.filter == "" && !m.view.Collapsed[r.Key] && i+1 < len(m.rows) && m.rows[i+1].Depth > r.Depth {
		m.view.Collapsed[r.Key] = true
		m.rebuild()
		return
	}
	for j := i - 1; j >= 0; j-- {
		if m.rows[j].Depth < r.Depth {
			m.moveTo(j)
			return
		}
	}
}

// moveTo selects the row at index i, clamped to the rows.
func (m *Model) moveTo(i int) {
	if len(m.rows) == 0 {
		return
	}
	m.selIdx = max(0, min(i, len(m.rows)-1))
	m.sel = m.rows[m.selIdx].Key
}
