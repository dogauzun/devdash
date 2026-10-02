package engine

import (
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// Fake pids sit above Linux's maximum pid (2^22), so they can never be a real process; the
// fake OS records signals and sends none.
const (
	T    = 5_000_010 // target
	C1   = 5_000_011 // child of T
	C2   = 5_000_012 // child of T
	G1   = 5_000_013 // child of C1
	X    = 5_000_020 // unrelated, sometimes in T's process group
	GP   = 5_000_090 // grandparent of devdash in the fake snapshot
	DD   = 5_000_100 // devdash itself, to the fake OS
	DDPP = 5_000_101 // its parent
)

func proc(pid, ppid int) model.Process {
	return model.Process{PID: pid, PPID: ppid, Name: "p", StartTime: time.Unix(int64(1000+pid%1000), 0), ProjectID: "/p"}
}

func snap(ps ...model.Process) model.Snapshot { return model.Snapshot{Processes: ps} }

// family is T with children C1 and C2, C1 with child G1, and an unrelated X.
func family() []model.Process {
	return []model.Process{proc(T, 7), proc(C1, T), proc(X, 7), proc(C2, T), proc(G1, C1)}
}

type sent struct {
	pid int
	sig syscall.Signal
}

// fakeOS scripts the OS seam: starts holds live pids, pgids their groups (anything else is in
// group 99), and kill records what it would send.
type fakeOS struct {
	starts  map[int]time.Time
	errs    map[int]error // start-time read errors
	pgids   map[int]int
	killErr map[int]error
	dies    map[int]bool // removed from starts when signalled
	sent    []sent
}

func newFake(ps ...model.Process) *fakeOS {
	f := &fakeOS{starts: map[int]time.Time{}, errs: map[int]error{}, pgids: map[int]int{}, killErr: map[int]error{}, dies: map[int]bool{}}
	for _, p := range ps {
		f.starts[p.PID] = p.StartTime
		f.dies[p.PID] = true
	}
	return f
}

func (f *fakeOS) sys() osys {
	return osys{
		start: func(pid int) (time.Time, error) {
			if err := f.errs[pid]; err != nil {
				return time.Time{}, err
			}
			t, ok := f.starts[pid]
			if !ok {
				return time.Time{}, errGone
			}
			return t, nil
		},
		getpgid: func(pid int) (int, error) {
			if g, ok := f.pgids[pid]; ok {
				return g, nil
			}
			return 99, nil
		},
		kill: func(pid int, sig syscall.Signal) error {
			f.sent = append(f.sent, sent{pid, sig})
			if err := f.killErr[pid]; err != nil {
				return err
			}
			for p := range f.starts {
				if (p == pid || pid < 0 && f.pgids[p] == -pid) && f.dies[p] {
					delete(f.starts, p)
				}
			}
			return nil
		},
		sleep: time.Sleep,
		self:  func() (int, int) { return DD, DDPP },
	}
}

func pids(ps []model.Process) []int {
	var out []int
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

func TestNewPlan(t *testing.T) {
	self, parent := DD, DDPP
	devdash := []model.Process{proc(self, parent), proc(parent, GP), proc(GP, 1)}
	unknown := model.Process{Name: "unknown", Listeners: []model.Listener{{Proto: "tcp4", Port: 631}}}
	proxy := proc(C2, T)
	proxy.ContainerID = "c0ffee"
	unknownCtr := unknown
	unknownCtr.ContainerID = "c0ffee"
	outside := proc(T, 7)
	outside.ProjectID = ""
	moved := proc(T, 7)
	moved.StartTime = moved.StartTime.Add(time.Second)

	tests := []struct {
		name      string
		procs     []model.Process
		key       model.RowKey
		opt       KillOptions
		pgids     map[int]int
		want      []int
		group     int
		sig       syscall.Signal
		outside   bool
		refusedAs string // substring of the refusal; "" means no refusal
	}{
		{name: "process", procs: family(), key: proc(T, 7).Key(), want: []int{T}, sig: syscall.SIGTERM},
		{name: "process force", procs: family(), key: proc(T, 7).Key(), opt: KillOptions{Force: true}, want: []int{T}, sig: syscall.SIGKILL},
		{name: "tree parent first", procs: family(), key: proc(T, 7).Key(), opt: KillOptions{Tree: true}, want: []int{T, C1, C2, G1}, sig: syscall.SIGTERM},
		{name: "tree force", procs: family(), key: proc(T, 7).Key(), opt: KillOptions{Tree: true, Force: true}, want: []int{T, C1, C2, G1}, sig: syscall.SIGKILL},
		{name: "subtree", procs: family(), key: proc(C1, T).Key(), opt: KillOptions{Tree: true}, want: []int{C1, G1}, sig: syscall.SIGTERM},
		{name: "group leader adds group members", procs: family(), key: proc(T, 7).Key(), opt: KillOptions{Tree: true},
			pgids: map[int]int{T: T, C1: T, X: T}, want: []int{T, C1, C2, G1, X}, group: T, sig: syscall.SIGTERM},
		{name: "group member that is not the leader", procs: family(), key: proc(C1, T).Key(), opt: KillOptions{Tree: true},
			pgids: map[int]int{T: T, C1: T, X: T}, want: []int{C1, G1}, sig: syscall.SIGTERM},
		{name: "group ignored in process mode", procs: family(), key: proc(T, 7).Key(),
			pgids: map[int]int{T: T, X: T}, want: []int{T}, sig: syscall.SIGTERM},
		{name: "outside any project", procs: []model.Process{outside}, key: outside.Key(), want: []int{T}, sig: syscall.SIGTERM, outside: true},

		{name: "pid 0 unknown owner", procs: []model.Process{unknown}, key: unknown.Key(), refusedAs: "pid 0"},
		{name: "pid 1", procs: []model.Process{proc(1, 0)}, key: proc(1, 0).Key(), refusedAs: "pid 1"},
		{name: "devdash itself", procs: devdash, key: devdash[0].Key(), refusedAs: "is devdash itself"},
		{name: "parent of devdash", procs: devdash, key: devdash[1].Key(), refusedAs: "runs devdash"},
		{name: "grandparent of devdash", procs: devdash, key: devdash[2].Key(), refusedAs: "runs devdash"},
		{name: "tree containing devdash", procs: append(slices.Clone(devdash), proc(T, 1), proc(C1, T)),
			key: devdash[2].Key(), opt: KillOptions{Tree: true}, refusedAs: "runs devdash"},
		{name: "group containing devdash", procs: family(), key: proc(T, 7).Key(), opt: KillOptions{Tree: true},
			pgids: map[int]int{T: T, self: T}, refusedAs: "process group"},
		{name: "group containing an ancestor", procs: append(family(), devdash...), key: proc(T, 7).Key(), opt: KillOptions{Tree: true},
			pgids: map[int]int{T: T, GP: T}, refusedAs: "process group"},
		{name: "tree containing pid 1 via group", procs: append(family(), proc(1, 0)), key: proc(T, 7).Key(), opt: KillOptions{Tree: true},
			pgids: map[int]int{T: T, 1: T}, refusedAs: "pid 1"},
		{name: "container port", procs: []model.Process{proxy}, key: proxy.Key(), refusedAs: "docker stop shop-db-1"},
		{name: "unknown owner of a container port", procs: []model.Process{unknownCtr}, key: unknownCtr.Key(), refusedAs: "docker stop shop-db-1"},
		{name: "tree containing a container port", procs: []model.Process{proc(T, 7), proxy}, key: proc(T, 7).Key(), opt: KillOptions{Tree: true}, refusedAs: "docker stop shop-db-1"},
		{name: "container row", key: model.RowKey{ContainerID: "c0ffee"}, refusedAs: "docker stop shop-db-1"},
		{name: "header row", procs: family(), key: model.RowKey{Header: model.GroupOther}, refusedAs: "not a process"},
		{name: "start time changed", procs: []model.Process{moved}, key: proc(T, 7).Key(), refusedAs: "reused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(tt.procs...)
			for k, v := range tt.pgids {
				f.pgids[k] = v
			}
			s := snap(tt.procs...)
			s.Containers = []model.Container{{ID: "c0ffee", Name: "shop-db-1"}}
			p, err := newPlan(s, tt.key, tt.opt, f.sys())
			if tt.refusedAs != "" {
				var r *Refusal
				if !errors.As(err, &r) || !strings.Contains(r.Reason, tt.refusedAs) {
					t.Fatalf("err = %v, want refusal containing %q", err, tt.refusedAs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := pids(p.Procs); !slices.Equal(got, tt.want) {
				t.Errorf("procs = %v, want %v", got, tt.want)
			}
			if p.Group != tt.group || p.Signal != tt.sig || p.Outside != tt.outside {
				t.Errorf("group, signal, outside = %d, %v, %v; want %d, %v, %v", p.Group, p.Signal, p.Outside, tt.group, tt.sig, tt.outside)
			}
			if len(f.sent) != 0 {
				t.Errorf("planning sent %v", f.sent)
			}
		})
	}
}

func plan(t *testing.T, f *fakeOS, key model.RowKey, o KillOptions, ps ...model.Process) Plan {
	t.Helper()
	p, err := newPlan(snap(ps...), key, o, f.sys())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKill(t *testing.T) {
	term := syscall.SIGTERM
	tests := []struct {
		name     string
		opt      KillOptions
		pgids    map[int]int // at plan time; setup may change them before the kill
		setup    func(f *fakeOS)
		wantSent []sent
		code     int
		check    func(t *testing.T, r Result)
	}{
		{name: "process", wantSent: []sent{{T, term}}},
		{name: "force", opt: KillOptions{Force: true}, wantSent: []sent{{T, syscall.SIGKILL}}},
		{name: "tree parent first", opt: KillOptions{Tree: true}, wantSent: []sent{{T, term}, {C1, term}, {C2, term}, {G1, term}}},
		{name: "group leader: one group signal, then the rest", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, C2: T, X: T},
			wantSent: []sent{{-T, term}, {G1, term}},
			check: func(t *testing.T, r Result) {
				for _, o := range r.Outcomes {
					if !o.Signalled || !o.Exited {
						t.Errorf("pid %d: signalled %v, exited %v", o.Process.PID, o.Signalled, o.Exited)
					}
				}
				if r.Group != T {
					t.Errorf("group %d", r.Group)
				}
			}},
		{name: "group leader gone: no group signal", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, X: T},
			setup:    func(f *fakeOS) { delete(f.starts, T) },
			wantSent: []sent{{C1, term}, {C2, term}, {G1, term}, {X, term}}},
		{name: "group leader left its group", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, X: T},
			setup:    func(f *fakeOS) { f.pgids[T] = 99 },
			wantSent: []sent{{T, term}, {C1, term}, {C2, term}, {G1, term}, {X, term}}},
		{name: "member left the group", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, C2: T, X: T},
			setup:    func(f *fakeOS) { f.pgids[C2] = 99 },
			wantSent: []sent{{-T, term}, {C2, term}, {G1, term}}},
		{name: "group member reused: not counted as signalled", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, X: T},
			setup:    func(f *fakeOS) { f.starts[X] = f.starts[X].Add(time.Second) },
			wantSent: []sent{{-T, term}, {C2, term}, {G1, term}}, code: 4,
			check: func(t *testing.T, r Result) {
				if o := r.Outcomes[4]; o.Signalled || !errors.Is(o.Err, ErrStartTime) {
					t.Errorf("X: signalled %v, err %v", o.Signalled, o.Err)
				}
			}},
		{name: "group EPERM", opt: KillOptions{Tree: true},
			pgids:    map[int]int{T: T, C1: T, C2: T, X: T},
			setup:    func(f *fakeOS) { f.killErr[-T] = syscall.EPERM; f.killErr[G1] = syscall.EPERM },
			wantSent: []sent{{-T, term}, {G1, term}}, code: 3},
		{name: "pid reused: never signalled",
			setup: func(f *fakeOS) { f.starts[T] = f.starts[T].Add(time.Second) },
			code:  4,
			check: func(t *testing.T, r Result) {
				if !errors.Is(r.Outcomes[0].Err, ErrStartTime) {
					t.Errorf("err = %v", r.Outcomes[0].Err)
				}
			}},
		{name: "start time unreadable: never signalled",
			setup: func(f *fakeOS) { f.errs[T] = syscall.EACCES }, code: 4},
		{name: "already gone before the signal",
			setup: func(f *fakeOS) { delete(f.starts, T) }},
		{name: "ESRCH counts as exited",
			setup: func(f *fakeOS) { f.killErr[T] = syscall.ESRCH }, wantSent: []sent{{T, term}}},
		{name: "EPERM", setup: func(f *fakeOS) { f.killErr[T] = syscall.EPERM }, wantSent: []sent{{T, term}}, code: 3,
			check: func(t *testing.T, r Result) {
				if r.Outcomes[0].Err.Error() != "permission denied, run with sudo" {
					t.Errorf("err = %v", r.Outcomes[0].Err)
				}
			}},
		{name: "EPERM wins over survivors", opt: KillOptions{Tree: true},
			setup:    func(f *fakeOS) { f.killErr[C1] = syscall.EPERM; f.dies[C2] = false },
			wantSent: []sent{{T, term}, {C1, term}, {C2, term}, {G1, term}}, code: 3},
		{name: "survivor", opt: KillOptions{Tree: true},
			setup:    func(f *fakeOS) { f.dies[C2] = false },
			wantSent: []sent{{T, term}, {C1, term}, {C2, term}, {G1, term}}, code: 4,
			check: func(t *testing.T, r Result) {
				if got := pids(r.Survivors()); !slices.Equal(got, []int{C2}) {
					t.Errorf("survivors = %v", got)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fam := family()
				f := newFake(fam...)
				for k, v := range tt.pgids {
					f.pgids[k] = v
				}
				p := plan(t, f, fam[0].Key(), tt.opt, fam...)
				if tt.setup != nil {
					tt.setup(f) // the OS changes between plan and kill
				}
				start := time.Now()
				r, err := kill(p, 0, f.sys())
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(f.sent, tt.wantSent) {
					t.Errorf("sent %v, want %v", f.sent, tt.wantSent)
				}
				if got := r.ExitCode(); got != tt.code {
					t.Errorf("exit code %d, want %d", got, tt.code)
				}
				wait := time.Since(start)
				if tt.code == 4 && len(r.Survivors()) > 0 && wait != DefaultKillTimeout {
					t.Errorf("waited %v for survivors, want %v", wait, DefaultKillTimeout)
				}
				if len(r.Survivors()) == 0 && wait != 0 {
					t.Errorf("waited %v with no survivors", wait)
				}
				if tt.check != nil {
					tt.check(t, r)
				}
			})
		})
	}
}

func TestKillPollsUntilExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake(proc(T, 7))
		f.dies[T] = false
		p := plan(t, f, proc(T, 7).Key(), KillOptions{}, proc(T, 7))
		sy := f.sys()
		polls := 0
		start := sy.start
		sy.start = func(pid int) (time.Time, error) {
			if polls++; polls == 4 { // validation, then three polls 100 ms apart
				delete(f.starts, pid)
			}
			return start(pid)
		}
		t0 := time.Now()
		r, err := kill(p, time.Second, sy)
		if err != nil || r.ExitCode() != 0 {
			t.Fatalf("exit %d, err %v", r.ExitCode(), err)
		}
		if got := time.Since(t0); got != 200*time.Millisecond {
			t.Errorf("exited after %v, want 200ms", got)
		}
	})
}

func TestKillRejectsBadPlans(t *testing.T) {
	self, parent := DD, DDPP
	term := syscall.SIGTERM
	tests := []struct {
		name string
		plan Plan
	}{
		{"empty", Plan{Signal: term}},
		{"pid 0", Plan{Procs: []model.Process{proc(0, 0)}, Signal: term}},
		{"pid 1", Plan{Procs: []model.Process{proc(1, 0)}, Signal: term}},
		{"negative pid", Plan{Procs: []model.Process{proc(-T, 0)}, Signal: term}},
		{"devdash", Plan{Procs: []model.Process{proc(T, 7), proc(self, T)}, Signal: term}},
		{"devdash's parent", Plan{Procs: []model.Process{proc(parent, 7)}, Signal: term}},
		{"group is not the target", Plan{Procs: []model.Process{proc(T, 7), proc(C1, T)}, Group: C1, Signal: term}},
		{"group contains devdash", Plan{Procs: []model.Process{proc(T, 7)}, Group: T, Signal: term}},
		{"other signal", Plan{Procs: []model.Process{proc(T, 7)}, Signal: syscall.SIGHUP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(tt.plan.Procs...)
			f.pgids = map[int]int{T: T, self: T}
			if _, err := kill(tt.plan, time.Millisecond, f.sys()); err == nil || len(f.sent) != 0 {
				t.Errorf("err %v, sent %v", err, f.sent)
			}
		})
	}
}
