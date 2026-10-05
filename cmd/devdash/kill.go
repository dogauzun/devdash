package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// exitRefused is kill's exit code when nothing was signalled because devdash refused a
// target (pid 1, devdash or an ancestor, a container port) or the confirmation was declined.
const exitRefused = 6

// Seams for tests: kill(2), the snapshot, the terminal the confirmation is read from, and
// whether stdout is a terminal. Unit tests replace killFn with a recorder, so they never signal.
var (
	killFn                   = engine.Kill
	snapshotFn               = engine.Snapshot
	stdin          io.Reader = os.Stdin
	stdinTerminal            = func() bool { return isTerminal(os.Stdin) }
	stdoutTerminal           = func(w io.Writer) bool { f, ok := w.(*os.File); return ok && isTerminal(f) }
)

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// target is one row holding port N: a process (or PID 0 pseudo-process), or a container
// whose published port has no process behind it.
type target struct {
	key     model.RowKey
	sudo    bool // the owner is unknown (PID 0): it is another user's, so sudo might see it
	runtime bool // the owner is a container runtime process (model.IsContainerRuntime)
}

// runKill stops whatever listens on TCP port N, from one snapshot: one plan per owner, shown
// together and confirmed once; if any owner is refused, nothing is signalled.
func runKill(ctx context.Context, o options, eo engine.Options, port uint16, stdout, stderr io.Writer) int {
	s, err := snapshotFn(ctx, eo)
	if err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	// Every name kill prints, the engine's refusals included, comes from s or after, so both are
	// cleaned, piped or not (DEV-150). Plans match processes by pid and start time, which Clean keeps.
	s = cleanText(s)
	ts := targets(s, port)
	if len(ts) == 0 {
		return write(stdout, stderr, fmt.Sprintf("nothing listens on port %d\n", port), 0)
	}

	ko := engine.KillOptions{Tree: o.Tree, Force: o.Force}
	var plans []engine.Plan
	code := 0
	for _, t := range ts {
		p, err := engine.NewPlan(s, t.key, ko)
		if err == nil {
			plans = append(plans, p)
			continue
		}
		if t.sudo {
			fmt.Fprintf(stderr, "devdash: %v: %s\n", err, ownerHint(s))
			code = max(code, 3)
		} else {
			msg := err.Error()
			if t.runtime { // most likely a container's port that Docker could not name
				msg = withDocker(msg, s)
			}
			fmt.Fprintln(stderr, "devdash:", msg)
			code = exitRefused
		}
	}
	if code != 0 {
		fmt.Fprintln(stderr, "devdash: nothing was signalled")
		return code
	}
	plans = dedupe(plans)

	n := 0
	for _, p := range plans {
		n += len(p.Procs)
	}
	plan := planText(s, plans, ko, port, n)
	if code := write(stdout, stderr, plan, 0); code != 0 {
		return code
	}
	if !o.Yes {
		if !stdinTerminal() {
			fmt.Fprintln(stderr, "devdash: confirmation needs a terminal; pass --yes to kill without asking")
			return 2
		}
		if !stdoutTerminal(stdout) { // redirected: the user must still see what they confirm
			fmt.Fprint(stderr, plan)
		}
		in := bufio.NewReader(stdin)
		ok := confirm(in, stderr, fmt.Sprintf("Send %s to %s?", sigName(plans[0].Signal), count(n, "process")))
		if ok && slices.ContainsFunc(plans, func(p engine.Plan) bool { return p.Outside }) {
			ok = confirm(in, stderr, "The target belongs to no project, so it may be a system service. Kill it anyway?")
		}
		if !ok {
			fmt.Fprintln(stderr, "devdash: nothing was signalled")
			return exitRefused
		}
	}

	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	var survivors []string
	var groups []int
	// Keyed by (pid, start time): a pid reused by a new holder within the wait was not signalled.
	signalled := map[model.RowKey]bool{}
	ran := false // some plan reached kill(2): the code is then its result, not "nothing signalled"
	for _, p := range plans {
		r, err := killFn(p, o.Timeout)
		if err != nil { // a plan NewPlan could not have made, or devdash's ancestry changed since; nothing in it was signalled
			fmt.Fprintln(stderr, "devdash: not signalled:", err)
			code = rank(code, 4) // its processes are still there, as Result.ExitCode counts an unsignalled pid
			continue
		}
		ran = true
		code = rank(code, r.ExitCode())
		if r.Group != 0 {
			groups = append(groups, r.Group)
		}
		for _, oc := range r.Outcomes {
			fmt.Fprintf(tw, "%d\t%s\t%s\n", oc.Process.PID, oc.Process.Name, outcome(oc))
			signalled[oc.Process.Key()] = oc.Signalled
		}
		for _, sv := range r.Survivors() {
			survivors = append(survivors, fmt.Sprintf("%d %s", sv.PID, sv.Name))
		}
	}
	_ = tw.Flush()
	if !ran {
		fmt.Fprintln(stderr, "devdash: nothing was signalled")
		return exitRefused
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "process group %d signalled\n", g)
	}
	switch {
	case len(survivors) > 0 && !o.Force:
		fmt.Fprintf(&b, "survivors: %s (try --force)\n", strings.Join(survivors, ", "))
	case len(survivors) > 0:
		fmt.Fprintf(&b, "survivors: %s\n", strings.Join(survivors, ", "))
	case code == 0:
		fmt.Fprintln(&b, "every signalled process exited")
	}

	// A socket shared after fork is credited to the lowest pid only (DEV-45), so a forked
	// child can still hold the port. It is reported but does not change the exit code, which
	// is about the processes that were signalled.
	after, err := snapshotFn(ctx, eo)
	after = cleanText(after)
	if err != nil {
		fmt.Fprintln(stderr, "devdash: cannot check the port again:", err)
	} else if held := targets(after, port); len(held) > 0 {
		var who []string
		other := false // a holder that was not signalled
		for _, t := range held {
			who = append(who, describe(after, t))
			other = other || t.key.ContainerID == "" && !signalled[t.key]
		}
		fmt.Fprintf(&b, "port %d is still held by %s", port, strings.Join(who, ", "))
		if other && !o.Tree {
			b.WriteString("; a forked child can hold it after its parent exits: try --tree")
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "port %d is free\n", port)
	}
	// Signals were sent, so the code is their result even when the report cannot be written.
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
	}
	return code
}

