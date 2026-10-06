package engine

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// DefaultKillTimeout is how long Kill waits for signalled processes to exit (--timeout).
const DefaultKillTimeout = 3 * time.Second

const killPoll = 100 * time.Millisecond

// KillOptions chooses the kill mode. Tree and Force are independent, as the CLI's --tree and
// --force flags are: Force only changes the signal of whichever set Tree chose.
type KillOptions struct {
	Tree  bool // the target and all its descendants, parent first, plus its process group when it leads one
	Force bool // SIGKILL instead of SIGTERM
}

// Plan is exactly what Kill will signal. Callers show it (the confirmation modal, the CLI)
// and then pass the same value to Kill; nothing outside it is ever signalled.
type Plan struct {
	// Procs are the processes that receive Signal, in order; Procs[0] is the target, and in
	// tree mode every parent comes before its children. Members of the target's process group
	// that are not its descendants follow, as far as the snapshot shows them.
	Procs  []model.Process
	Signal syscall.Signal // SIGTERM, or SIGKILL with Force
	// Group is the target's pid when it leads its process group in tree mode: the signal then
	// goes to -Group instead of the target alone, which also reaches group members the
	// snapshot does not show. 0 otherwise.
	Group int
	// Outside is true when the target belongs to no project group; callers ask for a second
	// confirmation, because those are usually system services.
	Outside bool
}

// Refusal is the error NewPlan returns for a target devdash will not signal.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "refused: " + r.Reason }

func refuse(format string, a ...any) error { return &Refusal{Reason: fmt.Sprintf(format, a...)} }

// ErrPermission is an Outcome's Err when kill(2) failed with EPERM (another user's process).
var ErrPermission = errors.New("permission denied, run with sudo")

// ErrStartTime is an Outcome's Err when the pid now has another start time than in the plan:
// the planned process is gone and its pid was reused (or, on Linux, the wall clock was stepped
// since devdash started). It was not signalled.
var ErrStartTime = errors.New("start time changed (pid reused?), not signalled")

// Outcome is what happened to one planned process.
type Outcome struct {
	Process   model.Process
	Signalled bool  // the signal was delivered, to the pid or through the process group
	Exited    bool  // gone (or a zombie) by the end of the wait; also when it was gone before the signal
	Err       error // why it was not signalled: ErrPermission, ErrStartTime, or a start-time read error
}

// Result reports a Kill, one Outcome per Plan.Procs entry in the same order.
type Result struct {
	Outcomes []Outcome
	Group    int // pgid that was signalled, 0 if none
}

// Survivors are the signalled processes still running when the wait ended.
func (r Result) Survivors() []model.Process {
	var ps []model.Process
	for _, o := range r.Outcomes {
		if o.Signalled && !o.Exited {
			ps = append(ps, o.Process)
		}
	}
	return ps
}

// ExitCode is the CLI's kill exit code: 3 when any pid gave EPERM, else 4 when anything
// planned is still running (a survivor, or a pid that was not signalled because its start
// time could not be checked), else 0.
func (r Result) ExitCode() int {
	code := 0
	for _, o := range r.Outcomes {
		if errors.Is(o.Err, ErrPermission) {
			return 3
		}
		if !o.Exited {
			code = 4
		}
	}
	return code
}

// osys is the OS seam: unit tests replace it, so they record signals instead of sending them.
type osys struct {
	// stat returns a pid's start time, in the collector's terms, and its parent, read from
	// the OS now (collector.ProcStat), or collector.ErrGone when the pid does not exist or is a
	// zombie (on Linux, once every thread has exited, so its files are closed).
	stat    func(pid int) (start time.Time, ppid int, err error)
	getpgid func(pid int) (int, error)
	kill    func(pid int, sig syscall.Signal) error
	sleep   func(time.Duration)
	self    func() (pid, ppid int) // devdash's pid and its parent's, read now
}

var realOS = osys{stat: collector.ProcStat, getpgid: syscall.Getpgid, kill: syscall.Kill, sleep: time.Sleep,
	self: func() (int, int) { return os.Getpid(), os.Getppid() }}

// NewPlan returns what killing the process with row key key would signal, computed from s, or
// a *Refusal. It sends nothing; it only reads process group ids from the OS.
func NewPlan(s model.Snapshot, key model.RowKey, o KillOptions) (Plan, error) {
	return newPlan(s, key, o, realOS)
}

