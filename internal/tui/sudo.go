package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-144 owns this file: S reruns the dashboard under sudo. It is offered only when
// Options.Sudo says sudo can run and root would see more: a snapshot warning sudo fixes
// (model.Warning.Sudo), or a kill that ended with permission denied. S opens a confirmation;
// y quits with the request, which Run returns so the caller execs sudo once the terminal is
// restored. devdash never reads the password: sudo asks for it on the terminal.

// sudoState is the S confirmation's state; the zero value is closed.
type sudoState struct {
	open   bool // the confirmation has the keyboard
	denied bool // a kill ended with permission denied; sudo stays offered from then on
	asked  bool // y confirmed: Run reports it
}

// sudoHint names S in the footer's key hints and in the kill result.
const sudoHint = "S rerun with sudo"

// sudoOffered reports whether S does anything now.
func (m *Model) sudoOffered() bool {
	return m.o.Sudo && (m.sudo.denied || slices.ContainsFunc(m.upd.Snapshot.Warnings, func(w model.Warning) bool { return w.Sudo }))
}

// sudoStart opens the confirmation (S in the table) when sudo is offered.
func (m *Model) sudoStart() {
	m.sudo.open = m.sudoOffered()
}

// sudoKey handles keys while the confirmation is open: y quits with the request, any other
// key cancels. A held key's repeat does nothing, so holding S never confirms.
func (m *Model) sudoKey(k tea.KeyPressMsg) tea.Cmd {
	if k.IsRepeat {
		return nil
	}
	m.sudo.open = false
	if k.String() != "y" {
		return nil
	}
	m.sudo.asked = true
	return tea.Quit
}

// sudoView draws the confirmation in w by h, like the kill modal: a title, a blank line, what
// happens, a blank line, then the keys, wrapped by killWrap. As in killLayout, when they do not
// fit the blank lines go first, then the explanation from its end; the keys go last (DEV-184).
func sudoView(w, h int) string {
	text := killWrap([]string{
		"The dashboard quits and restarts as root under sudo, with the same flags;",
		"sudo asks for your password on this terminal.",
		"The selection, filter and sort are reset.",
	}, w)
	keys := killWrap([]string{"y rerun with sudo  any other key cancels"}, w)
	blank := []string{""}
	room := h - 1 - len(keys) // below the title
	if room < len(text)+2 {
		blank = nil
	}
	lines := slices.Concat(blank, text[:max(min(len(text), room), 0)], blank, keys)
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(append([]string{styleBold.Render("rerun devdash with sudo")}, lines...), "\n")
}
