package tui

import (
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-33 owns this file: the detail pane (full argv wrapped, cwd, project and branch,
// listeners with bind address, parent chain, start time, user, Docker socket for containers).
// DEV-86: the command is capped so those fields stay on screen, and the pane scrolls.

// Detail pane layout.
const (
	detailMax      = 80    // widest split pane
	detailLabel    = 10    // label column: the longest labels ("main repo", "listeners", "container") and a space
	detailSep      = "| "  // left edge of the split pane, so it never runs into the table's last column
	detailChain    = " < " // between the entries of the parent chain
	detailMaxChain = 64    // parent chain entries; the start-time rule rules out loops, this is a backstop
)

// detailOwnerWarnings are the warning codes that explain an unknown listener owner.
var detailOwnerWarnings = []string{"listener_owner_unreadable", "pcblist_unavailable", "proc_hidepid"}

// detailWidth is the width of the right split at w columns (w >= splitWidth): two fifths of
// the screen, at most detailMax, and never so wide that the table gets fewer than minWidth
// columns (so 40 at 120 columns).
func detailWidth(w int) int { return max(min(w*2/5, detailMax, w-minWidth), 1) }

// detailView draws the selected row's details in w by h. As the right split it draws exactly h
// lines, each starting with detailSep; as the full-screen overlay it draws only its content.
// The command takes at most a third of h, so the fields after it stay on screen; a longer
// command ends with "… +N lines" and is written whole as "full argv" at the end of the pane.
// A pane longer than h scrolls (detailPage).
func (m *Model) detailView(w, h int) string {
	split := m.width >= splitWidth
	d := &detailDoc{w: w, cap: max(h/3, 2)}
	if split {
		d.w = max(w-len(detailSep), 1)
	}
	r, ok := m.selected()
	if ok {
		m.detailRow(d, r)
	} else {
		d.add("nothing selected")
	}
	if d.full != nil {
		d.blank()
		d.labelled("full argv", d.full)
	}
	lines := m.detailPage(d.lines, r.Key, h)
	if !split {
		return strings.Join(lines, "\n")
	}
	out := make([]string, h)
	for i := range out {
		var l string
		if i < len(lines) {
			l = lines[i]
		}
		out[i] = styleDim.Render(detailSep[:1]) + detailSep[1:] + l
	}
	return strings.Join(out, "\n")
}

// detailScroll is the pane's scroll position. It belongs to one row: another selection, or
// closing the pane (tui.go), starts the pane at the top again.
type detailScroll struct {
	key  model.RowKey // the row top was scrolled on
	top  int          // first pane line shown
	page int          // pane lines the last render showed: the pgup/pgdown step
}

// detailScrollOf returns the pane's scroll position for the row with key, at the top for a row
// other than the one it was scrolled on.
func (m *Model) detailScrollOf(key model.RowKey) *detailScroll {
	if m.dscroll.key != key {
		m.dscroll.key, m.dscroll.top = key, 0
	}
	return &m.dscroll
}

// detailPage returns what fits of the pane's lines in h for the row with key: all of them
// when they fit, otherwise h-1 lines from the scroll position (clamped here, and the page size
// kept for the keys) and a dim position line, or a single line when h is 1.
func (m *Model) detailPage(lines []string, key model.RowKey, h int) []string {
	ds := m.detailScrollOf(key)
	if h < 1 || len(lines) <= h {
		ds.top, ds.page = 0, max(h, 1)
		return lines
	}
	n := max(h-1, 1)
	ds.page = n
	ds.top = max(min(ds.top, len(lines)-n), 0)
	page := lines[ds.top : ds.top+n : ds.top+n]
	if n < h {
		page = append(page, styleDim.Render(fmt.Sprintf("lines %d-%d of %d, pgup/pgdn", ds.top+1, ds.top+n, len(lines))))
	}
	return page
}

// detailKey handles pgup and pgdown while the pane is open: they move the pane by the page the
// last render showed, and the next render clamps it. Every other key goes on to the table.
func (m *Model) detailKey(s string) bool {
	r, _ := m.selected() // the zero key when nothing is selected, as in detailView
	ds := m.detailScrollOf(r.Key)
	step := max(ds.page, 1)
	switch s {
	case "pgup":
		ds.top = max(ds.top-step, 0)
	case "pgdown":
		ds.top += step
	default:
		return false
	}
	return true
}

// closeDetail closes the pane; it opens again at the top.
func (m *Model) closeDetail() {
	m.detail = false
	m.dscroll.top = 0
}

// detailRow writes the details of r: a header, the PID 0 owner (a container's when Reconcile
// matched its port), a process, or a container.
func (m *Model) detailRow(d *detailDoc, r model.Row) {
	s := m.upd.Snapshot
	p := r.Process
	switch {
	case r.Key.Header != model.GroupNone:
		detailHeader(d, s, r)
		return
	case p != nil && p.PID == 0 && p.ContainerID != "":
		// A container's published port whose proxy devdash cannot read (docker-proxy is root's):
		// the port is explained, so no sudo hint, and the container's fields follow.
		title := "unknown owner"
		if r.Container != nil {
			title = quote(r.Container.Name) + " · container"
		}
		d.title(title)
		d.wrap("published by Docker; the process holding this port could not be read")
		detailListeners(d, p.Listeners)
	case p != nil && p.PID == 0:
		d.title("unknown owner")
		d.wrap("the process holding this port could not be read")
		detailListeners(d, p.Listeners)
		for _, w := range s.Warnings {
			if slices.Contains(detailOwnerWarnings, w.Code) && w.Hint != "" {
				d.field("hint", quote(w.Hint))
			}
		}
		return
	case p != nil:
		m.detailProcess(d, s, p)
		if p.ContainerID == "" {
			return
		}
	case r.Container != nil:
		d.title(quote(r.Container.Name) + " · container")
	default:
		return
	}
	d.blank()
	detailContainer(d, r)
	if p == nil {
		d.wrap("no process holds the port (published by Docker)")
	}
	if m.o.DockerSocket != nil {
		if sock := m.o.DockerSocket(); sock != "" {
			d.blank()
			d.wrap("docker socket: " + quote(sock))
		}
	}
}

// detailProcess writes the fields of a real process.
func (m *Model) detailProcess(d *detailDoc, s model.Snapshot, p *model.Process) {
	d.title(fmt.Sprintf("%s %d · %s", quote(p.Name), p.PID, p.Kind))
	switch {
	case p.Unknown&model.FieldArgv != 0:
		d.field("command", "unknown")
	case len(p.Argv) == 0:
		d.field("command", "none")
	default:
		d.command(quoteArgv(p.Argv))
	}
	cwd := "unknown"
	if p.Cwd != "" {
		cwd = quote(p.Cwd)
	}
	d.field("cwd", cwd)
	if pr := detailProject(s, p.ProjectID); pr != nil {
		d.field("project", detailProjectTitle(pr))
		d.field("root", quote(pr.Root))
		if pr.Worktree && pr.MainRepo != "" {
			d.field("main repo", quote(pr.MainRepo))
		}
	} else {
		d.field("project", "none")
	}
	detailListeners(d, p.Listeners)
	if chain := detailParents(s, p); len(chain) > 0 {
		d.words("parents", chain, detailChain)
	} else {
		d.field("parents", "none")
	}
	if !p.StartTime.IsZero() {
		age := max(m.o.Now().Sub(p.StartTime), 0)
		d.words("started", []string{p.StartTime.Local().Format(time.DateTime), "(" + ago(age) + " ago)"}, " ")
	}
	user := fmt.Sprintf("uid %d", p.UID)
	if p.User != "" {
		user = fmt.Sprintf("%s (uid %d)", quote(p.User), p.UID)
	}
	d.field("user", user)
	cpu := "unknown"
	switch {
	case p.Unknown&model.FieldCPU != 0:
	case math.IsNaN(p.CPUPercent):
		cpu = "–" // first sample
	default:
		cpu = strconv.FormatFloat(p.CPUPercent, 'f', 1, 64) + "%"
	}
	d.field("cpu", cpu)
	mem := "unknown"
	if p.Unknown&model.FieldMem == 0 {
		mem = detailBytes(p.RSSBytes)
	}
	d.field("mem", mem)
	if names := p.Unknown.Names(); len(names) > 0 {
		d.field("unknown", strings.Join(names, ", "))
	}
}

// detailListeners writes one line per listener: protocol, bind address and port.
func detailListeners(d *detailDoc, ls []model.Listener) {
	if len(ls) == 0 {
		d.field("listeners", "none")
		return
	}
	for i, l := range ls {
		v := []string{quote(l.Proto), netip.AddrPortFrom(l.Addr, l.Port).String()}
		if l.Addr.IsUnspecified() {
			v = append(v, "(every interface)") // wraps as one word
		}
		d.words(detailFirst(i, "listeners"), v, " ")
	}
}

// detailContainer writes the container of r: its name, image, state, compose labels and port
// mappings, or only its ID when Docker's list does not have it.
func detailContainer(d *detailDoc, r model.Row) {
	c := r.Container
	if c == nil {
		d.field("container", quote(r.Process.ContainerID))
		return
	}
	d.field("container", quote(c.Name))
	d.field("image", quote(c.Image))
	d.field("state", quote(c.State))
	if c.ComposeProject != "" {
		v := quote(c.ComposeProject)
		if c.ComposeService != "" {
			v += " / " + quote(c.ComposeService)
		}
		d.field("compose", v)
	}
	for i, pm := range c.Ports {
		v := fmt.Sprintf("%d/%s (not published)", pm.ContainerPort, quote(pm.Proto))
		if pm.HostPort != 0 {
			host := ":" + strconv.Itoa(int(pm.HostPort))
			if pm.HostIP.IsValid() {
				host = netip.AddrPortFrom(pm.HostIP, pm.HostPort).String()
			}
			v = fmt.Sprintf("%s -> %d/%s", host, pm.ContainerPort, quote(pm.Proto))
		}
		d.field(detailFirst(i, "ports"), v)
	}
}

// detailFirst is label for the first line of a list and blank for the others.
func detailFirst(i int, label string) string {
	if i == 0 {
		return label
	}
	return ""
}

// detailHeader writes a group header: the project's root, branch and main repo, the compose
// project's containers, or what the group holds.
func detailHeader(d *detailDoc, s model.Snapshot, r model.Row) {
	switch k := r.Key; k.Header {
	case model.GroupProject:
		pr := r.Project
		if pr == nil {
			d.title(quote(k.Group))
			return
		}
		d.title(detailProjectTitle(pr))
		d.field("root", quote(pr.Root))
		branch := quote(pr.Branch)
		if pr.Branch == "" {
			branch = "detached at " + quote(pr.ShortSHA)
		}
		d.field("branch", branch)
		if pr.Worktree && pr.MainRepo != "" {
			d.field("main repo", quote(pr.MainRepo))
		}
	case model.GroupCompose:
		d.title(quote(k.Group) + " (compose)")
		var names []string
		for _, c := range s.Containers {
			if c.ComposeProject == k.Group {
				names = append(names, quote(c.Name))
			}
		}
		if len(names) > 0 {
			d.words("members", names, ", ")
		}
	case model.GroupContainers:
		d.title("containers")
		d.wrap("containers without a compose project")
	default:
		d.title("other")
		d.wrap("processes outside every project")
	}
}

// detailProjectTitle is `name @ branch (worktree)`, with the short SHA for a detached HEAD.
func detailProjectTitle(pr *model.Project) string {
	ref := pr.Branch
	if ref == "" {
		ref = pr.ShortSHA
	}
	t := quote(pr.Name)
	if ref != "" {
		t += " @ " + quote(ref)
	}
	if pr.Worktree {
		t += " (worktree)"
	}
	return t
}

// detailProject returns the snapshot's project with id, or nil.
func detailProject(s model.Snapshot, id string) *model.Project {
	if id == "" {
		return nil
	}
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i]
		}
	}
	return nil
}