func newPlan(s model.Snapshot, key model.RowKey, o KillOptions, sy osys) (Plan, error) {
	if key.ContainerID != "" {
		name := s.ContainerName(key.ContainerID)
		return Plan{}, refuse("container %s: use docker stop %s", name, name)
	}
	if key.Header != model.GroupNone {
		return Plan{}, refuse("not a process")
	}
	target, byPID, children := index(s, key)
	if target == nil {
		if key.PID <= 0 {
			return Plan{}, refuse("pid %d is not a process", key.PID)
		}
		return Plan{}, refuse("pid %d is gone, or its pid was reused", key.PID)
	}
	refused := refusedPIDs(sy, byPID)

	plan := Plan{Procs: []model.Process{*target}, Signal: syscall.SIGTERM, Outside: target.ProjectID == ""}
	if o.Force {
		plan.Signal = syscall.SIGKILL
	}
	// No tree for the unknown owner (PID 0) or init: every process whose ppid is 0 or 1 would
	// join the plan, kept from the signal only by the refusals below (DEV-67). Both are refused.
	if o.Tree && target.PID > 1 {
		var in map[int]bool
		plan.Procs, in = tree(*target, children)
		if err := addGroup(&plan, s, in, refused, sy); err != nil {
			return Plan{}, err
		}
	}

	// One refused process refuses the whole action; nothing is skipped silently.
	for _, p := range plan.Procs {
		if err := refusal(s, p, refused); err != nil {
			return Plan{}, err
		}
	}
	return plan, nil
}

