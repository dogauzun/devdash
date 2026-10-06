package tui

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
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
func (o openedMsg) apply(m *Model) {
	if o.err != nil {
		m.status = "open failed: " + o.err.Error()
	} else {
		m.status = "opened " + o.url
	}
}

// openSelected opens http://localhost:<port> for the selected row's lowest port: its process's
// listeners, else its container's published TCP ports. A row without a port only gets a status
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

// openPort returns the lowest TCP port of r's process (lowestPort), or when it has none, of its
// container's published TCP ports (a browser cannot use a UDP one); 0 for a header or a row
// without a port.
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
		if p, ok := lowestPort(r.Process); ok {
			lower(int(p))
		}
	}
	if low == 0 && r.Container != nil {
		for _, pm := range r.Container.Ports {
			if pm.TCP() {
				lower(int(pm.HostPort))
			}
		}
	}
	return low
}

// openURL runs opener, open on macOS and xdg-open on Linux, on url, as the user who ran sudo
// when devdash is root (openAs).
func openURL(url string) error {
	cred, env, err := openAs(os.Geteuid(), os.Getenv, runtime.GOOS, user.LookupId)
	if err != nil {
		return err
	}
	return openWith(opener, url, cred, env)
}

// errOpenAsRoot is openAs's refusal: root never starts a browser as root.
var errOpenAsRoot = errors.New("not available as root")

// openAs decides who runs the opener. Not root: nil, nil, nil, devdash itself as before.
// Root: the user that SUDO_UID and SUDO_GID name (decimal, below 2^32-1, uid not 0), with no
// supplementary groups, and environment overrides so that the browser is that user's: HOME,
// USER and LOGNAME from the user database, and on Linux XDG_RUNTIME_DIR=/run/user/<uid> when
// sudo's env_reset removed it (xdg-open reaches the session's D-Bus and Wayland sockets
// through it). Root without such a user, or one the lookup cannot find, is refused.
func openAs(euid int, getenv func(string) string, goos string, lookup func(uid string) (*user.User, error)) (*syscall.Credential, []string, error) {
	if euid != 0 {
		return nil, nil, nil
	}
	id := func(k string) (uint32, bool) {
		n, err := strconv.ParseUint(getenv(k), 10, 32)
		return uint32(n), err == nil && n != math.MaxUint32 // (uid_t)-1 means "unchanged"
	}
	uid, okU := id("SUDO_UID")
	gid, okG := id("SUDO_GID")
	if !okU || !okG || uid == 0 {
		return nil, nil, errOpenAsRoot
	}
	u, err := lookup(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, nil, fmt.Errorf("%w (%v)", errOpenAsRoot, err)
	}
	env := []string{"HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}
	if goos == "linux" && getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid))
	}
	// Groups empty, NoSetGroups false: setgroups(0) drops root's supplementary groups.
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}, env, nil
}

// openWith runs name with url as its only argument, with no terminal (stdin, stdout and
// stderr are the null device, so nothing it prints reaches the screen), and waits up to
// openWait for it to exit. It is always reaped, so it never lingers as a zombie. A non-nil
// cred runs it as that user, with env overriding devdash's environment.
func openWith(name, url string, cred *syscall.Credential, env []string) error {
	cmd := exec.Command(name, url)
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		cmd.Env = append(os.Environ(), env...) // exec keeps the last value of a duplicate
	}
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
