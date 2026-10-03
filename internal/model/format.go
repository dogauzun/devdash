package model

import (
	"cmp"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Formatters for text both the dashboard and `port N` show, kept here so that the two read
// the same (spec "Release 1.0", the port answer: the TUI's group header label and uptime).

// Label is how people read p: `name @ branch (worktree)`, with the short SHA for a detached
// HEAD. It is the TUI's group header without the `(here)` suffix, which `port N` replaces with
// "this repo". Snapshot text, not yet cleaned.
func (p Project) Label() string {
	s := p.Name
	if at := cmp.Or(p.Branch, p.ShortSHA); at != "" {
		s += " @ " + at
	}
	if p.Worktree {
		s += " (worktree)"
	}
	return s
}

// Uptime formats a duration as its largest whole unit: 45s, 12m, 3h, 2d.
func Uptime(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", d/time.Second)
	case d < time.Hour:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dd", d/(24*time.Hour))
}

// Clean returns s with every rune that is not printable (unicode.IsPrint: C0 and C1 controls,
// DEL, format characters such as bidi overrides, every space but ' ') and every byte that is
// not valid UTF-8 replaced by '?', as ps(1) does, so each stands for one visible cell. Text
// from the snapshot (names, argv, cwd, branches) comes from other processes, any of which can
// set its own argv, so none of it may reach a terminal raw: a control character can move the
// cursor, retitle the window or split a line.
func Clean(s string) string {
	i := 0
	for i < len(s) && s[i] >= ' ' && s[i] < 0x7f { // printable ASCII, the usual case, is kept as is
		i++
	}
	if i == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 || !unicode.IsPrint(r) {
			b.WriteByte('?')
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}
