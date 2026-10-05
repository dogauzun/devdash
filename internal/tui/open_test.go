package tui

import (
	"errors"
	"os/user"
	"reflect"
	"slices"
	"strings"
	"syscall"
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
			openOther(m) // the unknown owner's row
			selectRow(t, m, tc.key)
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
	// opens the container's lowest published TCP port.
	s := fixture()
	s.Processes[7].Listeners = nil
	s.Containers[0].Ports = append(s.Containers[0].Ports,
		model.PortMapping{HostPort: 0, ContainerPort: 9999, Proto: "tcp"}, // exposed, not published
		model.PortMapping{HostPort: 53, ContainerPort: 53, Proto: "udp"},  // published, but no browser speaks UDP
		model.PortMapping{HostPort: 5433, ContainerPort: 5432, Proto: "tcp"})
	rec := &openRecorder{}
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Open = rec.open })
	feed(m, s)
	selectRow(t, m, keyOf(s, 300))
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
			selectRow(t, m, tc.key)
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
	selectRow(t, m, keyOf(s, 200))
	m.Update(press(m, "o")())
	if want := "open failed: xdg-open: exit status 3"; m.status != want || line(m, want) == "" {
		t.Errorf("status %q, want %q in the footer", m.status, want)
	}
}

func TestOpenRefusedAsRoot(t *testing.T) {
	rec := &openRecorder{err: errOpenAsRoot}
	m, _ := newTest(t, 80, 24, func(o *Options) { o.Open = rec.open })
	s := fixture()
	feed(m, s)
	selectRow(t, m, keyOf(s, 200))
	m.Update(press(m, "o")())
	if want := "open failed: not available as root"; m.status != want || line(m, want) == "" {
		t.Errorf("status %q, want %q in the footer", m.status, want)
	}
}

// TestOpenAs decides who runs the opener; nothing is started.
func TestOpenAs(t *testing.T) {
	users := map[string]*user.User{"501": {Uid: "501", Username: "me", HomeDir: "/Users/me"}}
	lookup := func(uid string) (*user.User, error) {
		if u, ok := users[uid]; ok {
			return u, nil
		}
		return nil, user.UnknownUserIdError(0)
	}
	me := &syscall.Credential{Uid: 501, Gid: 20, Groups: []uint32{}}
	for _, tc := range []struct {
		name     string
		euid     int
		env      map[string]string
		goos     string
		wantCred *syscall.Credential
		wantEnv  []string
		wantErr  bool
	}{
		{name: "not root", euid: 501, env: map[string]string{"SUDO_UID": "0", "SUDO_GID": "0"}, goos: "darwin"},
		{name: "not root, sudo -u", euid: 502, env: map[string]string{"SUDO_UID": "501", "SUDO_GID": "20"}, goos: "linux"},
		{name: "sudo on macOS", euid: 0, env: map[string]string{"SUDO_UID": "501", "SUDO_GID": "20"}, goos: "darwin",
			wantCred: me, wantEnv: []string{"HOME=/Users/me", "USER=me", "LOGNAME=me"}},
		{name: "sudo on Linux", euid: 0, env: map[string]string{"SUDO_UID": "501", "SUDO_GID": "20"}, goos: "linux",
			wantCred: me, wantEnv: []string{"HOME=/Users/me", "USER=me", "LOGNAME=me", "XDG_RUNTIME_DIR=/run/user/501"}},
		{name: "sudo -E on Linux keeps XDG_RUNTIME_DIR", euid: 0, goos: "linux",
			env:      map[string]string{"SUDO_UID": "501", "SUDO_GID": "20", "XDG_RUNTIME_DIR": "/run/user/501"},
			wantCred: me, wantEnv: []string{"HOME=/Users/me", "USER=me", "LOGNAME=me"}},
		{name: "root login", euid: 0, goos: "linux", wantErr: true},
		{name: "SUDO_GID missing", euid: 0, env: map[string]string{"SUDO_UID": "501"}, goos: "linux", wantErr: true},
		{name: "SUDO_UID root", euid: 0, env: map[string]string{"SUDO_UID": "0", "SUDO_GID": "0"}, goos: "darwin", wantErr: true},
		{name: "non-numeric", euid: 0, env: map[string]string{"SUDO_UID": "me", "SUDO_GID": "20"}, goos: "darwin", wantErr: true},
		{name: "signed", euid: 0, env: map[string]string{"SUDO_UID": "+501", "SUDO_GID": "20"}, goos: "darwin", wantErr: true},
		{name: "negative gid", euid: 0, env: map[string]string{"SUDO_UID": "501", "SUDO_GID": "-1"}, goos: "darwin", wantErr: true},
		{name: "uid overflows", euid: 0, env: map[string]string{"SUDO_UID": "4294967797", "SUDO_GID": "20"}, goos: "linux", wantErr: true},
		{name: "uid is (uid_t)-1", euid: 0, env: map[string]string{"SUDO_UID": "4294967295", "SUDO_GID": "20"}, goos: "linux", wantErr: true},
		{name: "unknown user", euid: 0, env: map[string]string{"SUDO_UID": "502", "SUDO_GID": "20"}, goos: "linux", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cred, env, err := openAs(tc.euid, func(k string) string { return tc.env[k] }, tc.goos, lookup)
			if tc.wantErr {
				if !errors.Is(err, errOpenAsRoot) || cred != nil || env != nil {
					t.Fatalf("got %+v %q %v, want only errOpenAsRoot", cred, env, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cred, tc.wantCred) || !slices.Equal(env, tc.wantEnv) {
				t.Errorf("got %+v %q, want %+v %q", cred, env, tc.wantCred, tc.wantEnv)
			}
		})
	}
}

// TestOpenWith runs harmless commands in place of open/xdg-open; no test launches a browser.
func TestOpenWith(t *testing.T) {
	if err := openWith("true", "http://localhost:1", nil, nil); err != nil {
		t.Errorf("true: %v", err)
	}
	if err := openWith("false", "http://localhost:1", nil, nil); err == nil || !strings.Contains(err.Error(), "false: exit status 1") {
		t.Errorf("false: %v, want the command and its exit status", err)
	}
	if err := openWith("devdash-no-such-command", "http://localhost:1", nil, nil); err == nil {
		t.Error("a missing command is not an error")
	}
}
