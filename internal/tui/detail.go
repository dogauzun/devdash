package tui

import (
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// DEV-33 owns this file: the detail pane (full argv wrapped, cwd, project and branch,
// listeners with bind address, parent chain, start time, user, Docker socket for containers).
// DEV-86: the command is capped so those fields stay on screen, and the pane scrolls.
// DEV-136: a process's project says whether it is in this repo, and a process with a listener
// gets the next free port after its lowest, from the port line's probe (port.go), run as a
// tea.Cmd off the UI goroutine.

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
	key   model.RowKey // the row top was scrolled on
	pager              // page is max(h, 1) when the pane fits
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
	page, pos := ds.cut(lines, h, "lines %d-%d of %d, pgup/pgdn")
	if pos != "" {
		page = append(page, styleDim.Render(pos))
	}
	return page
}

// detailKey handles pgup and pgdown while the pane is open: they move the pane by the page the
// last render showed, and the next render clamps it. Every other key goes on to the table.
func (m *Model) detailKey(s string) bool {
	r, _ := m.selected() // the zero key when nothing is selected, as in detailView
	ds := m.detailScrollOf(r.Key)
	if s != "pgup" && s != "pgdown" {
		return false
	}
	ds.top = max(ds.top+scrollStep(s, ds.page), 0) // no upper clamp: the next render clamps
	return true
}

// closeDetail closes the pane; it opens again at the top.
func (m *Model) closeDetail() {
	m.detail = false
	m.dscroll.top = 0
}

// detailFree is the pane's next free port (spec "Release 1.1", Detail pane): the search from
// the shown process's lowest TCP port plus one, as `port N` and the port line run it for N, for
// one process and snapshot. The answer stays while the same process and port are asked again
// from a newer snapshot, so the field does not flicker to `…` on every refresh.
type detailFree struct {
	key   model.RowKey // the process asked for
	from  uint16       // its lowest TCP port
	taken time.Time    // TakenAt of the snapshot asked from
	seq   uint64       // tags the latest run; an answer with another tag is dropped
	ans   portAnswer   // the latest answer for key and from
	have  bool         // ans answers key and from
}

// detailFreeMsg is a next free run's answer, tagged with its run.
type detailFreeMsg struct {
	seq uint64
	ans portAnswer
}

// apply keeps the answer when it is the latest run's.
func (msg detailFreeMsg) apply(m *Model) tea.Cmd {
	if msg.seq == m.dfree.seq {
		m.dfree.ans, m.dfree.have = msg.ans, true
	}
	return nil
}

// lowestPort is p's lowest TCP port: listeners of any proto but UDP, as holds and freeport.Find
// count them; ok is false when p has none.
func lowestPort(p *model.Process) (port uint16, ok bool) {
	for _, l := range p.Listeners {
		if l.TCP() && (!ok || l.Port < port) {
			port, ok = l.Port, true
		}
	}
	return port, ok
}

// detailFreeFor is the process whose next free port the pane shows, and the port the search
// starts after: the selected row's real process, with a TCP listener below 65535, while the
// pane is on screen (not under the help overlay or the kill modal). ok is false otherwise.
func (m *Model) detailFreeFor() (p *model.Process, from uint16, ok bool) {
	if !m.detail || m.help || m.kill.active() || !m.have {
		return nil, 0, false
	}
	r, sel := m.selected()
	if !sel || r.Key.Header != model.GroupNone || r.Process == nil || r.Process.PID == 0 {
		return nil, 0, false
	}
	from, ok = lowestPort(r.Process)
	if !ok || from == 65535 {
		return nil, 0, false
	}
	return r.Process, from, true
}

// detailProbe returns the command that searches for the next free port when the pane shows a
// process it has no answer for from the latest snapshot, tagged as the latest run; nil when
// there is nothing to ask. Update calls it after every message.
func (m *Model) detailProbe() tea.Cmd {
	p, from, ok := m.detailFreeFor()
	if !ok {
		return nil
	}
	f, s, key := &m.dfree, m.upd.Snapshot, p.Key()
	if f.key == key && f.from == from && f.taken.Equal(s.TakenAt) {
		return nil
	}
	if f.key != key || f.from != from {
		f.have = false
	}
	f.key, f.from, f.taken = key, from, s.TakenAt
	f.seq++
	seq, probe := f.seq, m.o.Probe
	return func() tea.Msg { return detailFreeMsg{seq: seq, ans: probePort(s, from, false, probe)} }
}

// detailNextFree writes p's next free field after its listeners: `…` until the answer for p
// and its lowest port arrives, then the port, `none in A-B`, or the probe's error in the warning
// colour; no field without a TCP listener or when the lowest is 65535.
func (m *Model) detailNextFree(d *detailDoc, p *model.Process) {
	from, ok := lowestPort(p)
	if !ok || from == 65535 {
		return
	}
	f := m.dfree
	switch a := f.ans; {
	case !f.have || f.key != p.Key() || f.from != from:
		d.field("next free", "…")
	case a.err != nil:
		d.warn("next free", model.Clean(a.err.Error()))
	case a.found:
		d.field("next free", strconv.Itoa(int(a.next)))
	default:
		d.field("next free", fmt.Sprintf("none in %d-%d", from+1, freeport.Last(from+1)))
	}
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
				d.warn("hint", quote(w.Hint))
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
	if r.Container != nil {
		detailForwarders(d, s, r.Container, p == nil)
	}
	if m.o.DockerSocket != nil {
		if sock := m.o.DockerSocket(); sock != "" {
			d.blank()
			d.wrap("docker socket: " + quote(sock))
		}
	}
}

