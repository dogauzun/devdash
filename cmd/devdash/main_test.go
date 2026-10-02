package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
)

func TestParse(t *testing.T) {
	home := testHome(t)
	j := func(d string) string { return filepath.Join(home, d) }
	def := options{Tick: 2 * time.Second, Timeout: 3 * time.Second}
	with := func(f func(*options)) options { o := def; f(&o); return o }
	tests := []struct {
		args []string
		want options
	}{
		{nil, def},
		{[]string{"--json"}, with(func(o *options) { o.JSON = true })},
		{[]string{"-json"}, with(func(o *options) { o.JSON = true })},
		{[]string{"--roots", "~/code,~/work,,rel"}, with(func(o *options) { o.Roots = []string{j("code"), j("work"), j("rel")} })},
		{[]string{"--roots=~/code", "--roots", "~", "--roots", j("work") + "/"}, with(func(o *options) { o.Roots = []string{j("code"), home, j("work")} })},
		{[]string{"--tick", "500ms"}, with(func(o *options) { o.Tick = 500 * time.Millisecond })},
		{[]string{"--tick=1m"}, with(func(o *options) { o.Tick = time.Minute })},
		{[]string{"--all", "--no-docker", "--no-color"}, with(func(o *options) { o.All, o.NoDocker, o.NoColor = true, true, true })},
		{[]string{"version"}, with(func(o *options) { o.Cmd = "version"; o.Args = []string{} })},
		{[]string{"port", "3000"}, with(func(o *options) { o.Cmd, o.Args = "port", []string{"3000"} })},
		{[]string{"--all", "port", "3000", "--tick", "1s", "--roots", "code"}, with(func(o *options) {
			o.All, o.Tick, o.Roots, o.Cmd, o.Args = true, time.Second, []string{j("code")}, "port", []string{"3000"}
		})},
		{[]string{"kill", "3000", "--tree", "--force", "--yes", "--timeout", "1s"}, with(func(o *options) {
			o.Cmd, o.Args, o.Tree, o.Force, o.Yes, o.Timeout = "kill", []string{"3000"}, true, true, true, time.Second
		})},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			got, err := parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Args) == 0 && len(tt.want.Args) == 0 {
				got.Args, tt.want.Args = nil, nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// testHome makes a temporary $HOME holding the directories code, work and rel and the file
// file, and makes it the working directory.
func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"code", "work", "rel"} {
		if err := os.Mkdir(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(home)
	return home
}

func TestRootsErrors(t *testing.T) {
	home := testHome(t)
	for _, root := range []string{"~nobody/code", "~/missing", filepath.Join(home, "missing"), "file", "code,~/nothing"} {
		t.Run(root, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"--roots", root, "--json"}, &stdout, &stderr, fake()); code != 2 || stdout.Len() != 0 {
				t.Errorf("exit %d, stdout %q; want 2 and nothing", code, stdout.String())
			}
			bad := root[strings.LastIndex(root, "/")+1:]
			if !strings.Contains(stderr.String(), bad) {
				t.Errorf("stderr %q does not name %q", stderr.String(), bad)
			}
		})
	}
}

// TestUsageErrorMessages: every usage error, the flag package's own included, is one
// "devdash: ..." line naming the flag with two dashes, then the usage once, exit 2 (DEV-95).
func TestUsageErrorMessages(t *testing.T) {
	home := testHome(t)
	missing := filepath.Join(home, "missing")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--roots", missing, "--json"}, "--roots " + missing + ": not an existing directory"},
		{[]string{"--roots=" + missing}, "--roots " + missing + ": not an existing directory"},
		{[]string{"--roots", "file"}, "--roots file: not an existing directory"},
		{[]string{"--roots", "code,~/nothing"}, "--roots ~/nothing: not an existing directory"},
		{[]string{"-roots", "~nobody/code"}, "--roots ~nobody/code: ~user is not supported, use the full path"},
		{[]string{"--roots"}, "--roots needs a value"},
		{[]string{"kill", "47002", "--timeout", "abc"}, "--timeout abc: not a duration"},
		{[]string{"kill", "47002", "--timeout"}, "--timeout needs a value"},
		{[]string{"kill", "47002", "--timeout", ""}, `--timeout "": not a duration`},
		{[]string{"--tick", "soon"}, "--tick soon: not a duration"},
		{[]string{"-tick=1 s", "port", "3000"}, `--tick "1 s": not a duration`},
		{[]string{"--tick"}, "--tick needs a value"},
		{[]string{"--tick", "100ms"}, "--tick 100ms is below the minimum of 500ms"},
		{[]string{"--json=maybe"}, "--json=maybe: not true or false"},
		{[]string{"kill", "47002", "--yes=2"}, "--yes=2: not true or false"},
		{[]string{"--all=false", "--no-docker=x"}, "--no-docker=x: not true or false"},
		{[]string{"--bogus"}, "unknown flag --bogus"},
		{[]string{"-bogus=1", "--json"}, "unknown flag --bogus"},
		{[]string{"port", "-1"}, "unknown flag --1"},
		{[]string{"---x"}, "bad flag syntax: ---x"},
		{[]string{"port", "abc"}, `"abc" is not a port number (1-65535)`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr, fake()); code != 2 || stdout.Len() != 0 {
				t.Errorf("exit %d, stdout %q; want 2 and nothing", code, stdout.String())
			}
			if want := "devdash: " + tt.want + "\n" + usage; stderr.String() != want {
				t.Errorf("stderr:\n%s\nwant:\n%s", stderr.String(), want)
			}
		})
	}
}

