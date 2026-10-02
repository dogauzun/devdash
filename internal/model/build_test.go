package model

import (
	"math"
	"net/netip"
	"reflect"
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
				if p.PID == 0 && (p.Name != "unknown" || p.Unknown != unknownOwner || len(p.Listeners) != 1 || p.Kind != KindServer) {
					t.Errorf("pseudo-process %+v", p)
				}
			}
			if !reflect.DeepEqual(gotOwned, tt.wantOwned) || !reflect.DeepEqual(gotUnknown, tt.wantUnknown) {
				t.Errorf("owned %v unknown %v, want %v %v", gotOwned, gotUnknown, tt.wantOwned, tt.wantUnknown)
			}
			var warn []Warning
			if tt.wantWarnCount > 0 {
				warn = []Warning{{"listener_owner_unreadable", tt.wantWarnCount, "run with sudo to see owners"}}
			}
			if !reflect.DeepEqual(s.Warnings, warn) {
				t.Errorf("warnings %+v, want %+v", s.Warnings, warn)
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
	stale.Listeners, stale.Kind, stale.ProjectID, stale.ContainerID = []Listener{{"tcp4", lo, 1}}, KindShell, "/x", "c" // derived fields are recomputed
	raw := Raw{
		TakenAt:   t0,
		Host:      Host{OS: "linux", Arch: "arm64", Hostname: "h", UID: 1000},
		Processes: []Process{stale},
		Listeners: []RawListener{{"tcp6", netip.IPv6Unspecified(), 3000, 10}},
		Warnings: []Warning{
			{"process_fields_unreadable", 3, "run with sudo"},
			{"proc_hidepid", 1, "hidepid=invisible"},
			{"process_fields_unreadable", 2, "ignored: first hint wins"},
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
		Warnings:      []Warning{{"process_fields_unreadable", 5, "run with sudo"}, {"proc_hidepid", 1, "hidepid=invisible"}},
	}
	wantProc := proc(10, 0)
	wantProc.Unknown = FieldCwd | FieldMem // collector bits pass through, no owner bit for a real process
	wantProc.Listeners = []Listener{{"tcp6", netip.IPv6Unspecified(), 3000}}
	wantProc.Kind = KindServer
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