// targets are the rows holding TCP port N, model.Holders: every process with a listener on it,
// and every container that publishes it. A listener reconciled to a container is that
// container's target, not its holder's, so only the container row says which container to stop.
func targets(s model.Snapshot, port uint16) []target {
	var ts []target
	for _, h := range model.Holders(s, port) {
		t := target{key: h.Key}
		if p := h.Process; p != nil {
			t.sudo, t.runtime = p.PID == 0, model.IsContainerRuntime(*p)
		}
		ts = append(ts, t)
	}
	return ts
}

// dedupe drops a plan whose every pid an earlier plan already signals (in tree mode, an owner
// that is a descendant of another owner). Larger plans go first, so a parent's tree is
// signalled before a child's own plan could be.
func dedupe(plans []engine.Plan) []engine.Plan {
	slices.SortStableFunc(plans, func(a, b engine.Plan) int { return len(b.Procs) - len(a.Procs) })
	seen := map[int]bool{}
	var kept []engine.Plan
	for _, p := range plans {
		if !slices.ContainsFunc(p.Procs, func(q model.Process) bool { return !seen[q.PID] }) {
			continue
		}
		for _, q := range p.Procs {
			seen[q.PID] = true
		}
		kept = append(kept, p)
	}
	return kept
}

// planText is what kill will do: the mode and signal to n processes, then one line per pid in
// signal order with its name, project and ports.
func planText(s model.Snapshot, plans []engine.Plan, ko engine.KillOptions, port uint16, n int) string {
	projects := map[string]string{}
	for _, p := range s.Projects {
		projects[p.ID] = p.Name
	}
	mode := "process"
	if ko.Tree {
		mode = "tree"
	}
	if ko.Force {
		mode += ", force"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "kill port %d: %s mode, %s to %s:\n", port, mode, sigName(plans[0].Signal), count(n, "process"))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, pl := range plans {
		for _, p := range pl.Procs {
			project := projects[p.ProjectID]
			if project == "" {
				project = "-"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", p.PID, p.Name, project, ports(p))
		}
	}
	_ = tw.Flush()
	for _, pl := range plans {
		if pl.Group != 0 {
			fmt.Fprintf(&b, "%d leads its process group: one signal to the whole group, members not listed above included\n", pl.Group)
		}
	}
	return b.String()
}

// ports lists p's listening ports, sorted and without repeats, or "-".
func ports(p model.Process) string {
	var ns []int
	for _, l := range p.Listeners {
		ns = append(ns, int(l.Port))
	}
	slices.Sort(ns)
	ns = slices.Compact(ns)
	if len(ns) == 0 {
		return "-"
	}
	ss := make([]string, len(ns))
	for i, n := range ns {
		ss[i] = strconv.Itoa(n)
	}
	return strings.Join(ss, ",")
}

func outcome(o engine.Outcome) string {
	switch {
	case o.Err != nil:
		return "not signalled: " + o.Err.Error()
	case o.Signalled && o.Exited:
		return "signalled, exited"
	case o.Signalled:
		return "signalled, still running"
	case o.Exited:
		return "gone before the signal"
	}
	return "not signalled"
}

// describe names the holder of a port for the after-kill check.
func describe(s model.Snapshot, t target) string {
	if id := t.key.ContainerID; id != "" {
		for _, c := range s.Containers {
			if c.ID == id && c.Name != "" {
				return "container " + c.Name
			}
		}
		return "container " + id
	}
	for _, p := range s.Processes {
		if p.Key() == t.key {
			if p.PID == 0 {
				return "an unknown owner (" + ownerHint(s) + ")"
			}
			return fmt.Sprintf("%d %s", p.PID, p.Name)
		}
	}
	return "?"
}

// ownerHint is the listener_owner_unreadable warning's hint followed by the Docker warning's,
// when there is one, as `port N` shows them.
func ownerHint(s model.Snapshot) string {
	for _, w := range s.Warnings {
		if w.Code == "listener_owner_unreadable" {
			return withDocker(w.Hint, s)
		}
	}
	return withDocker("run with sudo to see owners", s)
}

// confirm asks q on w and reads one line: only y or yes (any case) is a yes; EOF is a no.
func confirm(in *bufio.Reader, w io.Writer, q string) bool {
	fmt.Fprintf(w, "%s [y/N] ", q)
	line, err := in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		fmt.Fprintln(w)
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// rank merges two kill exit codes: permission denied (3) wins, then survivors (4), then 0.
func rank(a, b int) int {
	for _, c := range []int{3, 4} {
		if a == c || b == c {
			return c
		}
	}
	return 0
}

// write prints text on stdout and returns code, or exitFailed when stdout cannot be written.
func write(stdout, stderr io.Writer, text string, code int) int {
	if _, err := io.WriteString(stdout, text); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	return code
}

func sigName(s syscall.Signal) string {
	if s == syscall.SIGKILL {
		return "SIGKILL"
	}
	return "SIGTERM"
}

func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ses", n, what)
}
