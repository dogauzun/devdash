package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// stdoutWidth is the width of the terminal w writes to, in columns; 0 when it is not one or
// does not say. Tests replace it (with stdoutTerminal, kill.go).
var stdoutWidth = func(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), ioctlGetWinsize)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

// runPort answers "who has port N" from one snapshot (no CPU, so one sample): exit 0 and one
// line per listener when found, exit 1 and "free" when nothing listens. On a terminal the
// answer also says what each holder is and which port to use instead (writeAnswer); piped, it
// is the v0.1.1 output byte for byte, since scripts read those lines.
func runPort(ctx context.Context, o engine.Options, port uint16, stdout, stderr io.Writer) int {
	s, err := engine.Snapshot(ctx, o)
	if err == nil && stdoutTerminal(stdout) {
		return writeAnswer(stdout, stderr, s, port, stdoutWidth(stdout))
	}
	var found bool
	if err == nil {
		found, err = writePort(stdout, s, port)
	}
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	case !found:
		return 1
	}
	return 0
}

// writePort prints one line per listener on port: pid, name, project (or -) and bind
// address. A socket bound to both IPv4 and IPv6 (dual-stack ::) is one line; two sockets are
// two lines. A socket reconciled to a container prints the container instead of its holder:
// the holder's pid (- when unknown), the container's name, its compose project (or -), the
// address, then its image and the proxy holding the socket. When the snapshot carries a Docker
// warning, a line whose holder is unknown or is a container runtime process ends with its
// hint: the port is most likely a container's that Docker could not name. A container that publishes port
// with no socket on it (iptables only) gets one line per published address, with pid -. It
// prints "free" and returns false when nothing listens on port.
func writePort(w io.Writer, s model.Snapshot, port uint16) (bool, error) {
	projects := map[string]string{}
	for _, p := range s.Projects {
		projects[p.ID] = p.Name
	}
	hint := "run with sudo to see it"
	for _, x := range s.Warnings {
		if x.Code == "listener_owner_unreadable" {
			hint = x.Hint
		}
	}
	containers := map[string]model.Container{}
	for _, c := range s.Containers {
		containers[c.ID] = c
	}
	// container is the name, project and description columns of id's line.
	container := func(id string) (string, string, string) {
		c, ok := containers[id]
		if !ok {
			c = model.Container{ID: id}
		}
		name, project, desc := c.Name, c.ComposeProject, "container"
		if name == "" {
			name = c.ID
		}
		if project == "" {
			project = "-"
		}
		if c.Image != "" {
			desc += " (" + c.Image + ")"
		}
		return name, project, desc
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	found := false
	held := map[string]bool{} // containers with a socket on port
	for _, p := range s.Processes {
		for _, l := range p.Listeners {
			if l.Port != port {
				continue
			}
			found = true
			addr := netip.AddrPortFrom(l.Addr, l.Port)
			if l.ContainerID != "" {
				held[l.ContainerID] = true
				name, project, desc := container(l.ContainerID)
				pid := "-"
				if p.PID != 0 {
					pid, desc = strconv.Itoa(p.PID), desc+" via "+p.Name
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", pid, name, project, addr, desc)
				continue
			}
			project := projects[p.ProjectID]
			if project == "" {
				project = "-"
			}
			switch {
			case p.PID == 0:
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\towner unknown: %s\n", p.PID, p.Name, project, addr, withDocker(hint, s))
			case model.IsContainerRuntime(p) && dockerHint(s) != "":
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", p.PID, p.Name, project, addr, dockerHint(s))
			default:
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", p.PID, p.Name, project, addr)
			}
		}
	}
	for _, c := range s.Containers {
		if held[c.ID] {
			continue
		}
		var addrs []netip.Addr
		for _, m := range c.Ports {
			ip := m.HostIP
			if !ip.IsValid() { // Podman's empty host IP: every interface
				ip = netip.IPv4Unspecified()
			}
			if m.HostPort == port && m.Proto == "tcp" && !slices.Contains(addrs, ip) {
				addrs = append(addrs, ip)
			}
		}
		for _, ip := range addrs {
			found = true
			name, project, desc := container(c.ID)
			fmt.Fprintf(tw, "-\t%s\t%s\t%s\t%s, no listening socket\n", name, project, netip.AddrPortFrom(ip, port), desc)
		}
	}
	if !found {
		_, err := fmt.Fprintln(w, "free")
		return false, err
	}
	return true, tw.Flush()
}

// detailIndent starts each line writeAnswer adds under a holder, so they read as its own.
const detailIndent = "       "

// writeAnswer is the port answer on a terminal (spec "Release 1.0", the port answer):
// writePort's lines, each process holder followed once, after its last line, by holderLines;
// then, when N is held and below 65535, the next free port from the search `devdash free N+1`
// makes. Container lines and the PID 0 line keep their v1 form. width is the terminal's (0:
// unknown). A probe that fails leaves the next free line out and says why on stderr; the exit
// code stays that of the answer, since the holders found are still what was asked.
func writeAnswer(stdout, stderr io.Writer, s model.Snapshot, port uint16, width int) int {
	s = cleanText(s)
	var b bytes.Buffer
	found, _ := writePort(&b, s, port) // a bytes.Buffer does not fail
	if !found {
		return write(stdout, stderr, b.String(), 1)
	}
	projects := map[string]*model.Project{}
	var here *model.Project
	for i := range s.Projects {
		projects[s.Projects[i].ID] = &s.Projects[i]
		if s.Projects[i].Here {
			here = &s.Projects[i]
		}
	}

	// writePort writes one line per listener on port, process by process in snapshot order, then
	// the lines of containers with no socket on it; so each process's lines are the next n.
	lines := strings.SplitAfter(b.String(), "\n")
	var out strings.Builder
	at := 0
	for _, p := range s.Processes {
		n, own := 0, false // own: a line of p's is p's, not a container's
		for _, l := range p.Listeners {
			if l.Port == port {
				n++
				own = own || l.ContainerID == ""
			}
		}
		for _, l := range lines[at : at+n] {
			out.WriteString(l)
		}
		at += n
		if own && p.PID != 0 {
			for _, d := range holderLines(p, projects[p.ProjectID], here, s.TakenAt, width) {
				out.WriteString(detailIndent + d + "\n")
			}
		}
	}
	for _, l := range lines[at:] {
		out.WriteString(l)
	}

	var nextErr error
	if port < 65535 {
		next, ok, err := freeport.Find(s, port+1, probe)
		switch {
		case err != nil:
			nextErr = err
		case ok:
			fmt.Fprintf(&out, "next free: %d\n", next)
		default:
			fmt.Fprintf(&out, "next free: none in %d-%d\n", port+1, freeport.Last(port+1))
		}
	}
	code := write(stdout, stderr, out.String(), 0)
	if nextErr != nil {
		fmt.Fprintln(stderr, "devdash: next free:", nextErr)
	}
	return code
}

// cleanText is a copy of s with every text writePort and runKill print passed through model.Clean:
// process, project and container names, container IDs, images and compose projects, warning
// hints and address zones. Those come from other processes (a Linux comm set with
// PR_SET_NAME, a directory name), so raw they could drive the terminal, and a newline in one
// would shift the lines writeAnswer inserts onto the wrong holder. Piped port output keeps them
// raw, as v0.1.1 printed them. s itself is not changed: a snapshot is never mutated.
func cleanText(s model.Snapshot) model.Snapshot {
	s.Processes = slices.Clone(s.Processes)
	for i := range s.Processes {
		p := &s.Processes[i]
		p.Name, p.ContainerID = model.Clean(p.Name), model.Clean(p.ContainerID)
		p.Listeners = slices.Clone(p.Listeners)
		for j := range p.Listeners {
			l := &p.Listeners[j]
			l.ContainerID = model.Clean(l.ContainerID)
			if z := l.Addr.Zone(); z != "" {
				l.Addr = l.Addr.WithZone(model.Clean(z))
			}
		}
	}
	s.Projects = slices.Clone(s.Projects)
	for i := range s.Projects {
		s.Projects[i].Name = model.Clean(s.Projects[i].Name)
	}
	s.Containers = slices.Clone(s.Containers)
	for i := range s.Containers {
		c := &s.Containers[i]
		c.ID, c.Name, c.Image, c.ComposeProject = model.Clean(c.ID), model.Clean(c.Name), model.Clean(c.Image), model.Clean(c.ComposeProject)
	}
	s.Warnings = slices.Clone(s.Warnings)
	for i := range s.Warnings {
		s.Warnings[i].Hint = model.Clean(s.Warnings[i].Hint)
	}
	return s
}

// holderLines are what the port answer says under process holder p, unindented: its command,
// cut to width with "…" counting the indent (not when width is 0, unknown), and left out when
// argv is unknown; where it runs, how long it has been up and whether in the Here repository;
// its tags when it has any. pr is p's project and here the Here project, either nil; uptime
// counts to now, the snapshot's time. Snapshot text is cleaned, since it goes to a terminal.
func holderLines(p model.Process, pr, here *model.Project, now time.Time, width int) []string {
	var lines []string
	if len(p.Argv) > 0 {
		cmd := model.Clean(p.Command())
		if width > 0 {
			cmd = ansi.Truncate(cmd, max(width-len(detailIndent), 1), "…")
		}
		lines = append(lines, cmd)
	}
	var rest []string
	if !p.StartTime.IsZero() {
		rest = append(rest, "up "+model.Uptime(now.Sub(p.StartTime)))
	}
	if m := model.Location(pr, here); m != "" {
		rest = append(rest, m)
	}
	// Where gives way so that the uptime stays on the line: a cwd keeps its end, the directory that
	// names it, and is cut on the left (DEV-146); a project label keeps its start, the project's
	// name, and is cut on the right (DEV-171).
	where := model.Clean(cmp.Or(p.Cwd, "-"))
	if pr != nil {
		where = model.Clean(pr.Label())
	}
	ww := ansi.StringWidth(where)
	if over := len(detailIndent) + ansi.StringWidth(strings.Join(append([]string{where}, rest...), ", ")) - width; width > 0 && over > 0 && ww > 1 {
		var cut string
		if pr != nil {
			cut = ansi.Truncate(where, ww-over, "…") // a two-cell character that does not fit is dropped whole
		} else {
			cut = ansi.TruncateLeft(where, over+1, "…")
			if ansi.StringWidth(cut) > ww-over { // the cut fell inside a two-cell character, which TruncateLeft keeps
				cut = ansi.TruncateLeft(where, over+2, "…")
			}
		}
		where = cmp.Or(cut, "…") // "" when every cell is cut
	}
	lines = append(lines, strings.Join(append([]string{where}, rest...), ", "))
	if p.Tags != 0 {
		lines = append(lines, strings.Join(p.Tags.Labels(), ", "))
	}
	return lines
}

// dockerHint is the hint of the snapshot's Docker warning (docker_unreachable,
// docker_endpoint_invalid), or "" when Docker answered or is not configured.
func dockerHint(s model.Snapshot) string {
	for _, w := range s.Warnings {
		if strings.HasPrefix(w.Code, "docker_") {
			return w.Hint
		}
	}
	return ""
}

// withDocker is hint followed by the snapshot's Docker hint, when there is one.
func withDocker(hint string, s model.Snapshot) string {
	if d := dockerHint(s); d != "" {
		return hint + "; " + d
	}
	return hint
}
