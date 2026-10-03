package tui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// evil holds what a process can put in its own title or argv: a 7-bit SGR and an OSC title
// ended by BEL, a C1 CSI as a rune (U+009B) and as a raw byte (0x9B, invalid UTF-8), NUL, a
// newline, an invalid 0xFF byte and DEL.
const evil = "a\x1b[31mb\x1b]0;pwned\x07c\u009b2Jd\x9be\x00f\ng\xffh\x7fi"

// evilShown is evil as the table draws it: every one of those characters a '?'.
const evilShown = "a?[31mb?]0;pwned?c?2Jd?e?f?g?h?i"

// evilHost is a hostname with an OSC title and a C1 CSI, short enough to leave the rest of
// the header on screen at 80 columns.
const evilHost = "mbp\x1b]0;x\x07\u009b"

// evilSnapshot is the fixture with evil in every string the TUI draws: node vite's name, argv
// (its tool, so its label, among them), cwd and user, shop's name, branch and main repo, the compose project, shop-db-1's name and
// image and the warning hint; the hostname is evilHost.
func evilSnapshot() model.Snapshot {
	s := fixture()
	vite := &s.Processes[1]
	vite.Name = "node" + evil
	vite.Argv = []string{"node", "vite" + evil, "--port\n5173"}
	vite.Cwd = shopID + evil
	vite.User = "me" + evil
	s.Projects[0].Name = "shop" + evil
	s.Projects[0].Branch = "feat/" + evil
	s.Projects[0].MainRepo = "/src/shop-main" + evil
	for i := range s.Containers {
		s.Containers[i].ComposeProject = "shop" + evil
	}
	s.Containers[0].Name = "shop-db" + evil
	s.Containers[0].Image = "postgres" + evil
	s.Host.Hostname = evilHost
	s.Warnings[0].Hint = "run with sudo" + evil
	return s
}

// styleSGR matches the SGR sequences lipgloss styles emit; nothing else may be raw ESC.
var styleSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// assertNoControl fails unless the view at w by h is free of control characters: once the
// styles' SGR sequences are taken out of the raw content, no C0 control but the layout's
// newlines, no DEL, no C1 rune and no invalid UTF-8 is left, and evil's own SGR (red, which
// no style uses) is not there either. The screen must also be exactly h lines of w cells,
// with the footer's key hints on the last line.
func assertNoControl(t *testing.T, m *Model, where string) {
	t.Helper()
	w, h := m.width, m.height
	raw := m.View().Content
	if strings.Contains(raw, "\x1b[31m") {
		t.Errorf("%s: a process's SGR reaches the terminal", where)
	}
	rest := styleSGR.ReplaceAllString(raw, "")
	for i := 0; i < len(rest); {
		r, n := utf8.DecodeRuneInString(rest[i:])
		if r == utf8.RuneError && n == 1 || r < 0x20 && r != '\n' || r >= 0x7f && r <= 0x9f {
			t.Errorf("%s: raw %q at byte %d: %q", where, rest[i:i+n], i, rest[max(i-20, 0):min(i+20, len(rest))])
		}
		i += n
	}
	lines := strings.Split(ansi.Strip(raw), "\n")
	if len(lines) != h {
		t.Errorf("%s: %d lines on a %d-line terminal:\n%s", where, len(lines), h, screen(m))
	}
	for i, l := range lines {
		if ansi.StringWidth(l) != w {
			t.Errorf("%s: line %d is %d cells wide, want %d: %q", where, i, ansi.StringWidth(l), w, l)
		}
	}
	if !strings.HasPrefix(lines[len(lines)-1], "↑↓ move") {
		t.Errorf("%s: the key hints are not the last line:\n%s", where, screen(m))
	}
}

