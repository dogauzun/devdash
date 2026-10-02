package model

import "testing"

// These pin the placeholder contracts; the owning tickets replace them.

func TestResolvePlaceholder(t *testing.T) {
	procs := []Process{proc(10, 0), {Name: "unknown"}}
	if got := NewResolver("/home/me", nil).Resolve(procs); got != nil || procs[0].ProjectID != "" || procs[1].ProjectID != "" {
		t.Errorf("projects %v, ids %q %q", got, procs[0].ProjectID, procs[1].ProjectID)
	}
}

func TestClassifyPlaceholder(t *testing.T) {
	server := proc(10, 0)
	server.Listeners = []Listener{{"tcp4", lo, 80}}
	unknown := Process{Name: "unknown", Listeners: server.Listeners, Unknown: unknownOwner}
	if Classify(server) != KindServer || Classify(proc(11, 0)) != KindOther || Classify(unknown) != KindOther {
		t.Error("want server with a listener, other without, other for the PID 0 pseudo-process")
	}
}

func TestReconcilePlaceholder(t *testing.T) {
	procs := []Process{proc(10, 0)}
	got := Reconcile(procs, []Container{{ID: "abc"}})
	if len(got) != 1 || &got[0] != &procs[0] || got[0].ContainerID != "" {
		t.Errorf("got %+v", got)
	}
}

func TestFlattenPlaceholder(t *testing.T) {
	s := Build(Raw{
		Processes: []Process{proc(10, 0), proc(11, 0)},
		Listeners: []RawListener{{"tcp4", any4, 22, 0}, {"tcp4", any4, 631, 0}},
	}, Snapshot{}, nil, NewResolver("", nil))
	rows := Flatten(s, ViewOptions{ShowAll: true, HideContainers: true, Sort: SortCPU})
	if len(rows) != 4 {
		t.Fatalf("%d rows, want 4", len(rows))
	}
	keys := map[RowKey]bool{}
	for i, r := range rows {
		if r.Process != &s.Processes[i] || r.Key != s.Processes[i].Key() || r.Depth != 0 || r.Project != nil || r.Container != nil || r.Dimmed || keys[r.Key] {
			t.Errorf("row %d = %+v", i, r)
		}
		keys[r.Key] = true
	}
}

// TestRowKeysDistinct: every kind of row has a non-zero key, distinct from the others even when
// a project, a compose project and a container share a name.
func TestRowKeysDistinct(t *testing.T) {
	keys := []RowKey{
		proc(10, 0).Key(),
		{PID: 0, Listener: Listener{"tcp4", any4, 22}},
		{PID: 0, Listener: Listener{"tcp4", any4, 631}},
		{ContainerID: "shop"},
		{Header: GroupProject, Group: "shop"},
		{Header: GroupCompose, Group: "shop"},
		{Header: GroupContainers},
		{Header: GroupOther},
	}
	seen := map[RowKey]bool{{}: true}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("key %+v is zero or repeated", k)
		}
		seen[k] = true
	}
}
