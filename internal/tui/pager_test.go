package tui

import (
	"slices"
	"testing"
)

func TestPagerCut(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	const format = "%d-%d of %d"
	for _, tc := range []struct {
		name      string
		top, room int
		shown     []string
		pos       string
		wantTop   int
		wantPage  int
	}{
		{"room 0 shows nothing", 2, 0, []string{}, "", 2, 1},
		{"room negative shows nothing", 9, -3, []string{}, "", 5, 1},
		{"room 1 shows a line, not the position", 1, 1, []string{"b"}, "", 1, 1},
		{"room 2 shows a line and the position", 1, 2, []string{"b"}, "2-2 of 5", 1, 1},
		{"top past the end is clamped", 9, 3, []string{"d", "e"}, "4-5 of 5", 3, 2},
		{"negative top is clamped", -4, 3, []string{"a", "b"}, "1-2 of 5", 0, 2},
		{"one short of fitting", 0, 5, []string{"a", "b", "c", "d"}, "1-4 of 5", 0, 4},
		{"exactly fits with the position line", 0, 6, []string{"a", "b", "c", "d", "e"}, "1-5 of 5", 0, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pager{top: tc.top}
			shown, pos := p.cut(lines, tc.room, format)
			if !slices.Equal(shown, tc.shown) || pos != tc.pos {
				t.Errorf("cut = %q, %q; want %q, %q", shown, pos, tc.shown, tc.pos)
			}
			if p.top != tc.wantTop || p.page != tc.wantPage {
				t.Errorf("top, page = %d, %d; want %d, %d", p.top, p.page, tc.wantTop, tc.wantPage)
			}
			if len(shown) > 0 {
				_ = append(shown, "x") // appending a position line must not write over lines
				if !slices.Equal(lines, []string{"a", "b", "c", "d", "e"}) {
					t.Errorf("append to the cut wrote into lines: %q", lines)
				}
			}
		})
	}
}

func TestPagerMove(t *testing.T) {
	for _, tc := range []struct {
		name                string
		top, page, d, total int
		want                int
	}{
		{"down", 0, 3, 1, 10, 1},
		{"clamped at the end", 6, 3, 5, 10, 7},
		{"clamped at the start", 2, 3, -5, 10, 0},
		{"page 0 counts as 1", 8, 0, 5, 10, 9},
		{"total shorter than the page", 0, 5, 1, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pager{top: tc.top, page: tc.page}
			p.move(tc.d, tc.total)
			if p.top != tc.want {
				t.Errorf("top = %d, want %d", p.top, tc.want)
			}
		})
	}
}

func TestScrollStep(t *testing.T) {
	for _, tc := range []struct {
		key  string
		page int
		want int
	}{
		{"up", 4, -1}, {"k", 4, -1}, {"down", 4, 1}, {"j", 4, 1},
		{"pgup", 4, -4}, {"pgdown", 4, 4},
		{"pgup", 0, -1}, {"pgdown", 0, 1},
		{"q", 4, 0}, {"home", 4, 0},
	} {
		if got := scrollStep(tc.key, tc.page); got != tc.want {
			t.Errorf("scrollStep(%q, %d) = %d, want %d", tc.key, tc.page, got, tc.want)
		}
	}
}
