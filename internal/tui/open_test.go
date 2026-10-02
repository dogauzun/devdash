package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dogauzun/devdash/internal/model"
)

// openRecorder records the URLs o would open and answers with err.
type openRecorder struct {
	urls []string
	err  error
}

func (r *openRecorder) open(url string) error {
	r.urls = append(r.urls, url)
	return r.err
}

func TestOpenLowestPort(t *testing.T) {
	s := fixture()
	slices.Reverse(s.Processes[4].Listeners) // api: 8081 listed before 8080
	for _, tc := range []struct {
		name string
		key  model.RowKey
		want string
	}{
		{"several listeners", keyOf(s, 200), "http://localhost:8080"},
		{"every interface", keyOf(s, 101), "http://localhost:5173"},
		{"docker-proxy row", keyOf(s, 300), "http://localhost:5432"},
		{"container with no process", model.RowKey{ContainerID: "4e5d6c7b8a90"}, "http://localhost:8000"},
		{"unknown owner", keyOf(s, 0), "http://localhost:631"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &openRecorder{}
			m, _ := newTest(t, 80, 24, func(o *Options) { o.Open = rec.open })
			feed(m, s)
			selectKey(t, m, tc.key)
			cmd := press(m, "o")
			if cmd == nil {
				t.Fatalf("o returned no command; status %q", m.status)
			}
			if len(rec.urls) != 0 {
				t.Fatal("Open ran on the UI goroutine, not in the command")
			}
			m.Update(cmd())
			if !slices.Equal(rec.urls, []string{tc.want}) {
				t.Errorf("opened %q, want %q", rec.urls, tc.want)
			}
			if want := "opened " + tc.want; m.status != want || line(m, want) == "" {
				t.Errorf("status %q, want %q in the footer", m.status, want)
			}
		})
	}
}

func TestOpenContainerPortsFallback(t *testing.T) {
	// A process that reconciliation tied to a container but that has no listener of its own
	// opens the container's lowest published port.
	s := fixture()
	s.Processes[7].Listeners = nil
	s.Containers[0].Ports = append(s.Containers[0].Ports,
		model.PortMapping{HostPort: 0, ContainerPort: 9999, Proto: "tcp"}, // exposed, not published
		model.PortMapping{HostPort: 5433, ContainerPort: 5432, Proto: "tcp"})
	rec := &openRecorder{}
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Open = rec.open })
	feed(m, s)
	selectKey(t, m, keyOf(s, 300))
	m.Update(press(m, "o")())
	if !slices.Equal(rec.urls, []string{"http://localhost:5432"}) {
		t.Errorf("opened %q, want the lowest published port 5432", rec.urls)
	}
}

func TestOpenNoPort(t *testing.T) {
	s := fixture()
	for _, tc := range []struct {
		name string
		key  model.RowKey
	}{
		{"project header", model.RowKey{Header: model.GroupProject, Group: apiID}},
		{"process without listeners", keyOf(s, 201)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTest(t, 80, 24) // the default fake fails the test if Open is called
			feed(m, s)
			selectKey(t, m, tc.key)
			if cmd := press(m, "o"); cmd != nil {
				t.Error("o returned a command for a row without a port")
			}
			if m.status != "no port to open" || line(m, "no port to open") == "" {
				t.Errorf("status %q, want %q in the footer", m.status, "no port to open")
			}
			press(m, "j")
			if m.status != "" {
				t.Errorf("the next key does not clear the status: %q", m.status)
			}
		})
	}
	m, _ := newTest(t, 80, 24)
	if cmd := press(m, "o"); cmd != nil || m.status != "no port to open" {
		t.Errorf("nothing selected: cmd %v, status %q", cmd, m.status)
	}
}

func TestOpenError(t *testing.T) {
	rec := &openRecorder{err: errors.New("xdg-open: exit status 3")}
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Open = rec.open })
	s := fixture()
	feed(m, s)
	selectKey(t, m, keyOf(s, 200))
	m.Update(press(m, "o")())
	if want := "open failed: xdg-open: exit status 3"; m.status != want || line(m, want) == "" {
		t.Errorf("status %q, want %q in the footer", m.status, want)
	}
}

// TestOpenWith runs harmless commands in place of open/xdg-open; no test launches a browser.
func TestOpenWith(t *testing.T) {
	if err := openWith("true", "http://localhost:1"); err != nil {
		t.Errorf("true: %v", err)
	}
	if err := openWith("false", "http://localhost:1"); err == nil || !strings.Contains(err.Error(), "false: exit status 1") {
		t.Errorf("false: %v, want the command and its exit status", err)
	}
	if err := openWith("devdash-no-such-command", "http://localhost:1"); err == nil {
		t.Error("a missing command is not an error")
	}
}