func TestNoColorEnv(t *testing.T) {
	for env, want := range map[string]bool{"": false, "1": true, "0": true} {
		t.Setenv("NO_COLOR", env)
		if o, err := parse(nil); err != nil || o.NoColor != want {
			t.Errorf("NO_COLOR=%q: NoColor %v, err %v; want %v", env, o.NoColor, err, want)
		}
	}
}

// fake is a collector with one own process listening on 127.0.0.1:3000.
func fake() *collector.Fake {
	res := collector.Result{
		TakenAt:   time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Processes: []collector.Process{{PID: 42, PPID: 1, Name: "node", Argv: []string{"node", "app.js"}, StartTime: time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)}},
		Listeners: []collector.Listener{{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 3000, PID: 42}},
	}
	return &collector.Fake{Steps: []collector.Step{{Result: res}}}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		args       []string
		want       int
		wantStdout string // "*" means any non-empty output
	}{
		// Commands.
		{[]string{"version"}, 0, "devdash dev (commit none, built unknown)\n"},
		{[]string{"--tick", "1s", "version"}, 0, "devdash dev (commit none, built unknown)\n"},
		{[]string{"--json"}, 0, "*"},
		{[]string{"port", "3000"}, 0, "42  node  -  127.0.0.1:3000\n"},
		{[]string{"port", "3001"}, 1, "free\n"},
		{[]string{"port", "3000", "--no-docker"}, 0, "*"},
		{[]string{"-h"}, 0, usage},
		{[]string{"--help"}, 0, usage},
		{[]string{"port", "-h"}, 0, usage},
		// Usage errors: exit 2, nothing on stdout.
		{[]string{"--bogus"}, 2, ""},
		{[]string{"nope"}, 2, ""},
		{[]string{"version", "extra"}, 2, ""},
		{[]string{"--json", "version"}, 2, ""},
		{[]string{"port", "3000", "--json"}, 2, ""},
		{[]string{"--tick", "499ms"}, 2, ""},
		{[]string{"--tick", "0"}, 2, ""},
		{[]string{"--tick", "-1s", "--json"}, 2, ""},
		{[]string{"--tick", "soon"}, 2, ""},
		{[]string{"--tick"}, 2, ""},
		{[]string{"port"}, 2, ""},
		{[]string{"port", "1", "2"}, 2, ""},
		{[]string{"port", "http"}, 2, ""},
		{[]string{"port", "0"}, 2, ""},
		{[]string{"port", "65536"}, 2, ""},
		{[]string{"port", "-1"}, 2, ""},
		{[]string{"port", "3.5"}, 2, ""},
		{[]string{"kill"}, 2, ""},
		{[]string{"kill", "3000", "4000"}, 2, ""},
		{[]string{"kill", "http"}, 2, ""},
		{[]string{"kill", "0"}, 2, ""},
		{[]string{"kill", "3000", "--timeout", "0"}, 2, ""},
		{[]string{"--timeout", "-1s", "kill", "3000"}, 2, ""},
		{[]string{"kill", "3000", "--timeout", "soon"}, 2, ""},
		{[]string{"port", "3000", "--tree"}, 2, ""},
		{[]string{"--yes", "--json"}, 2, ""},
		{[]string{"--force"}, 2, ""},
		{[]string{"version", "--timeout", "1s"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := run(tt.args, &stdout, &stderr, fake())
			if got != tt.want {
				t.Errorf("exit = %d, want %d (stderr %q)", got, tt.want, stderr.String())
			}
			if tt.wantStdout == "*" && stdout.Len() == 0 || tt.wantStdout != "*" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if got == 2 && stderr.Len() == 0 {
				t.Error("exit 2 with nothing on stderr")
			}
			if got == 0 && stderr.Len() != 0 {
				t.Errorf("success with stderr %q", stderr.String())
			}
			if len(tt.args) == 1 && tt.args[0] == "--json" && !json.Valid(stdout.Bytes()) {
				t.Errorf("--json output is not JSON: %s", stdout.String())
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestRunFailed: devdash's own failure is exit 5 with nothing on stdout, never "free" (1).
func TestRunFailed(t *testing.T) {
	broken := func() *collector.Fake {
		return &collector.Fake{Steps: []collector.Step{{Err: errors.New("collector broke")}}}
	}
	for _, args := range [][]string{{"--json"}, {"port", "3000"}, {"port", "3001"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr, broken()); code != 5 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "collector broke") {
				t.Errorf("snapshot fails: exit %d, stdout %q, stderr %q; want 5, nothing, the error", code, stdout.String(), stderr.String())
			}
			stderr.Reset()
			if code := run(args, failWriter{}, &stderr, fake()); code != 5 || !strings.Contains(stderr.String(), "disk full") {
				t.Errorf("stdout fails: exit %d, stderr %q; want 5 and the error", code, stderr.String())
			}
		})
	}
}
