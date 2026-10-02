package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
)

func TestParse(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	def := options{Tick: 2 * time.Second}
	with := func(f func(*options)) options { o := def; f(&o); return o }
	tests := []struct {
		args []string
		want options
	}{
		{nil, def},
		{[]string{"--json"}, with(func(o *options) { o.JSON = true })},
		{[]string{"-json"}, with(func(o *options) { o.JSON = true })},
		{[]string{"--roots", "/a,/b,,rel"}, with(func(o *options) { o.Roots = []string{"/a", "/b", abs("rel")} })},
		{[]string{"--roots", "/a", "--roots=/b"}, with(func(o *options) { o.Roots = []string{"/a", "/b"} })},
		{[]string{"--tick", "500ms"}, with(func(o *options) { o.Tick = 500 * time.Millisecond })},
		{[]string{"--tick=1m"}, with(func(o *options) { o.Tick = time.Minute })},
		{[]string{"--all", "--no-docker", "--no-color"}, with(func(o *options) { o.All, o.NoDocker, o.NoColor = true, true, true })},
		{[]string{"version"}, with(func(o *options) { o.Cmd = "version"; o.Args = []string{} })},
		{[]string{"port", "3000"}, with(func(o *options) { o.Cmd, o.Args = "port", []string{"3000"} })},
		{[]string{"--all", "port", "3000", "--tick", "1s", "--roots", "/r"}, with(func(o *options) {
			o.All, o.Tick, o.Roots, o.Cmd, o.Args = true, time.Second, []string{"/r"}, "port", []string{"3000"}
		})},
		{[]string{"kill", "3000", "4000"}, with(func(o *options) { o.Cmd, o.Args = "kill", []string{"3000", "4000"} })},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			got, err := parse(tt.args, io.Discard)
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

func TestNoColorEnv(t *testing.T) {
	for env, want := range map[string]bool{"": false, "1": true, "0": true} {
		t.Setenv("NO_COLOR", env)
		if o, err := parse(nil, io.Discard); err != nil || o.NoColor != want {
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
		{nil, 2, ""}, // the dashboard is not implemented yet
		{[]string{"version"}, 0, "devdash dev (commit none, built unknown)\n"},
		{[]string{"--tick", "1s", "version"}, 0, "devdash dev (commit none, built unknown)\n"},
		{[]string{"--json"}, 0, "*"},
		{[]string{"port", "3000"}, 0, "42  node  -  127.0.0.1:3000\n"},
		{[]string{"port", "3001"}, 1, "free\n"},
		{[]string{"port", "3000", "--no-docker"}, 0, "*"},
		{[]string{"kill", "3000"}, 2, ""}, // reserved
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
