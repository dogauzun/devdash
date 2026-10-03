package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/freeport"
)

// Every test of this package sees every port as bindable unless it replaces probe itself, so
// no test depends on which ports happen to be taken on the machine.
func init() {
	probe = func(uint16) (bool, error) { return true, nil }
}

// stubProbe replaces probe for one test with f, and records the ports it is asked about.
func stubProbe(t *testing.T, f freeport.Prober) *[]uint16 {
	t.Helper()
	saved := probe
	t.Cleanup(func() { probe = saved })
	asked := new([]uint16)
	probe = func(p uint16) (bool, error) {
		*asked = append(*asked, p)
		return f(p)
	}
	return asked
}

// TestRunFree: the first port nothing in the snapshot holds and the probe can bind is printed
// with exit 0; none in range prints nothing and exits 1; a probe that could not look is exit 5
// with nothing on stdout, never 1. fake() listens on 127.0.0.1:3000.
func TestRunFree(t *testing.T) {
	boom := errors.New("probing port 3002: bind: operation not permitted")
	tests := []struct {
		name   string
		args   []string
		taken  []uint16 // ports the probe cannot bind
		fail   uint16   // a port whose probe fails with boom
		want   int
		stdout string
		stderr string
		asked  []uint16
	}{
		{"from is free", []string{"free", "4000"}, nil, 0, 0, "4000\n", "", []uint16{4000}},
		{"listener in the snapshot is skipped unprobed", []string{"free", "3000"}, nil, 0, 0, "3001\n", "", []uint16{3001}},
		{"probe says taken", []string{"free", "3000"}, []uint16{3001, 3002}, 0, 0, "3003\n", "", []uint16{3001, 3002, 3003}},
		{"flags around free", []string{"--no-docker", "free", "3000", "--roots", "."}, nil, 0, 0, "3001\n", "", []uint16{3001}},
		{"65535", []string{"free", "65535"}, nil, 0, 0, "65535\n", "", []uint16{65535}},
		{"nothing free in range", []string{"free", "3000"}, rangeOf(3001, 3099), 0, 1, "", "", rangeOf(3001, 3099)},
		{"nothing free up to 65535", []string{"free", "65500"}, rangeOf(65500, 65535), 0, 1, "", "", rangeOf(65500, 65535)},
		{"probe fails", []string{"free", "3000"}, []uint16{3001}, 3002, 5, "", "devdash: " + boom.Error() + "\n", []uint16{3001, 3002}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := stubProbe(t, func(p uint16) (bool, error) {
				if p == tt.fail {
					return false, boom
				}
				return !slices.Contains(tt.taken, p), nil
			})
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr, fake()); code != tt.want || stdout.String() != tt.stdout || stderr.String() != tt.stderr {
				t.Errorf("exit %d, stdout %q, stderr %q; want %d, %q, %q", code, stdout.String(), stderr.String(), tt.want, tt.stdout, tt.stderr)
			}
			if !slices.Equal(*asked, tt.asked) {
				t.Errorf("probed %v, want %v", *asked, tt.asked)
			}
		})
	}
}

// TestRunFreeFailed: no snapshot is exit 5 and nothing probed; an unwritable stdout is exit 5.
func TestRunFreeFailed(t *testing.T) {
	asked := stubProbe(t, func(uint16) (bool, error) { return true, nil })
	broken := &collector.Fake{Steps: []collector.Step{{Err: errors.New("collector broke")}}}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"free", "3000"}, &stdout, &stderr, broken); code != 5 || stdout.Len() != 0 || stderr.String() != "devdash: collector broke\n" {
		t.Errorf("snapshot fails: exit %d, stdout %q, stderr %q; want 5, nothing, the error", code, stdout.String(), stderr.String())
	}
	if len(*asked) != 0 {
		t.Errorf("probed %v without a snapshot", *asked)
	}
	stderr.Reset()
	if code := run([]string{"free", "3000"}, failWriter{}, &stderr, fake()); code != 5 || !strings.Contains(stderr.String(), "devdash: disk full") {
		t.Errorf("stdout fails: exit %d, stderr %q; want 5 and the error", code, stderr.String())
	}
}

// TestFreeLive runs `devdash free` with the real collector and probe from a port this test
// holds: the answer is another port of the range.
func TestFreeLive(t *testing.T) {
	stubProbe(t, freeport.Probe)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"free", fmt.Sprint(port)}, &stdout, &stderr, collector.New()); code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	got, err := strconv.ParseUint(strings.TrimSuffix(stdout.String(), "\n"), 10, 16)
	if err != nil || !strings.HasSuffix(stdout.String(), "\n") || uint16(got) == port || uint16(got) < port || uint16(got) > freeport.Last(port) {
		t.Errorf("free %d printed %q; want one other port of %d-%d", port, stdout.String(), port, freeport.Last(port))
	}
}

func rangeOf(from, to uint16) []uint16 {
	var ps []uint16
	for p := int(from); p <= int(to); p++ {
		ps = append(ps, uint16(p))
	}
	return ps
}
