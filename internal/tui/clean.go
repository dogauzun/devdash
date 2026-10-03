package tui

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Text from the snapshot (process names, argv and cwd, project, branch and container fields,
// the hostname, warning hints and the errors that embed them) comes from other processes, any
// of which can set its own title or argv; so can the filter query a user pastes. None of it
// may reach the terminal raw: a control character can move the cursor, retitle the window or
// split a table row. Everything drawn goes through one of these, at draw time: quote in the
// detail pane, where the exact bytes matter, model.Clean everywhere else, where widths matter.

// quote returns s, Go-quoted when it holds a character that is not printable or is not valid
// UTF-8 (a lone 0x9b is a C1 CSI, yet decodes to the printable U+FFFD).
func quote(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
}

// quoteArgv returns argv for display: an argument that is empty, holds a space or a character
// that is not printable, or is not valid UTF-8 is Go-quoted, so the reader sees where each
// argument ends and nothing in it reaches the terminal.
func quoteArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || !utf8.ValidString(a) ||
			strings.ContainsFunc(a, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return out
}
