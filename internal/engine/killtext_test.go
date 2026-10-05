package engine

import "testing"

func TestKillOptionsMode(t *testing.T) {
	for _, tt := range []struct {
		o    KillOptions
		want string
	}{
		{KillOptions{}, "process mode"},
		{KillOptions{Force: true}, "process mode, force"},
		{KillOptions{Tree: true}, "tree mode"},
		{KillOptions{Tree: true, Force: true}, "tree mode, force"},
	} {
		if got := tt.o.Mode(); got != tt.want {
			t.Errorf("%+v: Mode() = %q, want %q", tt.o, got, tt.want)
		}
	}
}
