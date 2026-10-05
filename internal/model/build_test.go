package model

import (
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

var (
	t0    = time.Unix(1_700_000_000, 0)
	start = time.Unix(1_699_999_000, 0)
	lo    = netip.MustParseAddr("127.0.0.1")
	any4  = netip.IPv4Unspecified()
)

func proc(pid int, cpu time.Duration) Process {
	return Process{PID: pid, PPID: 1, StartTime: start, Name: "p", Argv: []string{"p"}, CPUTime: cpu}
}

func TestFieldSetNames(t *testing.T) {
	tests := []struct {
		s    FieldSet
		want []string
	}{
		{0, []string{}},
		{FieldMem | FieldArgv, []string{"argv", "mem"}},
		{FieldMem | FieldCPU | FieldCwd | FieldArgv | FieldOwner, []string{"owner", "argv", "cwd", "cpu", "mem"}},
	}
	for _, tt := range tests {
		if got := tt.s.Names(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%05b.Names() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestTagSetNames(t *testing.T) {
	tests := []struct {
		s      TagSet
		names  []string
		labels []string
	}{
		{0, []string{}, []string{}},
		{TagCwdDeleted, []string{"cwd_deleted"}, []string{"cwd deleted"}},
		{TagCwdDeleted | TagOrphaned, []string{"orphaned", "cwd_deleted"}, []string{"orphaned", "cwd deleted"}},
	}
	for _, tt := range tests {
		if got := tt.s.Names(); !reflect.DeepEqual(got, tt.names) {
			t.Errorf("%02b.Names() = %q, want %q", tt.s, got, tt.names)
		}
		if got := tt.s.Labels(); !reflect.DeepEqual(got, tt.labels) {
			t.Errorf("%02b.Labels() = %q, want %q", tt.s, got, tt.labels)
		}
	}
}

func TestKindString(t *testing.T) {
	if KindOther.String() != "other" || KindEditor.String() != "editor" || Kind(200).String() != "other" {
		t.Errorf("kind names: %v %v %v", KindOther, KindEditor, Kind(200))
	}
}

func TestBuildListeners(t *testing.T) {
	l := func(port uint16, pid int) RawListener { return RawListener{"tcp4", lo, port, pid} }
	tests := []struct {
		name          string
		listeners     []RawListener
		wantOwned     map[int][]uint16 // pid -> ports
		wantUnknown   []uint16         // one PID 0 row per port, in order
		wantWarnCount int              // listener_owner_unreadable count, 0 for none
	}{
		{"none", nil, map[int][]uint16{}, nil, 0},
		{"owned", []RawListener{l(80, 10), l(81, 11), l(82, 10)}, map[int][]uint16{10: {80, 82}, 11: {81}}, nil, 0},
		{"no owner", []RawListener{l(22, 0), l(631, 0)}, map[int][]uint16{}, []uint16{22, 631}, 2},
		{"owner not in process list", []RawListener{l(5432, 99)}, map[int][]uint16{}, []uint16{5432}, 1},
		{"reuseport duplicates collapse", []RawListener{l(80, 0), l(80, 0), l(80, 98)}, map[int][]uint16{}, []uint16{80}, 1},
		{"owner's reuseport duplicates collapse", []RawListener{l(80, 10), l(80, 10), l(80, 10), l(81, 10)}, map[int][]uint16{10: {80, 81}}, nil, 0},
		{"same port, other address or proto kept", []RawListener{l(80, 10), {"tcp4", any4, 80, 10}, {"tcp6", netip.IPv6Unspecified(), 80, 10}}, map[int][]uint16{10: {80, 80, 80}}, nil, 0},
		{"mixed", []RawListener{l(22, 0), l(80, 10)}, map[int][]uint16{10: {80}}, []uint16{22}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := Raw{TakenAt: t0, Processes: []Process{proc(10, 0), proc(11, 0)}, Listeners: tt.listeners}
			s := Build(raw, Snapshot{}, nil, NewResolver("", nil))

			gotOwned := map[int][]uint16{}
			var gotUnknown []uint16
			keys := map[RowKey]bool{}
			for _, p := range s.Processes {
				if keys[p.Key()] {
					t.Errorf("duplicate row key %+v", p.Key())
				}
				keys[p.Key()] = true
				for _, l := range p.Listeners {
					if p.PID == 0 {
						gotUnknown = append(gotUnknown, l.Port)
					} else {
						gotOwned[p.PID] = append(gotOwned[p.PID], l.Port)
					}
				}
				if p.PID == 0 && (p.Name != "unknown" || p.Unknown != unknownOwner || len(p.Listeners) != 1 || p.Kind != KindOther) {
					t.Errorf("pseudo-process %+v", p)
				}
			}
			if !reflect.DeepEqual(gotOwned, tt.wantOwned) || !reflect.DeepEqual(gotUnknown, tt.wantUnknown) {
				t.Errorf("owned %v unknown %v, want %v %v", gotOwned, gotUnknown, tt.wantOwned, tt.wantUnknown)
			}
			var warn []Warning
			if tt.wantWarnCount > 0 {
				warn = []Warning{{Code: "listener_owner_unreadable", Count: tt.wantWarnCount, Hint: "run with sudo to see owners"}}
			}
			if !reflect.DeepEqual(s.Warnings, warn) {
				t.Errorf("warnings %+v, want %+v", s.Warnings, warn)
			}
		})
	}
}

// TestBuildOwnerHint: a collector that knows why owners are unreadable (root without
// CAP_SYS_PTRACE, DEV-49) replaces the "run with sudo" hint; the code and count stay.
func TestBuildOwnerHint(t *testing.T) {
	raw := Raw{TakenAt: t0, Listeners: []RawListener{{"tcp4", lo, 22, 0}}, OwnerHint: "add a capability"}
	s := Build(raw, Snapshot{}, nil, NewResolver("", nil))
	want := []Warning{{Code: "listener_owner_unreadable", Count: 1, Hint: "add a capability"}}
	if !reflect.DeepEqual(s.Warnings, want) {
		t.Errorf("warnings %+v, want %+v", s.Warnings, want)
	}
}

// TestBuildOwnerSudo: listener_owner_unreadable is marked as fixed by sudo exactly when the
// collector says root would see the owners (Raw.OwnerSudo, DEV-144).
func TestBuildOwnerSudo(t *testing.T) {
	for _, sudo := range []bool{false, true} {
		raw := Raw{TakenAt: t0, Listeners: []RawListener{{"tcp4", lo, 22, 0}}, OwnerSudo: sudo}
		s := Build(raw, Snapshot{}, nil, NewResolver("", nil))
		if len(s.Warnings) != 1 || s.Warnings[0].Sudo != sudo {
			t.Errorf("OwnerSudo %v: warnings %+v", sudo, s.Warnings)
		}
	}
}

// TestBuildOwnerWarningContainers: an unknown owner that Reconcile matches to a container
// (root's docker-proxy seen by a user on Linux) is explained, so it does not count towards
// listener_owner_unreadable; the PID 0 row itself stays (DEV-77).
func TestBuildOwnerWarningContainers(t *testing.T) {
	l := func(port uint16, pid int) RawListener { return RawListener{"tcp4", any4, port, pid} }
	db := Container{ID: "db", Ports: []PortMapping{{HostIP: any4, HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}}
	var five []Container
	for i := range uint16(5) {
		five = append(five, Container{ID: string(rune('a' + i)), Ports: []PortMapping{{HostIP: any4, HostPort: 8000 + i, ContainerPort: 80, Proto: "tcp"}}})
	}
	tests := []struct {
		name       string
		listeners  []RawListener
		containers []Container
		wantCount  int // 0 for no warning
		wantPID0   int // PID 0 rows, matched or not
	}{
		{"matched to a container", []RawListener{l(5432, 0)}, []Container{db}, 0, 1},
		{"owner hidden, matched", []RawListener{l(5432, 99)}, []Container{db}, 0, 1},
		{"not matched: other port", []RawListener{l(5433, 0)}, []Container{db}, 1, 1},
		{"no Docker", []RawListener{l(5432, 0)}, nil, 1, 1},
		{"matched and unmatched", []RawListener{l(5432, 0), l(22, 0)}, []Container{db}, 1, 2},
		{"five published, five others", []RawListener{
			l(8000, 0), l(8001, 0), l(8002, 0), l(8003, 0), l(8004, 0),
			l(22, 0), l(25, 0), l(53, 0), l(631, 0), l(5353, 0),
		}, five, 5, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := Raw{TakenAt: t0, Processes: []Process{proc(10, 0)}, Listeners: tt.listeners}
			s := Build(raw, Snapshot{}, tt.containers, NewResolver("", nil))
			var want []Warning
			if tt.wantCount > 0 {
				want = []Warning{{Code: "listener_owner_unreadable", Count: tt.wantCount, Hint: "run with sudo to see owners"}}
			}
			if !reflect.DeepEqual(s.Warnings, want) {
				t.Errorf("warnings %+v, want %+v", s.Warnings, want)
			}
			n := 0
			for _, p := range s.Processes {
				if p.PID == 0 {
					n++
				}
			}
			if n != tt.wantPID0 {
				t.Errorf("%d PID 0 rows, want %d", n, tt.wantPID0)
			}
		})
	}
}

func TestBuildCPUPercent(t *testing.T) {
	reused := proc(10, 0)
	reused.StartTime = start.Add(time.Minute)
	noCPU := proc(10, 900*time.Millisecond)
	noCPU.Unknown = FieldCPU
	tests := []struct {
		name       string
		prev, cur  []Process
		wall       time.Duration
		wantPct10  float64 // pid 10 in cur; NaN means NaN
		noPrevSnap bool
	}{
		{"first sample", nil, []Process{proc(10, time.Second)}, 0, math.NaN(), true},
		{"two samples", []Process{proc(10, time.Second)}, []Process{proc(10, 1500*time.Millisecond)}, 2 * time.Second, 25, false},
		{"multi-core", []Process{proc(10, 0)}, []Process{proc(10, 3*time.Second)}, 2 * time.Second, 150, false},
		{"pid reused, new start time", []Process{proc(10, time.Second)}, []Process{reused}, 2 * time.Second, math.NaN(), false},
		{"new pid", []Process{proc(11, time.Second)}, []Process{proc(10, 2*time.Second)}, 2 * time.Second, math.NaN(), false},
		{"cpu unknown now", []Process{proc(10, 0)}, []Process{noCPU}, 2 * time.Second, math.NaN(), false},
		{"cpu unknown before", []Process{noCPU}, []Process{proc(10, time.Second)}, 2 * time.Second, math.NaN(), false},
		{"clock went back", []Process{proc(10, 0)}, []Process{proc(10, time.Second)}, -time.Second, math.NaN(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prev Snapshot
			if !tt.noPrevSnap {
				prev = Build(Raw{TakenAt: t0, Processes: tt.prev}, Snapshot{}, nil, NewResolver("", nil))
			}
			s := Build(Raw{TakenAt: t0.Add(tt.wall), Processes: tt.cur}, prev, nil, NewResolver("", nil))
			got := s.Processes[0].CPUPercent
			if math.IsNaN(tt.wantPct10) != math.IsNaN(got) || (!math.IsNaN(got) && math.Abs(got-tt.wantPct10) > 1e-9) {
				t.Errorf("cpu%% = %v, want %v", got, tt.wantPct10)
			}
		})
	}
}

func TestBuildCopiesAndFills(t *testing.T) {
	stale := proc(10, 0)
	stale.Unknown = FieldCwd | FieldMem
	stale.Listeners, stale.Kind, stale.ProjectID, stale.ContainerID = []Listener{{Proto: "tcp4", Addr: lo, Port: 1}}, KindShell, "/x", "c" // derived fields are recomputed
	stale.Tags = TagOrphaned | TagCwdDeleted
	stale.CwdDeleted = true // collected, so it passes through
	raw := Raw{
		TakenAt:   t0,
		Host:      Host{OS: "linux", Arch: "arm64", Hostname: "h", UID: 1000},
		Processes: []Process{stale},
		Listeners: []RawListener{{"tcp6", netip.IPv6Unspecified(), 3000, 10}},
		Warnings: []Warning{
			{Code: "process_fields_unreadable", Count: 3, Hint: "run with sudo"},
			{Code: "proc_hidepid", Count: 1, Hint: "hidepid=invisible"},
			{Code: "process_fields_unreadable", Count: 2, Hint: "ignored: first hint wins"},
		},
		Timings: Timing{"proctable": time.Millisecond},
	}
	containers := []Container{{ID: "abc", Name: "db"}}
	s := Build(raw, Snapshot{}, containers, NewResolver("", nil))

	want := Snapshot{
		SchemaVersion: 1,
		TakenAt:       t0,
		Host:          raw.Host,
		Containers:    containers,
		Warnings:      []Warning{{Code: "process_fields_unreadable", Count: 5, Hint: "run with sudo"}, {Code: "proc_hidepid", Count: 1, Hint: "hidepid=invisible"}},
	}
	wantProc := proc(10, 0)
	wantProc.Unknown = FieldCwd | FieldMem // collector bits pass through, no owner bit for a real process
	wantProc.Listeners = []Listener{{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 3000}}
	wantProc.Kind = KindServer
	wantProc.CwdDeleted = true
	wantProc.Tags = TagOrphaned | TagCwdDeleted // ppid 1 and a deleted cwd, recomputed from the collected fields
	if len(s.Processes) != 1 || len(s.Timing) != 2 || s.Timing["proctable"] != time.Millisecond {
		t.Fatalf("processes %+v timing %v", s.Processes, s.Timing)
	}
	got := s.Processes[0]
	got.CPUPercent = 0 // NaN never compares equal; covered by TestBuildCPUPercent
	if !reflect.DeepEqual(got, wantProc) {
		t.Errorf("process\n got %+v\nwant %+v", got, wantProc)
	}
	s.Processes, s.Timing = nil, nil
	if !reflect.DeepEqual(s, want) {
		t.Errorf("snapshot\n got %+v\nwant %+v", s, want)
	}
	if raw.Processes[0].Kind != KindShell || len(raw.Timings) != 1 {
		t.Error("Build mutated raw")
	}
}

func TestBuildName(t *testing.T) {
	tests := []struct {
		name, comm string
		argv       []string
		unknown    FieldSet
		want       string
	}{
		{"macOS cut p_comm", "com.docker.backe", []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "-x"}, 0, "com.docker.backend"},
		{"Linux cut comm", "my-very-long-se", []string{"/tmp/my-very-long-service-name", "3000"}, 0, "my-very-long-service-name"},
		{"cut, argv[0] with spaces", "Google Chrome He", []string{"/Applications/Google Chrome.app/x/Google Chrome Helper (Renderer)"}, 0, "Google Chrome Helper (Renderer)"},
		{"cut, bare argv[0]", "SetStoreUpdateSe", []string{"SetStoreUpdateService"}, 0, "SetStoreUpdateService"},
		{"cut, trailing slash", "my-very-long-se", []string{"/tmp/my-very-long-service-name/"}, 0, "my-very-long-service-name"},
		{"cut login shell drops dash", "my-very-long-sh", []string{"-my-very-long-shell"}, 0, "my-very-long-shell"},
		{"short name kept, login shell", "zsh", []string{"-zsh"}, 0, "zsh"},
		{"short name kept, title rewrite", "nginx", []string{"nginx: master process /usr/sbin/nginx -g daemon off;"}, 0, "nginx"},
		{"short name kept, title rewrite, prefix", "postgres", []string{"postgres: checkpointer"}, 0, "postgres"},
		{"short name kept, symlink argv[0]", "python3.12", []string{"python3"}, 0, "python3.12"},
		{"cut, argv[0] is something else", "my-very-long-se", []string{"/usr/bin/other"}, 0, "my-very-long-se"},
		{"argv unknown", "com.docker.backe", []string{"/x/com.docker.backend"}, FieldArgv, "com.docker.backe"},
		{"argv empty", "my-very-long-se", nil, 0, "my-very-long-se"},
		{"argv[0] empty", "my-very-long-se", []string{"", "my-very-long-service-name"}, 0, "my-very-long-se"},
		{"exactly 15, not cut", "fifteen-chars-x", []string{"/bin/fifteen-chars-x"}, 0, "fifteen-chars-x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := proc(10, 0)
			p.Name, p.Argv, p.Unknown = tt.comm, tt.argv, tt.unknown
			raw := Raw{TakenAt: t0, Processes: []Process{p}}
			if got := Build(raw, Snapshot{}, nil, NewResolver("", nil)).Processes[0].Name; got != tt.want {
				t.Errorf("Name = %q, want %q", got, tt.want)
			}
			if raw.Processes[0].Name != tt.comm {
				t.Error("Build mutated raw")
			}
		})
	}
}

// A container-runtime process inherits the daemon's cwd (dockerd started by hand from a
// checkout, a systemd --user unit with WorkingDirectory in a repository), which says nothing
// about where it belongs: it gets no project, whether or not a container claims its port, and
// the repository it sits in is not a project of the snapshot unless another process is in it.
func TestBuildRuntimeNoProject(t *testing.T) {
	base := tmp(t)
	repo := mkrepo(t, base, "devdash", "main")
	sub := mkdir(t, repo, "cmd")
	rt := func(pid, ppid int, name string, argv ...string) Process {
		return Process{PID: pid, PPID: ppid, StartTime: start, Name: name, Argv: append([]string{"/usr/bin/" + name}, argv...), Cwd: sub}
	}
	raw := Raw{
		TakenAt: t0,
		Processes: []Process{
			rt(2900, 1, "dockerd"),
			rt(2910, 2900, "containerd"),
			rt(2920, 1, "containerd-shim-runc-v2", "-namespace", "moby"),
			rt(2943, 2900, "docker-proxy", "-proto", "tcp", "-host-ip", "0.0.0.0", "-host-port", "18081", filepath.Join(repo, "x")),
			// Its cwd is unreadable: the parent chain stops at the runtime ancestor.
			{PID: 2950, PPID: 2920, StartTime: start, Name: "nginx", Argv: []string{"nginx"}, Unknown: FieldCwd},
		},
		Listeners: []RawListener{{"tcp4", any4, 18081, 2943}},
	}
	web := Container{ID: "c0ffee", Name: "shop-web-1", ComposeProject: "shop",
		Ports: []PortMapping{{HostIP: any4, HostPort: 18081, ContainerPort: 80, Proto: "tcp"}}}

	for _, tt := range []struct {
		name          string
		containers    []Container
		wantContainer string
	}{
		{"docker unreachable", nil, ""},
		{"reconciled", []Container{web}, web.ID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := Build(raw, Snapshot{}, tt.containers, NewResolver("", nil))
			for _, p := range s.Processes {
				if p.ProjectID != "" {
					t.Errorf("%s (pid %d): ProjectID %q, want none", p.Name, p.PID, p.ProjectID)
				}
				if p.PID == 2943 && p.ContainerID != tt.wantContainer {
					t.Errorf("docker-proxy ContainerID %q, want %q", p.ContainerID, tt.wantContainer)
				}
			}
			if len(s.Projects) != 0 {
				t.Errorf("projects %+v, want none", s.Projects)
			}

			// A process of the user's own in the repository still resolves, and is the
			// project's only member.
			own := raw
			own.Processes = append(slices.Clone(raw.Processes), Process{PID: 3000, PPID: 1, StartTime: start, Name: "go", Argv: []string{"go"}, Cwd: repo})
			s = Build(own, Snapshot{}, tt.containers, NewResolver("", nil))
			for _, p := range s.Processes {
				if want := map[bool]string{true: repo}[p.PID == 3000]; p.ProjectID != want {
					t.Errorf("with own process: %s (pid %d): ProjectID %q, want %q", p.Name, p.PID, p.ProjectID, want)
				}
			}
			if want := []Project{project(repo, "main")}; !slices.Equal(s.Projects, want) {
				t.Errorf("with own process: projects %+v, want %+v", s.Projects, want)
			}
		})
	}
}
