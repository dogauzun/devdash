package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/model"
)

// DEV-33 owns this file: o opens http://localhost:<lowest port> of the selected row.

// openWait is how long openWith waits for the opener to exit, so that a failure (xdg-open
// finding no browser exits non-zero at once) reaches the footer; an opener still running
// after it (xdg-open waiting on the browser it started) counts as a success.
const openWait = time.Second

// openedMsg reports the outcome of opening url.
type openedMsg struct {
	url string
	err error
}

// apply shows the outcome in the footer.
func (o openedMsg) apply(m *Model) tea.Cmd {
	if o.err != nil {
		m.status = "open failed: " + o.err.Error()
	} else {
		m.status = "opened " + o.url
	}
	return nil
}

// openSelected opens http://localhost:<port> for the selected row's lowest port: its process's
// listeners, else its container's published ports. A row without a port only gets a status
// message. Options.Open runs in the returned command, off the UI goroutine.
func (m *Model) openSelected() tea.Cmd {
	r, ok := m.selected()
	port := 0
	if ok {
		port = openPort(r)
	}
	if port == 0 {
		m.status = "no port to open"
		return nil
	}
	url := "http://localhost:" + strconv.Itoa(port)
	open := m.o.Open
	return func() tea.Msg { return openedMsg{url: url, err: open(url)} }
}

// openPort returns the lowest port of r's process listeners, or when it has none, of its
// container's published ports; 0 for a header or a row without a port.
func openPort(r model.Row) int {
	low := 0
	lower := func(p int) {
		if p != 0 && (low == 0 || p < low) {
			low = p
		}
	}
	if r.Key.Header != model.GroupNone {
		return 0
	}
	if r.Process != nil {
		for _, l := range r.Process.Listeners {
			lower(int(l.Port))
		}
	}
	if low == 0 && r.Container != nil {
		for _, pm := range r.Container.Ports {
			lower(int(pm.HostPort))
		}
	}
	return low
}

// openURL runs open (macOS) or xdg-open (Linux) on url.
func openURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return openWith("open", url)
	case "linux":
		return openWith("xdg-open", url)
	}
	return fmt.Errorf("no browser opener on %s", runtime.GOOS)
}

// openWith runs name with url as its only argument, with no terminal (stdin, stdout and
// stderr are the null device, so nothing it prints reaches the screen), and waits up to
// openWait for it to exit. It is always reaped, so it never lingers as a zombie.
func openWith(name, url string) error {
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	case <-time.After(openWait):
	}
	return nil
}