// detailForwarders names what holds container c's published ports. On a container row that has
// no process of its own (holders), the processes holding them: a forwarder Reconcile matched to
// several containers (OrbStack Helper, com.docker.backend) keeps its own row, as `port N` names
// it. With none, the port has no host socket (iptables only); with some, each published tcp
// port none of them holds is named (DEV-186), on a process row too, whose process is one of
// them (DEV-196).
func detailForwarders(d *detailDoc, s model.Snapshot, c *model.Container, holders bool) {
	held := map[uint16]bool{}
	for i := range s.Processes {
		p := &s.Processes[i]
		forwards := false
		for _, l := range p.Listeners {
			if l.ContainerID == c.ID {
				held[l.Port], forwards = true, true
			}
		}
		if forwards && holders {
			d.wrap(fmt.Sprintf("held by %s %d (forwards the container's port)", quote(p.Name), p.PID))
		}
	}
	if len(held) == 0 {
		d.wrap("no process holds the port (published by Docker)")
		return
	}
	for _, pm := range c.Ports {
		if pm.TCP() && pm.HostPort != 0 && !held[pm.HostPort] {
			held[pm.HostPort] = true // once per port, not per family
			d.wrap(fmt.Sprintf("no process holds port %d (published by Docker)", pm.HostPort))
		}
	}
}

// detailProcess writes the fields of a real process.
func (m *Model) detailProcess(d *detailDoc, s model.Snapshot, p *model.Process) {
	d.title(fmt.Sprintf("%s %d · %s", quote(p.Label()), p.PID, p.Kind))
	switch {
	case p.Unknown&model.FieldArgv != 0:
		d.field("command", "unknown")
	case len(p.Argv) == 0:
		d.field("command", "none")
	default:
		d.command(quoteArgv(p.Argv))
	}
	for i, t := range detailTags(s, p) {
		d.field(detailFirst(i, "tags"), t)
	}
	cwd := "unknown"
	if p.Cwd != "" {
		cwd = quote(p.Cwd)
	}
	d.field("cwd", cwd)
	if pr := detailProject(s, p.ProjectID); pr != nil {
		project := detailProjectTitle(pr)
		if loc := model.Location(pr, detailHere(s)); loc != "" {
			project += " · " + loc
		}
		d.field("project", project)
		d.field("root", quote(pr.Root))
		if pr.Worktree && pr.MainRepo != "" {
			d.field("main repo", quote(pr.MainRepo))
		}
	} else {
		d.field("project", "none")
	}
	detailListeners(d, p.Listeners)
	m.detailNextFree(d, p)
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

// detailTags returns each of p's tags with what it means (spec "Release 1.0", TUI), in the
// table's order: "orphaned: parent exited; now a child of launchd". An orphan's new parent is
// launchd on macOS; on Linux init when its ppid is 1, else the user's systemd, the subreaper
// the tag also names, when the snapshot shows it.
func detailTags(s model.Snapshot, p *model.Process) []string {
	var out []string
	if p.Tags&model.TagOrphaned != 0 {
		why := "parent exited"
		switch {
		case s.Host.OS == "darwin":
			why += "; now a child of launchd"
		case p.PPID == 1:
			why += "; now a child of init"
		case slices.ContainsFunc(s.Processes, func(q model.Process) bool { return q.PID == p.PPID && q.Name == "systemd" }):
			why += "; now a child of the user's systemd"
		}
		out = append(out, model.TagOrphaned.Labels()[0]+": "+why)
	}
	if p.Tags&model.TagCwdDeleted != 0 {
		out = append(out, model.TagCwdDeleted.Labels()[0]+": working directory deleted")
	}
	return out
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
		// Neither branch nor SHA is an unknown branch (DEV-152): no field, as the title has no ref.
		switch {
		case pr.Branch != "":
			d.field("branch", quote(pr.Branch))
		case pr.ShortSHA != "":
			d.field("branch", "detached at "+quote(pr.ShortSHA))
		}
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

// detailHere returns the snapshot's Here project, the one devdash was run from, or nil.
func detailHere(s model.Snapshot) *model.Project {
	for i := range s.Projects {
		if s.Projects[i].Here {
			return &s.Projects[i]
		}
	}
	return nil
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

// warn writes a labelled value like field, in the warning colour.
func (d *detailDoc) warn(label, value string) {
	lines := detailWrap(strings.Split(value, " "), " ", d.w-detailLabel)
	for i, l := range lines {
		lines[i] = styleWarn.Render(l)
	}
	d.labelled(label, lines)
}

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
