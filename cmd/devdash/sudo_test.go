package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/tui"
)

// No test of this package looks at the real uid or PATH, or execs anything (DEV-144).
func init() {
	lookSudo = func() string { return "" }
	execve = func(string, []string, []string) error { return errors.New("execve not stubbed") }
}

// execCall is one call of the stubbed execve, with whether the engine had stopped by then.
type execCall struct {
	path          string
	argv, env     []string
	engineStopped bool
}

// stubSudo makes sudo available at path ("" for not), the dashboard return asked and err, and
// execve record its calls and return execErr.
func stubSudo(t *testing.T, path string, asked bool, err, execErr error) (*tui.Options, *[]execCall) {
	t.Helper()
	savedLook, savedDash, savedExec := lookSudo, dashboard, execve
	t.Cleanup(func() { lookSudo, dashboard, execve = savedLook, savedDash, savedExec })
	var got tui.Options
	var updates <-chan engine.Update
	calls := new([]execCall)
	lookSudo = func() string { return path }
	dashboard = func(_ context.Context, o tui.Options, _ ...tea.ProgramOption) (bool, error) {
		got, updates = o, o.Source.Updates()
		return asked, err
	}
	execve = func(path string, argv, env []string) error {
		stopped := false
	drain: // the engine closes Updates when it stops; a stopped engine has nothing left to send
		for {
			select {
			case _, ok := <-updates:
				if !ok {
					stopped = true
					break drain
				}
			default:
				break drain
			}
		}
		*calls = append(*calls, execCall{path, argv, env, stopped})
		return execErr
	}
	return &got, calls
}

func TestRunTUISudo(t *testing.T) {
	args := []string{"--tick", "1s", "--no-docker", "--roots", t.TempDir(), "--all"}
	o, calls := stubSudo(t, "/usr/bin/sudo", true, nil, nil)
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr, fake()); code != 0 || stderr.Len() != 0 {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	if !o.Sudo {
		t.Error("the dashboard was not told sudo is available")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("execve called %d times, want 1", len(*calls))
	}
	c := (*calls)[0]
	want := append([]string{"sudo", "--", exe}, args...)
	if c.path != "/usr/bin/sudo" || !slices.Equal(c.argv, want) {
		t.Errorf("execve(%q, %q), want (%q, %q)", c.path, c.argv, "/usr/bin/sudo", want)
	}
	if !slices.Equal(c.env, os.Environ()) {
		t.Error("execve did not get the current environment")
	}
	if !c.engineStopped {
		t.Error("execve ran before the engine stopped")
	}
}

func TestRunTUISudoExecFails(t *testing.T) {
	_, calls := stubSudo(t, "/usr/bin/sudo", true, nil, errors.New("permission denied"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--no-docker"}, &stdout, &stderr, fake()); code != exitFailed {
		t.Errorf("exit %d, want %d", code, exitFailed)
	}
	if len(*calls) != 1 || !strings.Contains(stderr.String(), "devdash: ") || !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("%d execve calls, stderr %q", len(*calls), stderr.String())
	}
}

func TestRunTUINoSudo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		asked bool
		err   error
		want  int
	}{
		{"quit", "/usr/bin/sudo", false, nil, 0},
		{"dashboard failed", "/usr/bin/sudo", false, errors.New("no terminal"), exitFailed},
		{"sudo unavailable", "", false, nil, 0},
	} {
		o, calls := stubSudo(t, tc.path, tc.asked, tc.err, nil)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--no-docker"}, &stdout, &stderr, fake()); code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.want)
		}
		if len(*calls) != 0 {
			t.Errorf("%s: execve called", tc.name)
		}
		if o.Sudo != (tc.path != "") {
			t.Errorf("%s: Options.Sudo %v", tc.name, o.Sudo)
		}
	}
}