// lineIndex is the index of the first screen line containing s, or -1.
func lineIndex(m *Model, s string) int {
	for i, l := range strings.Split(screen(m), "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

func TestCleanTable(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		w, h := size[0], size[1]
		ref, _ := newTest(t, w, h)
		feed(ref, fixture())
		m, _ := newTest(t, w, h)
		feed(m, evilSnapshot())
		where := fmt.Sprintf("table %dx%d", w, h)
		assertNoControl(t, m, where)
		// Cells are cut at 100 columns, so the name cells are checked up to the cut.
		for _, want := range []string{"vite" + evilShown[:12], "shop" + evilShown + " @ feat/" + evilShown, "shop-db" + evilShown[:12]} {
			if line(m, want) == "" {
				t.Errorf("%s: no line shows %q:\n%s", where, want, screen(m))
			}
		}
		if !strings.HasPrefix(screen(m), "mbp?]0;x?? · 2 s ago · 2 projects") {
			t.Errorf("%s: the header does not show the cleaned hostname:\n%s", where, screen(m))
		}
		// A newline in argv must not split node vite's row: the rows below it stay put.
		if got, want := lineIndex(m, "esbuild"), lineIndex(ref, "esbuild"); got != want {
			t.Errorf("%s: esbuild on line %d, want %d:\n%s", where, got, want, screen(m))
		}
	}
	// The widest name cell (shop-db-1's, under the compose header) is measured as drawn, with
	// a cell for each stand-in, not as the raw text, whose control characters are 0 wide.
	m, _ := newTest(t, 120, 40)
	feed(m, evilSnapshot())
	label := "shop-db" + evilShown + " (postgres" + evilShown + ")"
	if got, want := m.cache().longest, ansi.StringWidth("  "+markNone+label); got != want {
		t.Errorf("longest name cell = %d, want %d (%q)", got, want, label)
	}
}

func TestCleanDetail(t *testing.T) {
	s := evilSnapshot()
	rows := map[string]model.RowKey{
		"process":      keyOf(s, 101),
		"container":    keyOf(s, 300),
		"project":      {Header: model.GroupProject, Group: shopID},
		"compose":      {Header: model.GroupCompose, Group: "shop" + evil},
		"unknown":      keyOf(s, 0),
		"parent chain": keyOf(s, 102), // esbuild's parent is node vite
	}
	for _, size := range [][2]int{{80, 40}, {120, 40}} {
		for name, k := range rows {
			m, _ := newTest(t, size[0], size[1])
			feed(m, s)
			detailSelect(t, m, k)
			press(m, "enter")
			where := fmt.Sprintf("detail %s %dx%d", name, size[0], size[1])
			assertNoControl(t, m, where)
		}
	}
	// The pane quotes: the exact bytes stay readable.
	m, _ := newTest(t, 80, 40)
	feed(m, s)
	detailSelect(t, m, keyOf(s, 101))
	press(m, "enter")
	for _, want := range []string{`"vitea\x1b[31mb`, ` (nodea\x1b[31mb`} {
		if !strings.Contains(screen(m), want) {
			t.Errorf("the detail pane does not quote the label (%q):\n%s", want, screen(m))
		}
	}
}

func TestCleanKill(t *testing.T) {
	s := evilSnapshot()
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		where := func(stage string) string { return fmt.Sprintf("kill %s %dx%d", stage, size[0], size[1]) }

		m, _, _, fk := newKillTest(t, size[0], size[1], s)
		selectRow(t, m, keyOf(s, 101))
		press(m, "x")
		assertNoControl(t, m, where("confirm"))
		if line(m, "kill vite"+evilShown) == "" {
			t.Errorf("%s: no title with the cleaned name:\n%s", where("confirm"), screen(m))
		}
		press(m, "t") // tree mode: esbuild too, under shop's project name
		assertNoControl(t, m, where("tree"))
		fk.results = append(fk.results, func(p engine.Plan) (engine.Result, error) {
			return outcomes(p, func(model.Process) engine.Outcome {
				return engine.Outcome{Signalled: true, Err: errors.New("still " + evil)}
			}), nil
		})
		run(t, m, press(m, "enter"))
		assertNoControl(t, m, where("report"))

		m, _, fp, fk := newKillTest(t, size[0], size[1], s)
		fk.results = append(fk.results, func(engine.Plan) (engine.Result, error) {
			return engine.Result{}, errors.New("kill " + evil)
		})
		selectRow(t, m, keyOf(s, 101))
		press(m, "x")
		run(t, m, press(m, "enter"))
		assertNoControl(t, m, where("error"))

		fp.refuse = func(engine.KillOptions) error {
			return &engine.Refusal{Reason: "container shop-db" + evil + ": use docker stop shop-db" + evil}
		}
		press(m, "esc", "x")
		assertNoControl(t, m, where("refused"))

		fp.refuse = func(o engine.KillOptions) error { // only tree mode is refused: the confirm stage says why
			if o.Tree {
				return &engine.Refusal{Reason: "tree of " + evil + ": no"}
			}
			return nil
		}
		press(m, "esc", "x", "t")
		assertNoControl(t, m, where("tree refused"))

		fp.refuse = func(engine.KillOptions) error { return errors.New("plan " + evil) }
		press(m, "esc", "x")
		assertNoControl(t, m, where("status"))
		if !strings.Contains(m.status, evil) || line(m, "cannot kill vite"+evilShown) == "" {
			t.Errorf("%s: the footer does not show the cleaned status:\n%s", where("status"), screen(m))
		}

		// Outside every project, the second confirmation names the target too.
		out := evilSnapshot()
		out.Processes[1].ProjectID = ""
		m, _, _, _ = newKillTest(t, size[0], size[1], out)
		selectRow(t, m, keyOf(out, 101))
		press(m, "x", "enter")
		assertNoControl(t, m, where("outside"))
		if line(m, "vite"+evilShown[:12]) == "" || m.kill.stage != killOutside {
			t.Errorf("%s: not the second confirmation for the cleaned name:\n%s", where("outside"), screen(m))
		}
	}
}

