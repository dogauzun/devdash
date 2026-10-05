package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/model"
)

func TestHelp(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	feed(m, fixture()) // with a warning line, the body is 21 lines: the overlay fits
	press(m, "?")
	// Every key of the spec's key table, and the keys beyond it, in full at 80 columns.
	for _, want := range []string{
		"↑ ↓ j k     move the selection",
		"pgup pgdown page the table or the open detail pane; g/home first, G/end last row",
		"← → h l     collapse or expand a group or node; → unfolds a chain, ← refolds",
		"enter       open or close the detail pane (esc closes it too)",
		"/           filter by port, name, argv, project, container or tag; esc clears",
		"0-9         port search: opens the filter with the digit, selects the holder",
		"x           kill modal: p process, t tree, f force, Y second confirm, esc cancel",
		"o           open http://localhost:<port> (the lowest port)",
		"a           show or hide shells and editors",
		"d           show or hide container rows",
		"s           cycle sort within groups: default, port, cpu, start time, name",
		"r           refresh now",
		"?           this help",
		"q ctrl-c    quit",
		"Warnings",
		"run with sudo to see owners",
	} {
		hasLine(t, m, want)
	}
	for _, k := range helpKeys {
		if n := helpKeyWidth + ansi.StringWidth(k[1]); n > 80 {
			t.Errorf("help line for %q is %d cells", k[0], n)
		}
	}
	if got := bodyLines(m)[0]; got != "Keys  (any key closes)" {
		t.Errorf("help title %q", got)
	}
	if strings.Contains(screen(m), "to scroll") {
		t.Errorf("help that fits has a position line:\n%s", screen(m))
	}
	if line(m, "mbp ·") == "" || line(m, "↑↓ move") == "" {
		t.Error("help hides the header or the footer")
	}
	press(m, "down") // it fits whole: no key scrolls, so this one closes
	if m.help {
		t.Error("down did not close help that fits")
	}
	press(m, "?", "x")
	if m.help || m.kill.active() {
		t.Error("the key that closes help also acted")
	}
}

// helpSeen opens help and returns every distinct body line seen while scrolling down to the
// end, in order.
func helpSeen(t *testing.T, m *Model) []string {
	t.Helper()
	press(m, "?")
	var seen []string
	for range 50 {
		for _, l := range bodyLines(m) {
			if !slices.Contains(seen, l) {
				seen = append(seen, l)
			}
		}
		top := m.hpos.top
		press(m, "down")
		if !m.help {
			t.Fatal("down closed help")
		}
		if m.hpos.top == top {
			return seen
		}
	}
	t.Fatal("help never reached its end")
	return nil
}

func TestHelpWarnings(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	fourWarnings(m)
	if !strings.HasSuffix(line(m, "(? for all)"), "… (? for all)") {
		t.Fatalf("footer has no cut marker:\n%s", screen(m))
	}
	seen := helpSeen(t, m)
	// Every hint in full, one per line, with its count when above 1.
	for i, want := range []string{
		"Warnings",
		"processes of other users have unreadable fields; run with sudo to see them",
		"  (count 12)",
		"docker: not reachable at unix:///var/run/docker.sock",
		"run with sudo to see owners",
		"collection is slow: refreshing every 4s",
	} {
		j := slices.Index(seen, want)
		if j < 0 {
			t.Fatalf("help never shows %q; seen:\n%s", want, strings.Join(seen, "\n"))
		}
		if i > 0 && seen[j-1] == "" {
			t.Errorf("blank line above %q", want)
		}
	}
	if !slices.Contains(seen, "q ctrl-c    quit") {
		t.Error("the key table is not in the overlay")
	}
}

func TestHelpWarningWraps(t *testing.T) {
	long := "some processes have unreadable fields: denied even to root with CAP_SYS_PTRACE, run devdash in the host pid namespace"
	m, _ := newTest(t, 80, 40)
	s := fixture()
	s.Warnings = []model.Warning{{Code: "x", Count: 3, Hint: long}, {Code: "y", Count: 2, Hint: long}}
	feed(m, s)
	press(m, "?")
	hasLine(t, m, "some processes have unreadable fields: denied even to root with")
	hasLine(t, m, "  CAP_SYS_PTRACE, run devdash in the host pid namespace (count 5)")
	if strings.Count(screen(m), "some processes") != 2 { // the footer and the overlay, once each
		t.Errorf("the warning is not listed once:\n%s", screen(m))
	}
}

