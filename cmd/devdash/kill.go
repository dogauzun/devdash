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
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// exitRefused is kill's exit code when nothing was signalled because devdash refused a
// target (pid 1, devdash or an ancestor, a container port) or the confirmation was declined.
const exitRefused = 6

// Seams for tests: kill(2), the snapshot, the terminal the confirmation is read from, and
// whether stdout is a terminal. Unit tests replace killFn with a recorder, so they never signal.
// term.IsTerminal is the TIOCGETA (darwin) or TCGETS (linux) ioctl.
var (
	killFn                   = engine.Kill
	snapshotFn               = engine.Snapshot
	stdin          io.Reader = os.Stdin
	stdinTerminal            = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
	stdoutTerminal           = func(w io.Writer) bool { f, ok := w.(*os.File); return ok && term.IsTerminal(f.Fd()) }
)

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
	// A listener reconciled to a container is that container's holder, not its process's, so
	// only the container row says which container to stop.
	hs := model.Holders(s, port)
	if len(hs) == 0 {
		return write(stdout, stderr, fmt.Sprintf("nothing listens on port %d\n", port), 0)
	}

	ko := engine.KillOptions{Tree: o.Tree, Force: o.Force}
	plans, code := planTargets(s, hs, ko, stderr)
	if code != 0 {
		return code
	}
	n := 0
	for _, p := range plans {
		n += len(p.Procs)
	}
	plan := planText(s, plans, ko, port, n)
	if code := write(stdout, stderr, plan, 0); code != 0 {
		return code
	}
	if !o.Yes {
		if code := confirmKill(plans, plan, n, stdout, stderr); code != 0 {
			return code
		}
	}
	var b bytes.Buffer
	code, signalled, ran := signalPlans(&b, plans, o, stderr)
	if !ran {
		fmt.Fprintln(stderr, "devdash: nothing was signalled")
		return exitRefused
	}
	recheckPort(ctx, &b, eo, port, signalled, o.Tree, stderr)
	// Signals were sent, so the code is their result even when the report cannot be written.
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
	}
	return code
}

// planTargets plans each holder with ko, deduped. When any holder is refused, it says why on
// stderr, then that nothing was signalled, and returns the exit code: 3 when every refusal is an
// unknown owner's (PID 0: another user's, so sudo might see it), else exitRefused.
func planTargets(s model.Snapshot, hs []model.Holder, ko engine.KillOptions, stderr io.Writer) ([]engine.Plan, int) {
	var plans []engine.Plan
	code := 0
	for _, h := range hs {
		p, err := engine.NewPlan(s, h.Key, ko)
		if err == nil {
			plans = append(plans, p)
			continue
		}
		if h.Process != nil && h.Process.PID == 0 {
			fmt.Fprintf(stderr, "devdash: %v: %s\n", err, ownerHint(s))
			code = max(code, 3)
		} else {
			msg := err.Error()
			if h.Process != nil && model.IsContainerRuntime(*h.Process) { // most likely a container's port that Docker could not name
				msg = withDocker(msg, s)
			}
			fmt.Fprintln(stderr, "devdash:", msg)
			code = exitRefused
		}
	}
	if code != 0 {
		fmt.Fprintln(stderr, "devdash: nothing was signalled")
		return nil, code
	}
	return dedupe(plans), 0
}

// confirmKill asks on the terminal before n processes are signalled, a second time when a plan
// is outside every project. It returns 0 on yes, else the exit code after saying why on stderr.
func confirmKill(plans []engine.Plan, plan string, n int, stdout, stderr io.Writer) int {
	if !stdinTerminal() {
		fmt.Fprintln(stderr, "devdash: confirmation needs a terminal; pass --yes to kill without asking")
		return 2
	}
	if !stdoutTerminal(stdout) { // redirected: the user must still see what they confirm
		fmt.Fprint(stderr, plan)
	}
	in := bufio.NewReader(stdin)
	ok := confirm(in, stderr, fmt.Sprintf("Send %s to %s?", unix.SignalName(plans[0].Signal), model.Count(n, "process", "processes")))
	if ok && slices.ContainsFunc(plans, func(p engine.Plan) bool { return p.Outside }) {
		ok = confirm(in, stderr, "The target belongs to no project, so it may be a system service. Kill it anyway?")
	}
	if !ok {
		fmt.Fprintln(stderr, "devdash: nothing was signalled")
		return exitRefused
	}
	return 0
}

