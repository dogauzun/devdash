package tui

import "charm.land/lipgloss/v2"

// Styles use the 16 ANSI colours, so the terminal's own light or dark palette decides how
// they look, and only attributes that survive --no-color and NO_COLOR carry meaning: the
// selection is reverse video and hidden-but-connected rows are faint, so everything stays
// readable without colour (the program downsamples colours away; see Run).
var (
	accent      = lipgloss.Color("6") // cyan
	styleSel    = lipgloss.NewStyle().Reverse(true).Foreground(accent)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow: stale, permission hints
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleAccent = lipgloss.NewStyle().Foreground(accent)
)
