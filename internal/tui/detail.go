package tui

import (
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-33 owns this file: the detail pane (full argv wrapped, cwd, project and branch,
// listeners with bind address, parent chain, start time, user, Docker socket for containers).

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
// Content longer than h is cut by the caller; there is no scrolling in v1.
func (m *Model) detailView(w, h int) string {
	split := m.width >= splitWidth
	d := &detailDoc{w: w}
	if split {
		d.w = max(w-len(detailSep), 1)
	}
	if r, ok := m.selected(); ok {
		m.detailRow(d, r)
	} else {
		d.add("nothing selected")
	}
	if !split {
		return strings.Join(d.lines, "\n")
	}
	out := make([]string, h)
	for i := range out {
		var l string
		if i < len(d.lines) {
			l = d.lines[i]
		}
		out[i] = styleDim.Render(detailSep[:1]) + detailSep[1:] + l
	}
	return strings.Join(out, "\n")
}

// detailRow writes the details of r: a header, the PID 0 owner, a process, or a container.
func (m *Model) detailRow(d *detailDoc, r model.Row) {
	s := m.upd.Snapshot
	p := r.Process
	switch {
	case r.Key.Header != model.GroupNone:
		detailHeader(d, s, r)
		return
	case p != nil && p.PID == 0:
		d.title("unknown owner")
		d.add("the process holding this port could not be read")
		detailListeners(d, p.Listeners)
		for _, w := range s.Warnings {
			if slices.Contains(detailOwnerWarnings, w.Code) && w.Hint != "" {
				d.field("hint", w.Hint)
			}
		}
		return
	case p != nil:
		m.detailProcess(d, s, p)
		if p.ContainerID == "" {
			return
		}
	case r.Container != nil:
		d.title(detailClean(r.Container.Name) + " · container")
	default:
		return
	}
	d.blank()
	detailContainer(d, r)
	if p == nil {
		d.add("no process holds the port (published by Docker)")
	}
	if m.o.DockerSocket != nil {
		if sock := m.o.DockerSocket(); sock != "" {
			d.blank()
			d.wrap("docker socket: " + detailClean(sock))
		}
	}
}

// detailProcess writes the fields of a real process.
func (m *Model) detailProcess(d *detailDoc, s model.Snapshot, p *model.Process) {
	d.title(fmt.Sprintf("%s %d · %s", detailClean(p.Name), p.PID, p.Kind))
	switch {
	case p.Unknown&model.FieldArgv != 0:
		d.field("command", "unknown")
	case len(p.Argv) == 0:
		d.field("command", "none")
	default:
		d.words("command", detailArgv(p.Argv), " ")
	}
	cwd := "unknown"
	if p.Cwd != "" {
		cwd = detailClean(p.Cwd)
	}
	d.field("cwd", cwd)
	if pr := detailProject(s, p.ProjectID); pr != nil {
		d.field("project", detailProjectTitle(pr))
		d.field("root", detailClean(pr.Root))
		if pr.Worktree && pr.MainRepo != "" {
			d.field("main repo", detailClean(pr.MainRepo))
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
		user = fmt.Sprintf("%s (uid %d)", detailClean(p.User), p.UID)
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
		v := []string{l.Proto, netip.AddrPortFrom(l.Addr, l.Port).String()}
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
		d.field("container", detailClean(r.Process.ContainerID))
		return
	}
	d.field("container", detailClean(c.Name))
	d.field("image", detailClean(c.Image))
	d.field("state", detailClean(c.State))
	if c.ComposeProject != "" {
		v := detailClean(c.ComposeProject)
		if c.ComposeService != "" {
			v += " / " + detailClean(c.ComposeService)
		}
		d.field("compose", v)
	}
	for i, pm := range c.Ports {
		v := fmt.Sprintf("%d/%s (not published)", pm.ContainerPort, detailClean(pm.Proto))
		if pm.HostPort != 0 {
			host := ":" + strconv.Itoa(int(pm.HostPort))
			if pm.HostIP.IsValid() {
				host = netip.AddrPortFrom(pm.HostIP, pm.HostPort).String()
			}
			v = fmt.Sprintf("%s -> %d/%s", host, pm.ContainerPort, detailClean(pm.Proto))
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
			d.title(detailClean(k.Group))
			return
		}
		d.title(detailProjectTitle(pr))
		d.field("root", detailClean(pr.Root))
		branch := detailClean(pr.Branch)
		if pr.Branch == "" {
			branch = "detached at " + detailClean(pr.ShortSHA)
		}
		d.field("branch", branch)
		if pr.Worktree && pr.MainRepo != "" {
			d.field("main repo", detailClean(pr.MainRepo))
		}
	case model.GroupCompose:
		d.title(detailClean(k.Group) + " (compose)")
		var names []string
		for _, c := range s.Containers {
			if c.ComposeProject == k.Group {
				names = append(names, detailClean(c.Name))
			}
		}
		if len(names) > 0 {
			d.words("members", names, ", ")
		}
	case model.GroupContainers:
		d.title("containers")
		d.add("containers without a compose project")
	default:
		d.title("other")
		d.add("processes outside every project")
	}
}

// detailProjectTitle is `name @ branch (worktree)`, with the short SHA for a detached HEAD.
func detailProjectTitle(pr *model.Project) string {
	ref := pr.Branch
	if ref == "" {
		ref = pr.ShortSHA
	}
	t := detailClean(pr.Name)
	if ref != "" {
		t += " @ " + detailClean(ref)
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
		chain = append(chain, fmt.Sprintf("%s %d", detailClean(par.Name), par.PID))
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

// detailArgv returns argv for display: an argument that is empty, holds a space or a
// character that is not printable (a terminal escape in a process title) is Go-quoted, so
// the pane shows where each argument ends and nothing in it reaches the terminal.
func detailArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsFunc(a, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return out
}

// detailClean returns s, Go-quoted when it holds a character that is not printable, so that no
// name, path or label from the snapshot can send control sequences to the terminal.
func detailClean(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return r != ' ' && !unicode.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
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

// detailDoc collects the pane's lines at content width w.
type detailDoc struct {
	w     int
	lines []string
}

func (d *detailDoc) add(l string)   { d.lines = append(d.lines, l) }
func (d *detailDoc) blank()         { d.add("") }
func (d *detailDoc) title(t string) { d.add(styleBold.Render(t)) }

// field writes a labelled value, wrapped at spaces to the value column.
func (d *detailDoc) field(label, value string) { d.words(label, strings.Split(value, " "), " ") }

// words writes a label and words joined by sep, wrapped to the value column; continuation
// lines are indented to the value column.
func (d *detailDoc) words(label string, words []string, sep string) {
	pad := strings.Repeat(" ", detailLabel)
	for i, l := range detailWrap(words, sep, d.w-detailLabel) {
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
// line; at a break, sep's visible part stays at the end of the line. A width below 1 does not
// wrap. It always returns at least one line.
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
			lines = append(lines, ansi.Cut(cur, 0, width))
			cur = ansi.Cut(cur, width, n)
		}
	}
	return append(lines, cur)
}
