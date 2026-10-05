package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

func TestFooterHintsFit(t *testing.T) {
	for _, tc := range []struct {
		w    int
		want string
	}{
		{120, "↑↓ move  ←→ fold  enter detail  / filter  x kill  o open  a all  d docker  s sort  r refresh  ? help  q quit"},
		{80, "↑↓ move  ←→ fold  enter detail  / filter  x kill  o open  a all  ? help  q quit"},
		{60, "↑↓ move  ←→ fold  enter detail  / filter  ? help  q quit"},
		{14, "? help  q quit"},
	} {
		if got := footerHints(tc.w); got != tc.want {
			t.Errorf("footerHints(%d)\n got %q\nwant %q", tc.w, got, tc.want)
		}
		if n := ansi.StringWidth(footerHints(tc.w)); n > tc.w {
			t.Errorf("footerHints(%d) is %d cells", tc.w, n)
		}
	}
	for _, w := range []int{60, 80, 120} {
		m, _ := newTest(t, w, 24)
		feed(m, fixture())
		lines := strings.Split(screen(m), "\n")
		if last := lines[len(lines)-1]; !strings.HasSuffix(last, "? help  q quit") {
			t.Errorf("at %d columns the last line is %q", w, last)
		}
	}
}

func TestFooterWarningsWrap(t *testing.T) {
	long := "some processes have unreadable fields: denied even to root with CAP_SYS_PTRACE, run devdash in the host pid namespace"
	m, _ := newTest(t, 80, 24)
	s := fixture()
	s.Warnings = []model.Warning{{Code: "x", Count: 1, Hint: long}}
	feed(m, s)
	lines := strings.Split(screen(m), "\n")
	if len(lines) != 24 {
		t.Fatalf("screen has %d lines, want 24", len(lines))
	}
	got := lines[len(lines)-3] + " " + lines[len(lines)-2]
	if got != long {
		t.Errorf("warning over two lines\n got %q\nwant %q", got, long)
	}

	// Longer than two lines: the second ends in the cut marker, still within the width.
	s.Warnings[0].Hint = strings.Repeat(long+" ", 3)
	feed(m, s)
	lines = strings.Split(screen(m), "\n")
	second := lines[len(lines)-2]
	if !strings.HasSuffix(second, "… (? for all)") || ansi.StringWidth(second) > 80 || !strings.HasPrefix(lines[len(lines)-3], "some processes") {
		t.Errorf("a warning longer than two lines:\n%q\n%q", lines[len(lines)-3], second)
	}
	if len(lines) != 24 {
		t.Errorf("screen has %d lines, want 24", len(lines))
	}
}

// fourWarnings delivers the DEV-85 repro: three snapshot warnings and the engine's slow-refresh
// hint, more than two footer lines hold at 80 columns.
func fourWarnings(m *Model) {
	s := fixture()
	s.Warnings = []model.Warning{
		{Code: "process_fields_unreadable", Count: 12, Hint: "processes of other users have unreadable fields; run with sudo to see them"},
		{Code: "docker_unreachable", Count: 1, Hint: "docker: not reachable at unix:///var/run/docker.sock"},
		{Code: "listener_owner_unreadable", Count: 1, Hint: "run with sudo to see owners"},
	}
	m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 4 * time.Second,
		Warnings: []model.Warning{{Code: "refresh_slowed", Count: 1, Hint: "collection is slow: refreshing every 4s"}}}))
}

func TestFooterWarningsCutMarker(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	fourWarnings(m)
	lines := strings.Split(screen(m), "\n")
	if len(lines) != 24 {
		t.Fatalf("screen has %d lines, want 24", len(lines))
	}
	first, second := lines[len(lines)-3], lines[len(lines)-2]
	if want := "processes of other users have unreadable fields; run with sudo to see them ·"; first != want {
		t.Errorf("first warning line\n got %q\nwant %q", first, want)
	}
	// The cut marker ends the second line, after a whole word, within the width.
	if want := "docker: not reachable at unix:///var/run/docker.sock · run with… (? for all)"; second != want {
		t.Errorf("second warning line\n got %q\nwant %q", second, want)
	}
	if !strings.HasSuffix(lines[len(lines)-1], "? help  q quit") {
		t.Errorf("key line %q", lines[len(lines)-1])
	}

	// Two lines that hold everything get no marker.
	s := fixture()
	s.Warnings = []model.Warning{{Code: "a", Count: 1, Hint: "docker: not reachable at unix:///var/run/docker.sock"}, {Code: "b", Count: 1, Hint: "run with sudo to see owners"}}
	feed(m, s)
	if strings.Contains(screen(m), "for all") || line(m, "docker: not reachable at unix:///var/run/docker.sock · run with sudo to see") == "" || line(m, "owners") != "owners" {
		t.Errorf("warnings that fit are cut:\n%s", screen(m))
	}

	// Every width that cuts keeps the marker whole and the line within the width.
	for _, w := range []int{20, 40, 60, 100} {
		m, _ := newTest(t, w, 24)
		fourWarnings(m)
		var cut string
		for l := range strings.SplitSeq(m.footerView(w), "\n") {
			if n := ansi.StringWidth(l); n > w {
				t.Errorf("at %d columns a footer line is %d cells: %q", w, n, ansi.Strip(l))
			}
			if strings.HasSuffix(ansi.Strip(l), "… (? for all)") {
				cut = l
			}
		}
		if cut == "" {
			t.Errorf("at %d columns no cut marker:\n%s", w, ansi.Strip(m.footerView(w)))
		}
	}
}

func TestWarningsCounts(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	s.Warnings = []model.Warning{
		{Code: "a", Count: 2, Hint: "run with sudo"},
		{Code: "b", Count: 1, Hint: "docker: down"},
		{Code: "c", Count: 3, Hint: "run with sudo"},
		{Code: "no_hint", Count: 1},
	}
	m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 2 * time.Second,
		Warnings: []model.Warning{{Code: "d", Count: 4, Hint: "run with sudo"}}, Err: errors.New("collect: boom")}))
	want := []model.Warning{
		{Code: "a", Count: 9, Hint: "run with sudo"}, // one per hint, counts summed
		{Code: "b", Count: 1, Hint: "docker: down"},
		{Code: "no_hint", Count: 1, Hint: "no_hint"},
		{Code: "refresh_failed", Count: 1, Hint: "refresh failed: collect: boom"},
	}
	if got := m.warnings(); !slices.Equal(got, want) {
		t.Errorf("warnings()\n got %+v\nwant %+v", got, want)
	}
}