// signalPlans runs Kill on each plan and writes to b one line per outcome, the signalled
// process groups, then the survivors or that every signalled process exited. It returns the
// exit code, whether each process (by pid and start time) was signalled, and whether any plan
// reached kill(2); when none did, b is not complete.
func signalPlans(b *bytes.Buffer, plans []engine.Plan, o options, stderr io.Writer) (code int, signalled map[model.RowKey]bool, ran bool) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	var survivors []string
	var groups []int
	// Keyed by (pid, start time): a pid reused by a new holder within the wait was not signalled.
	signalled = map[model.RowKey]bool{}
	for _, p := range plans {
		r, err := killFn(p, o.Timeout)
		if err != nil { // a plan NewPlan could not have made, or devdash's ancestry changed since; nothing in it was signalled
			fmt.Fprintln(stderr, "devdash: not signalled:", err)
			code = rank(code, 4) // its processes are still there, as Result.ExitCode counts an unsignalled pid
			continue
		}
		ran = true // some plan reached kill(2): the code is then its result, not "nothing signalled"
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
		return code, signalled, false
	}
	for _, g := range groups {
		fmt.Fprintf(b, "process group %d signalled\n", g)
	}
	switch {
	case len(survivors) > 0 && !o.Force:
		fmt.Fprintf(b, "survivors: %s (try --force)\n", strings.Join(survivors, ", "))
	case len(survivors) > 0:
		fmt.Fprintf(b, "survivors: %s\n", strings.Join(survivors, ", "))
	case code == 0:
		fmt.Fprintln(b, "every signalled process exited")
	}
	return code, signalled, true
}

// recheckPort takes a new snapshot and writes to b whether port is free or who still holds it.
// A socket shared after fork is credited to the lowest pid only (DEV-45), so a forked child
// can still hold the port: a holder that was not signalled gets the --tree hint outside tree
// mode. It is reported but does not change the exit code, which is about the processes that
// were signalled.
func recheckPort(ctx context.Context, b *bytes.Buffer, eo engine.Options, port uint16, signalled map[model.RowKey]bool, tree bool, stderr io.Writer) {
	after, err := snapshotFn(ctx, eo)
	after = cleanText(after)
	if err != nil {
		fmt.Fprintln(stderr, "devdash: cannot check the port again:", err)
		return
	}
	held := model.Holders(after, port)
	if len(held) == 0 {
		fmt.Fprintf(b, "port %d is free\n", port)
		return
	}
	var who []string
	other := false // a holder that was not signalled
	for _, h := range held {
		who = append(who, describe(after, h))
		other = other || h.Key.ContainerID == "" && !signalled[h.Key]
	}
	fmt.Fprintf(b, "port %d is still held by %s", port, strings.Join(who, ", "))
	if other && !tree {
		b.WriteString("; a forked child can hold it after its parent exits: try --tree")
	}
	b.WriteString("\n")
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
	projects := s.ProjectNames()
	var b bytes.Buffer
	fmt.Fprintf(&b, "kill port %d: %s, %s to %s:\n", port, ko.Mode(), unix.SignalName(plans[0].Signal), model.Count(n, "process", "processes"))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, pl := range plans {
		for _, p := range pl.Procs {
			project := projects[p.ProjectID]
			if project == "" {
				project = "-"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", p.PID, p.Name, project, p.PortList())
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
func describe(s model.Snapshot, h model.Holder) string {
	if p := h.Process; p != nil {
		if p.PID == 0 {
			return "an unknown owner (" + ownerHint(s) + ")"
		}
		return fmt.Sprintf("%d %s", p.PID, p.Name)
	}
	id := h.Key.ContainerID
	for _, c := range s.Containers {
		if c.ID == id && c.Name != "" {
			return "container " + c.Name
		}
	}
	return "container " + id
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