func TestCleanFooter(t *testing.T) {
	rec := &openRecorder{err: errors.New("xdg-open: " + evil)}
	m, _ := newTest(t, 200, 24, func(o *Options) { o.Open = rec.open }) // the warnings line fits
	s := evilSnapshot()
	m.Update(updateMsg(engine.Update{Snapshot: s, Interval: 2 * time.Second,
		Warnings: []model.Warning{{Code: "engine", Hint: "engine " + evil}}, Err: errors.New("tick " + evil)}))
	assertNoControl(t, m, "footer warnings")
	for _, want := range []string{"run with sudo" + evilShown, "engine " + evilShown, "refresh failed: tick " + evilShown} {
		if line(m, want) == "" {
			t.Errorf("the footer does not show %q:\n%s", want, screen(m))
		}
	}
	detailSelect(t, m, keyOf(s, 101))
	m.Update(press(m, "o")())
	assertNoControl(t, m, "footer open error")
	if line(m, "open failed: xdg-open: "+evilShown) == "" {
		t.Errorf("the footer does not show the cleaned open error:\n%s", screen(m))
	}
}

func TestCleanFilterEcho(t *testing.T) {
	m, _ := newTest(t, 120, 24) // the header fits
	feed(m, evilSnapshot())
	press(m, "/")
	m.Update(tea.PasteMsg{Content: "x\x1b[31m\x9b\n"})
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y\x1b[31m\u009b\x07\xff"}) // a terminal could deliver this as text
	assertNoControl(t, m, "filter prompt")
	// Paste drops what is not printable (an invalid byte arrives as U+FFFD); typed text is cleaned.
	if want := " · /x[31m\ufffdy?[31m???_"; !strings.Contains(strings.Split(screen(m), "\n")[0], want) {
		t.Errorf("the header does not echo the query:\n%s", screen(m))
	}
	press(m, "enter")
	assertNoControl(t, m, "filter echo")
	if want := " · filter: x[31m\ufffdy?[31m???"; !strings.HasSuffix(strings.Split(screen(m), "\n")[0], want) {
		t.Errorf("the header does not echo the filter:\n%s", screen(m))
	}
}

func TestQuoteArgv(t *testing.T) {
	// 0x9b is a C1 CSI on its own; as invalid UTF-8 it decodes to the printable U+FFFD.
	got := quoteArgv([]string{"sh", "-c", "echo hi", "", "\x1b[31mred", "\x9b31mred", "café"})
	want := []string{"sh", "-c", `"echo hi"`, `""`, `"\x1b[31mred"`, `"\x9b31mred"`, "café"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("quoteArgv = %q, want %q", got, want)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/src/my app": "/src/my app",
		"café":        "café",
		"a\x1b[2Jb":   `"a\x1b[2Jb"`,
		"a\x9b2Jb":    `"a\x9b2Jb"`,
		"\xff":        `"\xff"`,
		"tab\there":   `"tab\there"`,
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %q, want %q", in, got, want)
		}
	}
}
