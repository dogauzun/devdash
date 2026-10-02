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
	if Classify(server) != KindServer || Classify(proc(11, 0)) != KindOther {
		t.Error("want server with a listener, other without")
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
	rows := Flatten(s, ViewOptions{})
	if len(rows) != 4 {
		t.Fatalf("%d rows, want 4", len(rows))
	}
	keys := map[RowKey]bool{}
	for i, r := range rows {
		if r.Process != &s.Processes[i] || r.Key != s.Processes[i].Key() || r.Depth != 0 || r.Project != nil || r.Dimmed || keys[r.Key] {
			t.Errorf("row %d = %+v", i, r)
		}
		keys[r.Key] = true
	}
}
