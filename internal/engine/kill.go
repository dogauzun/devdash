package engine

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

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
	// start returns a pid's start time read from the OS now, in the collector's terms, or
	// errGone when the pid does not exist or is a zombie.
	start   func(pid int) (time.Time, error)
	getpgid func(pid int) (int, error)
	kill    func(pid int, sig syscall.Signal) error
	sleep   func(time.Duration)
	self    func() (pid, ppid int) // devdash's pid and its parent's, read now
}

var errGone = errors.New("no such process")

var realOS = osys{start: procStart, getpgid: syscall.Getpgid, kill: syscall.Kill, sleep: time.Sleep,
	self: func() (int, int) { return os.Getpid(), os.Getppid() }}

// NewPlan returns what killing the process with row key key would signal, computed from s, or
// a *Refusal. It sends nothing; it only reads process group ids from the OS.
func NewPlan(s model.Snapshot, key model.RowKey, o KillOptions) (Plan, error) {
	return newPlan(s, key, o, realOS)
}

func newPlan(s model.Snapshot, key model.RowKey, o KillOptions, sy osys) (Plan, error) {
	container := func(id string) string {
		for _, c := range s.Containers {
			if c.ID == id && c.Name != "" {
				return c.Name
			}
		}
		return id
	}
	if key.ContainerID != "" {
		name := container(key.ContainerID)
		return Plan{}, refuse("container %s: use docker stop %s", name, name)
	}
	if key.Header != model.GroupNone {
		return Plan{}, refuse("not a process")
	}

	byPID := map[int]*model.Process{}
	children := map[int][]*model.Process{}
	var target *model.Process
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
	if target == nil {
		if key.PID <= 0 {
			return Plan{}, refuse("pid %d is not a process", key.PID)
		}
		return Plan{}, refuse("pid %d is gone, or its pid was reused", key.PID)
	}

	// devdash and its ancestors: the direct parent comes from the OS, the rest of the chain
	// from the snapshot's ppid map.
	self, parent := sy.self()
	refused := map[int]string{self: "is devdash itself"}
	for pid := parent; pid > 1 && refused[pid] == ""; {
		refused[pid] = "runs devdash"
		p := byPID[pid]
		if p == nil {
			break
		}
		pid = p.PPID
	}

	plan := Plan{Procs: []model.Process{*target}, Signal: syscall.SIGTERM, Outside: target.ProjectID == ""}
	if o.Force {
		plan.Signal = syscall.SIGKILL
	}
	if o.Tree {
		in := map[int]bool{target.PID: true}
		for i := 0; i < len(plan.Procs); i++ { // breadth first: parents before children
			for _, c := range children[plan.Procs[i].PID] {
				if !in[c.PID] {
					in[c.PID] = true
					plan.Procs = append(plan.Procs, *c)
				}
			}
		}
		if pg, err := sy.getpgid(target.PID); err == nil && pg == target.PID && target.PID > 1 {
			for pid, why := range refused {
				if g, err := sy.getpgid(pid); err == nil && g == pg {
					return Plan{}, refuse("process group %d contains pid %d, which %s", pg, pid, why)
				}
			}
			plan.Group = pg
			for i := range s.Processes {
				p := &s.Processes[i]
				if p.PID > 0 && !in[p.PID] {
					if g, err := sy.getpgid(p.PID); err == nil && g == pg {
						in[p.PID] = true
						plan.Procs = append(plan.Procs, *p)
					}
				}
			}
		}
	}

	// One refused process refuses the whole action; nothing is skipped silently.
	for _, p := range plan.Procs {
		switch {
		case p.ContainerID != "":
			name := container(p.ContainerID)
			return Plan{}, refuse("pid %d (%s) holds a port of container %s: use docker stop %s", p.PID, p.Name, name, name)
		case p.PID == 0:
			return Plan{}, refuse("pid 0 is not a process (the owner of this port is unknown)")
		case p.PID == 1:
			return Plan{}, refuse("pid 1 (%s) is init", p.Name)
		case refused[p.PID] != "":
			return Plan{}, refuse("pid %d (%s) %s", p.PID, p.Name, refused[p.PID])
		}
	}
	return plan, nil
}