func TestHelpScroll(t *testing.T) {
	m, _ := newTest(t, 80, 14)
	feed(m, fixture()) // the body is 11 lines: title, 9 overlay lines, the position line
	press(m, "?")
	body := bodyLines(m)
	if len(body) != 11 {
		t.Fatalf("body has %d lines, want 11", len(body))
	}
	if body[0] != "Keys  (↑↓ j k pgup pgdown scroll, any other key closes)" {
		t.Errorf("title %q", body[0])
	}
	total := len(helpKeys) + 3 // blank, Warnings, one warning
	pos := func(from, to int) string { return fmt.Sprintf("lines %d-%d of %d, ↑↓ to scroll", from, to, total) }
	if body[1] != "↑ ↓ j k     move the selection" || body[10] != pos(1, 9) {
		t.Errorf("first page:\n%s", strings.Join(body, "\n"))
	}
	if line(m, "q ctrl-c") != "" {
		t.Error("the last row shows before scrolling")
	}

	press(m, "down", "j")
	if got := bodyLines(m)[10]; got != pos(3, 11) {
		t.Errorf("after down, j: %q", got)
	}
	press(m, "k")
	if got := bodyLines(m)[10]; got != pos(2, 10) {
		t.Errorf("after k: %q", got)
	}
	press(m, "pgdown", "pgdown", "pgdown") // clamped at the end
	if got := bodyLines(m)[10]; got != pos(total-8, total) {
		t.Errorf("after pgdown: %q", got)
	}
	for _, want := range []string{"r           refresh now", "?           this help", "q ctrl-c    quit", "run with sudo to see owners"} {
		hasLine(t, m, want)
	}
	press(m, "pgup", "up", "up")
	if got := bodyLines(m)[10]; got != pos(1, 9) {
		t.Errorf("after pgup, up, up: %q", got)
	}
	press(m, "pgdown", "x") // any other key closes and does nothing else
	if m.help || m.kill.active() {
		t.Error("x did not just close help")
	}
	press(m, "?") // opens at the top again
	if got := bodyLines(m)[10]; got != pos(1, 9) {
		t.Errorf("reopened at %q", got)
	}

	// With one line under the title, it shows an overlay line and no position line.
	m, _ = newTest(t, 80, 5)
	feed(m, fixture())
	press(m, "?")
	if body := bodyLines(m); len(body) != 2 || body[1] != "↑ ↓ j k     move the selection" {
		t.Errorf("at 80x5:\n%s", screen(m))
	}
	press(m, "down")
	if !m.help || bodyLines(m)[1] != "pgup pgdown page the table or the open detail pane; g/home first, G/end last row" {
		t.Errorf("at 80x5 down did not scroll:\n%s", screen(m))
	}
}

// TestHelpNarrow: below 80 columns an action wraps under its own column instead of being cut
// at the screen edge, and the scrolling and the position line count the wrapped lines (DEV-94).
func TestHelpNarrow(t *testing.T) {
	m, _ := newTest(t, 60, 15)
	feed(m, fixture())
	lines := m.helpLines(60)
	for _, l := range lines {
		if n := ansi.StringWidth(l); n > 60 {
			t.Errorf("help line is %d cells at 60 columns: %q", n, ansi.Strip(l))
		}
	}
	total := len(lines)
	if total <= len(helpKeys)+3 { // blank, Warnings, one warning
		t.Fatalf("no action wraps at 60 columns: %d lines", total)
	}
	seen := helpSeen(t, m)
	indent := strings.Repeat(" ", helpKeyWidth)
	for _, want := range []string{
		"pgup pgdown page the table or the open detail pane; g/home",
		indent + "first, G/end last row",
		"x           kill modal: p process, t tree, f force, Y second",
		indent + "confirm, esc cancel",
		"enter       open or close the detail pane (esc closes it",
		indent + "too)",
		"q ctrl-c    quit",
		"run with sudo to see owners",
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("help never shows %q; seen:\n%s", want, strings.Join(seen, "\n"))
		}
	}
	words := map[string]bool{}
	for _, l := range seen {
		for f := range strings.FieldsSeq(l) {
			words[f] = true
		}
	}
	for _, k := range helpKeys {
		for f := range strings.FieldsSeq(k[1]) {
			if !words[f] {
				t.Errorf("help never shows %q of the %q row", f, k[0])
			}
		}
	}
	body := bodyLines(m) // scrolled to the end by helpSeen
	if got, want := body[len(body)-1], fmt.Sprintf("lines %d-%d of %d, ↑↓ to scroll", total-len(body)+3, total, total); got != want {
		t.Errorf("position line at the end %q, want %q", got, want)
	}
	if body[len(body)-2] != "run with sudo to see owners" {
		t.Errorf("the last overlay line is not shown at the end:\n%s", strings.Join(body, "\n"))
	}
}
