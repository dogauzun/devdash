package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

func TestHeader(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	if got := strings.Split(screen(m), "\n")[0]; got != "devdash · collecting" {
		t.Errorf("before the first snapshot: header %q", got)
	}

	feed(m, fixture())
	want := "mbp · 2 s ago · 2 projects · 6 listeners · 2 containers"
	if got := strings.Split(screen(m), "\n")[0]; got != want {
		t.Errorf("header\n got %q\nwant %q", got, want)
	}

	// Two missed ticks: the age turns into "stale"; the snapshot itself is kept.
	s := fixture()
	s.TakenAt = at(8 * time.Second)
	m.Update(updateMsg(engine.Update{Snapshot: s, Missed: 2, Err: errors.New("collect: context deadline exceeded")}))
	if got := strings.Split(screen(m), "\n")[0]; !strings.HasPrefix(got, "mbp · stale 8 s · 2 projects") {
		t.Errorf("stale header %q", got)
	}
	if !warned(t, m, "mbp · stale", "stale 8 s") {
		t.Error("stale age is not in the warning colour")
	}
	m.Update(updateMsg(engine.Update{Snapshot: s, Missed: 1}))
	if got := strings.Split(screen(m), "\n")[0]; !strings.HasPrefix(got, "mbp · 8 s ago") {
		t.Errorf("one missed tick is not stale yet: %q", got)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0 s", 1999 * time.Millisecond: "1 s", 59 * time.Second: "59 s",
		time.Minute: "1 m", 90 * time.Minute: "1 h",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFooter(t *testing.T) {
	m, _ := newTest(t, 120, 24)
	lines := strings.Split(screen(m), "\n")
	if got := lines[len(lines)-1]; !strings.HasPrefix(got, "↑↓ move") {
		t.Errorf("without warnings the last line is the key hints: %q", got)
	}

	s := fixture()
	s.Warnings = append(s.Warnings, model.Warning{Code: "pcblist_unavailable", Count: 1, Hint: "run with sudo to see owners"})
	m.Update(updateMsg(engine.Update{Snapshot: s, Missed: 1, Err: errors.New("boom"),
		Warnings: []model.Warning{{Code: "refresh_slowed", Count: 1, Hint: "collection is slow: refreshing every 4s"}}}))
	lines = strings.Split(screen(m), "\n")
	want := "run with sudo to see owners · collection is slow: refreshing every 4s · refresh failed: boom"
	if got := lines[len(lines)-2]; got != want {
		t.Errorf("warnings line, deduplicated by hint\n got %q\nwant %q", got, want)
	}
	if len(lines) != 24 {
		t.Errorf("screen has %d lines, want 24", len(lines))
	}
}

func TestScreenSize(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {60, 10}, {200, 3}} {
		m, _ := newTest(t, size[0], size[1])
		feed(m, fixture())
		for _, keys := range [][]string{nil, {"enter"}, {"?"}} {
			press(m, keys...)
			lines := strings.Split(m.View().Content, "\n")
			if len(lines) != size[1] {
				t.Errorf("%v after %v: %d lines, want %d", size, keys, len(lines), size[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w != size[0] {
					t.Errorf("%v after %v: line %d is %d cells wide, want %d", size, keys, i, w, size[0])
				}
			}
			press(m, "esc")
		}
	}
}

func TestQuitAndRefresh(t *testing.T) {
	m, src := newTest(t, 80, 24)
	feed(m, fixture())
	press(m, "r")
	if src.refreshes != 1 {
		t.Errorf("r: %d refreshes, want 1", src.refreshes)
	}
	for _, k := range []string{"q", "ctrl+c"} {
		if cmd := press(m, k); cmd == nil || cmd() != (tea.QuitMsg{}) {
			t.Errorf("%s does not quit", k)
		}
	}
	// ctrl+c quits from inside the filter prompt too, where q is just a letter.
	press(m, "/")
	if cmd := press(m, "ctrl+c"); cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Error("ctrl+c does not quit from the filter prompt")
	}
}

func TestEngineSubscription(t *testing.T) {
	src := &fakeSource{ch: make(chan engine.Update, 1)}
	m := New(Options{Source: src, Now: func() time.Time { return now }, Kill: failKill(t)})
	src.ch <- engine.Update{Snapshot: fixture()}
	msg := m.wait()()
	_, cmd := m.Update(msg)
	if !m.have || cmd == nil {
		t.Fatalf("update not applied (have=%v) or not re-subscribed (cmd=%v)", m.have, cmd)
	}
	close(src.ch)
	if _, cmd := m.Update(cmd()); cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Error("a closed update channel does not quit")
	}
}

func TestKeyRouting(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	s := fixture()
	feed(m, s)
	press(m, "down")
	want := keyOf(s, 200) // api is the most recently active group (go test), so its header, then api
	if m.sel != want {
		t.Fatalf("down selected %+v, want api %+v", m.sel, want)
	}
	// While the filter prompt is open, letters go to it: j is not a move and q does not quit.
	quits := func(cmd tea.Cmd) bool { return cmd != nil && cmd() == (tea.QuitMsg{}) }
	press(m, "/")
	typeText(m, "j")
	if quits(press(m, "q")) {
		t.Error("q quit from the filter prompt")
	}
	press(m, "enter")
	if m.sel != want {
		t.Errorf("selection moved while typing a filter: %+v, want %+v", m.sel, want)
	}
	press(m, "esc") // clear the filter
	// Help takes the next key and closes; that key does nothing else.
	if quits(press(m, "?", "q")) {
		t.Error("q quit from the help overlay")
	}
	if m.help {
		t.Error("help still open")
	}
	if m.sel != want || line(m, "mbp ·") == "" {
		t.Error("closing help changed the selection or the header")
	}
}

// TestHeaderFit: the filter gets its room first; the counts are dropped (containers, then
// listeners, then projects) and then the hostname is cut with "…" before the filter is, and a
// query wider than what is left shows its tail, so the cursor is always on screen (DEV-84).
func TestHeaderFit(t *testing.T) {
	const mac = "Alexs-MacBook-Pro.local" // os.Hostname on a typical Mac
	header := func(m *Model) string {
		t.Helper()
		got := strings.Split(screen(m), "\n")[0]
		if n := ansi.StringWidth(got); n > m.width {
			t.Errorf("header is %d cells at %d columns: %q", n, m.width, got)
		}
		return got
	}
	withHost := func(h string) model.Snapshot {
		s := fixture()
		s.Host.Hostname = h
		return s
	}

	m, _ := newTest(t, 80, 24)
	feed(m, withHost(mac))
	press(m, "/")
	typeText(m, "5173")
	if got, want := header(m), mac+" · 2 s ago · 2 projects · 6 listeners · /5173_"; got != want {
		t.Errorf("prompt at 80 columns\n got %q\nwant %q", got, want)
	}
	press(m, "enter")
	if got, want := header(m), mac+" · 2 s ago · 2 projects · 6 listeners · filter: 5173"; got != want {
		t.Errorf("applied filter at 80 columns\n got %q\nwant %q", got, want)
	}

	// A 60-character query: every count goes, then the hostname is cut.
	q := strings.Repeat("abcdefghij", 5) + "0123456789"
	m, _ = newTest(t, 80, 24)
	feed(m, withHost(mac))
	press(m, "/")
	typeText(m, q)
	if got, want := header(m), "Alex… · 2 s ago · /"+q+"_"; got != want {
		t.Errorf("60-character query\n got %q\nwant %q", got, want)
	}
	press(m, "enter")
	if got, want := header(m), "A… · 2 s ago · filter: …"+q[len(q)-56:]; got != want {
		t.Errorf("60-character applied filter\n got %q\nwant %q", got, want)
	}

	// Wider than the whole line: the hostname is down to one letter and the query shows its
	// tail, the cursor last.
	long := strings.Repeat(q, 2)
	m, _ = newTest(t, 80, 24)
	feed(m, withHost(mac))
	press(m, "/")
	typeText(m, long)
	if got, want := header(m), "A… · 2 s ago · /…"+long[len(long)-62:]+"_"; got != want {
		t.Errorf("120-character query\n got %q\nwant %q", got, want)
	}

	// No filter, narrow: whole counts go, never one cut mid-word.
	m, _ = newTest(t, 50, 24)
	feed(m, fixture())
	if got, want := header(m), "mbp · 2 s ago · 2 projects · 6 listeners"; got != want {
		t.Errorf("at 50 columns\n got %q\nwant %q", got, want)
	}
	m, _ = newTest(t, 30, 24)
	feed(m, fixture())
	if got, want := header(m), "mbp · 2 s ago · 2 projects"; got != want {
		t.Errorf("at 30 columns\n got %q\nwant %q", got, want)
	}

	// Before the first snapshot the prompt still shows.
	m, _ = newTest(t, 30, 24)
	press(m, "/")
	typeText(m, "python3 -m http.server")
	if got, want := header(m), "d… · collecting · /…tp.server_"; got != want {
		t.Errorf("collecting\n got %q\nwant %q", got, want)
	}
}
