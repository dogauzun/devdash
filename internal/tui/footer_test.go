package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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

	// Longer than two lines: the second ends in an ellipsis, still within the width.
	s.Warnings[0].Hint = strings.Repeat(long+" ", 3)
	feed(m, s)
	lines = strings.Split(screen(m), "\n")
	second := lines[len(lines)-2]
	if !strings.HasSuffix(second, "…") || ansi.StringWidth(second) > 80 || !strings.HasPrefix(lines[len(lines)-3], "some processes") {
		t.Errorf("a warning longer than two lines:\n%q\n%q", lines[len(lines)-3], second)
	}
	if len(lines) != 24 {
		t.Errorf("screen has %d lines, want 24", len(lines))
	}
}
