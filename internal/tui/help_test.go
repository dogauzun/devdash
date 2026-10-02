package tui

import (
	"strings"
	"testing"
)

func TestHelp(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	feed(m, fixture()) // with a warning line, the body is 21 lines
	press(m, "?")
	// Every key of the spec's key table, in full at 80 columns.
	for _, want := range []string{
		"↑ ↓ j k    move the selection",
		"← → h l    collapse or expand a project group or a tree node",
		"enter      open or close the detail pane (esc closes it too)",
		"/          filter by port, name, argv, project or container; esc clears",
		"x          kill modal: p process, t tree, f force, esc cancel",
		"o          open http://localhost:<port> (the lowest port)",
		"a          show or hide shells and editors",
		"d          show or hide container rows",
		"s          cycle sort within groups: default, port, cpu, start time",
		"r          refresh now",
		"?          this help",
		"q ctrl-c   quit",
	} {
		hasLine(t, m, want)
	}
	if !strings.HasPrefix(bodyLines(m)[0], "Keys") {
		t.Errorf("help has no title: %q", bodyLines(m)[0])
	}
	if line(m, "mbp ·") == "" || line(m, "↑↓ move") == "" {
		t.Error("help hides the header or the footer")
	}
	press(m, "x")
	if m.help || m.kill.active() {
		t.Error("the key that closes help also acted")
	}
}