// Kill signals p, waits up to timeout (DefaultKillTimeout when 0) for the signalled processes
// to exit, polling every 100 ms, and reports. Before every kill(2) the pid's start time is
// read fresh from the OS and compared with the plan, so a reused pid is never signalled. It
// returns an error, having signalled nothing, only for a Plan that NewPlan could not have
// made.
func Kill(p Plan, timeout time.Duration) (Result, error) {
	return kill(p, timeout, realOS)
}

// Kill is the package-level Kill followed by a Refresh, so the next snapshot shows the result
// at once when the loop is running.
func (e *Engine) Kill(p Plan, timeout time.Duration) (Result, error) {
	r, err := Kill(p, timeout)
	e.Refresh()
	return r, err
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

	// alive re-validates o against a start time read from the OS now; when the process is
	// gone, reused or unreadable it records that on o and reports false.
	//
	// ponytail: the read happens microseconds before kill(2); a pid that exits and is reused
	// inside that window would still be signalled. Closing it needs pidfd (Linux only); macOS
	// has no race-free way.
	alive := func(o *Outcome) bool {
		start, err := sy.start(o.Process.PID)
		switch {
		case errors.Is(err, errGone):
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
	send := func(pid int, os ...*Outcome) {
		err := sy.kill(pid, p.Signal)
		for _, o := range os {
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
		if err == nil && pid < 0 {
			r.Group = -pid
		}
	}

	// A target that still leads its group gets one signal to -pgid, which reaches the whole
	// group at the same moment, so no member can respawn another first. Members are found
	// (and validated) just before, so they are reported as signalled even if they die at once.
	done := make([]bool, len(p.Procs))
	if p.Group != 0 {
		done[0] = true
		if leader := &r.Outcomes[0]; alive(leader) {
			if g, err := sy.getpgid(p.Group); err == nil && g == p.Group {
				members := []*Outcome{leader}
				for i := 1; i < len(r.Outcomes); i++ {
					if g, err := sy.getpgid(p.Procs[i].PID); err == nil && g == p.Group {
						done[i] = true
						if alive(&r.Outcomes[i]) {
							members = append(members, &r.Outcomes[i])
						}
					}
				}
				send(-p.Group, members...)
			} else {
				send(leader.Process.PID, leader) // it left its group: signal it alone
			}
		}
	}
	for i := range r.Outcomes {
		if o := &r.Outcomes[i]; !done[i] && alive(o) {
			send(o.Process.PID, o)
		}
	}

	for {
		pending := false
		for i := range r.Outcomes {
			o := &r.Outcomes[i]
			if !o.Signalled || o.Exited {
				continue
			}
			start, err := sy.start(o.Process.PID)
			if errors.Is(err, errGone) || err == nil && !start.Equal(o.Process.StartTime) {
				o.Exited = true
			} else {
				pending = true
			}
		}
		if !pending || !time.Now().Before(deadline) {
			return r, nil
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
	self, parent := sy.self()
	for _, proc := range p.Procs {
		if proc.PID <= 1 || proc.PID == self || proc.PID == parent {
			return fmt.Errorf("kill: pid %d not allowed", proc.PID)
		}
	}
	if p.Group != 0 {
		if p.Group != p.Procs[0].PID {
			return fmt.Errorf("kill: group %d is not the target's pid %d", p.Group, p.Procs[0].PID)
		}
		for _, pid := range []int{self, parent} {
			if g, err := sy.getpgid(pid); err != nil || g == p.Group {
				return fmt.Errorf("kill: group %d contains devdash or its parent", p.Group)
			}
		}
	}
	return nil
}