// index finds the process with row key key in s (nil when there is none) and indexes the real
// processes (pid > 0) by pid and by parent.
func index(s model.Snapshot, key model.RowKey) (target *model.Process, byPID map[int]*model.Process, children map[int][]*model.Process) {
	byPID = map[int]*model.Process{}
	children = map[int][]*model.Process{}
	for i := range s.Processes {
		p := &s.Processes[i]
		if p.Key() == key {
			target = p
		}
		if p.PID > 0 {
			byPID[p.PID] = p
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	return target, byPID, children
}

// refusedPIDs maps devdash and its ancestors to why they are refused, read fresh from the OS
// and also from the snapshot's ppid map, so a chain cut short in either one is still covered.
func refusedPIDs(sy osys, byPID map[int]*model.Process) map[int]string {
	refused := map[int]string{}
	for _, pid := range sy.chain() {
		refused[pid] = "runs devdash"
	}
	self, parent := sy.self()
	refused[self] = "is devdash itself"
	seen := map[int]bool{}
	for pid := parent; pid > 1 && !seen[pid]; {
		seen[pid] = true
		refused[pid] = "runs devdash"
		p := byPID[pid]
		if p == nil {
			break
		}
		pid = p.PPID
	}
	return refused
}

// tree is target and its descendants, breadth first (parents before children), and the set of
// their pids.
func tree(target model.Process, children map[int][]*model.Process) ([]model.Process, map[int]bool) {
	procs := []model.Process{target}
	in := map[int]bool{target.PID: true}
	for i := 0; i < len(procs); i++ {
		for _, c := range children[procs[i].PID] {
			if !in[c.PID] {
				in[c.PID] = true
				procs = append(procs, *c)
			}
		}
	}
	return procs, in
}

// addGroup, when the target (plan.Procs[0]) leads its process group, sets plan.Group and
// appends the group members s shows that are not in the plan yet (in, updated). A group holding
// a refused pid refuses the action.
func addGroup(plan *Plan, s model.Snapshot, in map[int]bool, refused map[int]string, sy osys) error {
	pg, err := sy.getpgid(plan.Procs[0].PID)
	if err != nil || pg != plan.Procs[0].PID {
		return nil
	}
	for pid, why := range refused {
		if g, err := sy.getpgid(pid); err == nil && g == pg {
			return refuse("process group %d contains pid %d, which %s", pg, pid, why)
		}
	}
	plan.Group = pg
	for i := range s.Processes {
		p := &s.Processes[i]
		if p.PID <= 0 || in[p.PID] {
			continue
		}
		if g, err := sy.getpgid(p.PID); err == nil && g == pg {
			in[p.PID] = true
			plan.Procs = append(plan.Procs, *p)
		}
	}
	return nil
}

// refusal is why p may not be signalled, or nil.
func refusal(s model.Snapshot, p model.Process, refused map[int]string) error {
	// The container is named only when it accounts for every port p holds: with a socket
	// that matched nothing (Docker Desktop's Kubernetes on 6443 next to a database's 5432),
	// docker stop would stop the database and leave that port held. Reconcile no longer gives
	// such a process a ContainerID (PR #52 review); the check stays for any snapshot that does.
	whole := p.ContainerID != "" && !slices.ContainsFunc(p.Listeners, func(l model.Listener) bool { return l.ContainerID == "" })
	switch {
	case whole:
		return refuseContainer(p, s.ContainerName(p.ContainerID))
	case model.IsContainerRuntime(p):
		return refuse("pid %d (%s) is part of the container runtime, not your service: %s", p.PID, p.Name, runtimeHint(p))
	case p.PID == 0:
		return refuse("pid 0 is not a process (the owner of this port is unknown)")
	case p.PID == 1:
		return refuse("pid 1 (%s) is init", p.Name)
	case refused[p.PID] != "":
		return refuse("pid %d (%s) %s", p.PID, p.Name, refused[p.PID])
	case p.ContainerID != "": // Reconcile marks only runtime processes and PID 0, refused above; kept refused all the same
		return refuseContainer(p, s.ContainerName(p.ContainerID))
	}
	return nil
}

func refuseContainer(p model.Process, name string) error {
	return refuse("pid %d (%s) holds a port of container %s: use docker stop %s", p.PID, p.Name, name, name)
}

// runtimeHint is how to find the container behind a runtime process's ports: docker ps
// filtered on the published port when it is Docker's and holds exactly one (com.docker.backend
// holds every container's ports), else the plain listing of the runtime's CLI.
func runtimeHint(p model.Process) string {
	var ports []uint16
	for _, l := range p.Listeners {
		if !slices.Contains(ports, l.Port) {
			ports = append(ports, l.Port)
		}
	}
	switch cli := model.RuntimeCLI(p); {
	case cli == "docker" && len(ports) == 1:
		return fmt.Sprintf("find its container with docker ps --filter publish=%d", ports[0])
	case cli != "":
		return "find the container with " + cli + " ps"
	default:
		return "find the container with docker ps or podman ps"
	}
}

// Kill signals p, waits up to timeout (DefaultKillTimeout when 0) for the signalled processes
// to exit, polling every 100 ms, and reports. Before every kill(2) the pid's start time is
// read fresh from the OS and compared with the plan, so a reused pid is never signalled. It
// returns an error, having signalled nothing, only for a Plan that NewPlan could not have
// made.
func Kill(p Plan, timeout time.Duration) (Result, error) {
	return kill(p, timeout, realOS)
}

func kill(p Plan, timeout time.Duration, sy osys) (Result, error) {
	if err := p.check(sy); err != nil {
		return Result{}, err
	}
	if timeout <= 0 {
		timeout = DefaultKillTimeout
	}
	deadline := time.Now().Add(timeout)
	r := Result{Outcomes: make([]Outcome, len(p.Procs))}
	for i, proc := range p.Procs {
		r.Outcomes[i].Process = proc
	}

	done := make([]bool, len(p.Procs)) // handled by the group step, skipped one by one below
	if p.Group != 0 {
		signalGroup(p, &r, done, sy)
	}
	for i := range r.Outcomes {
		if o := &r.Outcomes[i]; !done[i] && valid(o, sy) {
			send(sy, o.Process.PID, p.Signal, o)
		}
	}
	wait(r.Outcomes, deadline, sy)
	return r, nil
}

// valid re-validates o against a start time read from the OS now; when the process is
// gone, reused or unreadable it records that on o and reports false.
//
// ponytail: the read happens microseconds before kill(2); a pid that exits and is reused
// inside that window would still be signalled. Closing it needs pidfd (Linux only); macOS
// has no race-free way.
func valid(o *Outcome, sy osys) bool {
	start, _, err := sy.stat(o.Process.PID)
	switch {
	case errors.Is(err, collector.ErrGone):
		o.Exited = true
	case err != nil:
		o.Err = fmt.Errorf("cannot read start time, not signalled: %w", err)
	case !start.Equal(o.Process.StartTime):
		o.Err = ErrStartTime
	default:
		return true
	}
	return false
}

// send signals pid (a negative pid is a process group) and records the result on every
// outcome it stands for.
func send(sy osys, pid int, sig syscall.Signal, outs ...*Outcome) {
	err := sy.kill(pid, sig)
	for _, o := range outs {
		switch {
		case err == nil:
			o.Signalled = true
		case errors.Is(err, syscall.ESRCH):
			o.Exited = true
		case errors.Is(err, syscall.EPERM):
			o.Err = ErrPermission
		default:
			o.Err = err
		}
	}
}

// signalGroup signals the leader of p.Group (r.Outcomes[0]) and marks in done what it settled.
// A target that still leads its group gets one signal to -pgid, which reaches the whole
// group at the same moment, so no member can respawn another first. Members are found
// (and validated) just before, so they are reported as signalled even if they die at once.
// A planned member still in the group that fails validation (reused, or unreadable) would
// be reached by -pgid too, so then there is no group signal: the leader goes first alone
// and the validated rest one by one after it.
func signalGroup(p Plan, r *Result, done []bool, sy osys) {
	done[0] = true
	leader := &r.Outcomes[0]
	if !valid(leader, sy) {
		return
	}
	g, err := sy.getpgid(p.Group)
	group := err == nil && g == p.Group
	members := []*Outcome{leader}
	var idx []int
	for i := 1; group && i < len(r.Outcomes); i++ {
		if g, err := sy.getpgid(p.Procs[i].PID); err == nil && g == p.Group {
			switch o := &r.Outcomes[i]; {
			case valid(o, sy):
				members, idx = append(members, o), append(idx, i)
			case o.Err != nil:
				group, done[i] = false, true
			default:
				done[i] = true // gone (or a zombie) already
			}
		}
	}
	if !group {
		send(sy, leader.Process.PID, p.Signal, leader)
		return
	}
	for _, i := range idx {
		done[i] = true
	}
	send(sy, -p.Group, p.Signal, members...)
	if leader.Signalled {
		r.Group = p.Group
	}
}

// wait polls the signalled outcomes every killPoll until each has exited (gone, a zombie, or
// its pid reused) or deadline has passed.
func wait(outs []Outcome, deadline time.Time, sy osys) {
	for {
		pending := false
		for i := range outs {
			o := &outs[i]
			if !o.Signalled || o.Exited {
				continue
			}
			start, _, err := sy.stat(o.Process.PID)
			if errors.Is(err, collector.ErrGone) || err == nil && !start.Equal(o.Process.StartTime) {
				o.Exited = true
			} else {
				pending = true
			}
		}
		if !pending || !time.Now().Before(deadline) {
			return
		}
		sy.sleep(killPoll)
	}
}

// check re-asserts what NewPlan guarantees, so no bug or hand-made Plan can signal pid 0, a
// negative pid, init, devdash itself or devdash's process group.
func (p Plan) check(sy osys) error {
	if len(p.Procs) == 0 {
		return errors.New("kill: empty plan")
	}
	if p.Signal != syscall.SIGTERM && p.Signal != syscall.SIGKILL {
		return fmt.Errorf("kill: signal %v not allowed", p.Signal)
	}
	chain := sy.chain()
	for _, proc := range p.Procs {
		if proc.PID <= 1 || slices.Contains(chain, proc.PID) {
			return fmt.Errorf("kill: pid %d not allowed", proc.PID)
		}
	}
	if p.Group != 0 {
		if p.Group != p.Procs[0].PID {
			return fmt.Errorf("kill: group %d is not the target's pid %d", p.Group, p.Procs[0].PID)
		}
		for _, pid := range chain {
			if g, err := sy.getpgid(pid); err != nil || g == p.Group {
				return fmt.Errorf("kill: group %d contains pid %d, devdash or one of its ancestors", p.Group, pid)
			}
		}
	}
	return nil
}

// chain is devdash's pid and its ancestors, read from the OS now: the parent, then each
// one's parent, up to pid 1 or the first read error. Unlike the snapshot it cannot miss an
// ancestor that was hidden or not yet collected.
func (sy osys) chain() []int {
	self, pid := sy.self()
	c := []int{self}
	for pid > 1 && !slices.Contains(c, pid) {
		c = append(c, pid)
		_, next, err := sy.stat(pid)
		if err != nil {
			break
		}
		pid = next
	}
	return c
}