// detailParents returns p's ancestors from its parent up, as "name pid", following PPID through
// the snapshot until pid 0 (no parent). A parent that is missing from the snapshot, or that
// did not start strictly before its child by (start time, pid) and so is a reused pid, ends
// the chain as a bare "pid N". The start-time rule is Flatten's; it makes cycles impossible.
func detailParents(s model.Snapshot, p *model.Process) []string {
	byPID := make(map[int]*model.Process, len(s.Processes))
	for i := range s.Processes {
		if q := &s.Processes[i]; q.PID != 0 {
			byPID[q.PID] = q
		}
	}
	var chain []string
	for c := p; c.PPID > 0 && len(chain) < detailMaxChain; {
		par := byPID[c.PPID]
		if par == nil || !detailStartedBefore(par, c) {
			chain = append(chain, fmt.Sprintf("pid %d", c.PPID))
			break
		}
		chain = append(chain, fmt.Sprintf("%s %d", quote(par.Name), par.PID))
		c = par
	}
	return chain
}

// detailStartedBefore reports whether a started strictly before b by (start time, pid).
func detailStartedBefore(a, b *model.Process) bool {
	if c := a.StartTime.Compare(b.StartTime); c != 0 {
		return c < 0
	}
	return a.PID < b.PID
}

// detailBytes formats n in binary units: "512 B", "1.5 KiB", "178.9 MiB".
func detailBytes(n uint64) string {
	if n < 1<<10 {
		return fmt.Sprintf("%d B", n)
	}
	v, units := float64(n)/(1<<10), []string{"KiB", "MiB", "GiB", "TiB"}
	i := 0
	for ; v >= 1<<10 && i < len(units)-1; i++ {
		v /= 1 << 10
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// detailDoc collects the pane's lines at content width w. A command longer than cap lines is
// cut there (command) and kept whole in full for the end of the pane.
type detailDoc struct {
	w     int
	lines []string
	cap   int
	full  []string
}

func (d *detailDoc) add(l string)   { d.lines = append(d.lines, l) }
func (d *detailDoc) blank()         { d.add("") }
func (d *detailDoc) title(t string) { d.add(styleBold.Render(t)) }

// field writes a labelled value, wrapped at spaces to the value column.
func (d *detailDoc) field(label, value string) { d.words(label, strings.Split(value, " "), " ") }

// words writes a label and words joined by sep, wrapped to the value column; continuation
// lines are indented to the value column.
func (d *detailDoc) words(label string, words []string, sep string) {
	d.labelled(label, detailWrap(words, sep, d.w-detailLabel))
}

// command writes argv wrapped to the value column in at most cap lines: a longer one keeps
// cap-1 lines, ends with "… +N lines" and is kept whole in full.
func (d *detailDoc) command(argv []string) {
	lines := detailWrap(argv, " ", d.w-detailLabel)
	if keep := d.cap - 1; keep > 0 && len(lines) > d.cap {
		d.full = lines
		lines = append(lines[:keep:keep], fmt.Sprintf("… +%d lines", len(lines)-keep))
	}
	d.labelled("command", lines)
}

// labelled writes lines in the value column, label before the first.
func (d *detailDoc) labelled(label string, lines []string) {
	pad := strings.Repeat(" ", detailLabel)
	for i, l := range lines {
		prefix := pad
		if i == 0 {
			prefix = label + pad[min(len(label), detailLabel-1):]
		}
		d.add(prefix + l)
	}
}

// wrap writes s wrapped at spaces to the full width, continuation lines indented by two.
func (d *detailDoc) wrap(s string) {
	for i, l := range detailWrap(strings.Split(s, " "), " ", d.w-2) {
		if i > 0 {
			l = "  " + l
		}
		d.add(l)
	}
}

// detailWrap joins words with sep into lines at most width cells wide. A word wider than the
// width starts its own line and is cut into width-wide pieces, its last piece continuing the
// line; at a break, sep's visible part stays at the end of the line. A grapheme wider than the
// width (a double-width rune at width 1) is a piece of its own. A width below 1 does not wrap.
// It always returns at least one line.
func detailWrap(words []string, sep string, width int) []string {
	if width < 1 {
		return []string{strings.Join(words, sep)}
	}
	tail := strings.TrimRight(sep, " ")
	var lines []string
	cur := ""
	for i, w := range words {
		switch {
		case i == 0:
		case ansi.StringWidth(cur+sep+w) <= width:
			cur += sep
		default:
			lines = append(lines, cur+tail)
			cur = ""
		}
		cur += w
		for n := ansi.StringWidth(cur); n > width; n = ansi.StringWidth(cur) {
			piece := ansi.Cut(cur, 0, width)
			if piece == "" { // the first grapheme alone is too wide: it still has to make progress
				piece, _ = ansi.FirstGraphemeCluster(cur, ansi.GraphemeWidth)
				if piece == cur {
					break // the last piece stays on the line, overflowing it
				}
			}
			lines = append(lines, piece)
			cur = strings.TrimPrefix(cur, piece)
		}
	}
	return append(lines, cur)
}
