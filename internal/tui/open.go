package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"
)

// DEV-33 owns this file: o opens http://localhost:<lowest port> of the selected row.

// openSelected opens the selected row's lowest port in the browser.
//
// ponytail: placeholder until DEV-33.
func (m *Model) openSelected() tea.Cmd { return nil }

// openURL runs open (macOS) or xdg-open (Linux) on url.
//
// ponytail: placeholder until DEV-33.
func openURL(string) error { return errors.ErrUnsupported }
