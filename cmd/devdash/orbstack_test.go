package main

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// OrbStack as recorded on macOS 27.0.1 with OrbStack 2.2.3 and
// `docker --context orbstack run -d -p 18090:80 nginx:alpine` (DEV-73): the published port is
// held for each family by OrbStack Helper, which runs as the user under launchd and also
// listens on loopback ports of its own.
var (
	orbArgv = []string{"/Applications/OrbStack.app/Contents/Frameworks/OrbStack Helper.app/Contents/MacOS/OrbStack Helper",
		"vmgr", "-build-id", "macho:05775f98b4bb3d75b88520220872c248", "-handoff"}
	orbProbe = model.Container{ID: "0rb0123456789", Name: "devdash-orb-probe", Image: "nginx:alpine", State: "running", Ports: []model.PortMapping{
		{HostIP: netip.IPv4Unspecified(), HostPort: 18090, ContainerPort: 80, Proto: "tcp"},
		{HostIP: netip.IPv6Unspecified(), HostPort: 18090, ContainerPort: 80, Proto: "tcp"},
	}}
)

// orbListeners are the Helper's sockets: *:18090 for each family, then its own 32222 and 59838.
func orbListeners(pid int) []model.RawListener {
	lo4, lo6 := netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()
	return []model.RawListener{
		{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 18090, PID: pid},
		{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 18090, PID: pid},
		{Proto: "tcp4", Addr: lo4, Port: 32222, PID: pid},
		{Proto: "tcp6", Addr: lo6, Port: 32222, PID: pid},
		{Proto: "tcp4", Addr: lo4, Port: 59838, PID: pid},
	}
}

// TestWritePortOrbStack: a published port OrbStack Helper holds names the container via the
// Helper, with no separate "no listening socket" container line; the Helper's own ports stay
// its own, and without Docker the published port shows the Helper (DEV-73).
func TestWritePortOrbStack(t *testing.T) {
	helper := model.Process{PID: 14887, PPID: 1, UID: 501, Name: "OrbStack Helper", Argv: orbArgv, StartTime: time.Unix(1, 0)}
	tests := []struct {
		name       string
		containers []model.Container
		port       uint16
		want       string
	}{
		{"published port", []model.Container{orbProbe}, 18090,
			"14887  devdash-orb-probe  -  0.0.0.0:18090  container (nginx:alpine) via OrbStack Helper\n" +
				"14887  devdash-orb-probe  -  [::]:18090     container (nginx:alpine) via OrbStack Helper\n"},
		{"the helper's own port", []model.Container{orbProbe}, 32222,
			"14887  OrbStack Helper  -  127.0.0.1:32222\n" +
				"14887  OrbStack Helper  -  [::1]:32222\n"},
		{"no docker", nil, 18090,
			"14887  OrbStack Helper  -  0.0.0.0:18090\n" +
				"14887  OrbStack Helper  -  [::]:18090\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := model.Raw{Processes: []model.Process{helper}, Listeners: orbListeners(helper.PID)}
			s := model.Build(raw, model.Snapshot{}, tt.containers, model.NewResolver("", nil))
			var b bytes.Buffer
			found, err := writePort(&b, s, tt.port)
			if err != nil || !found || b.String() != tt.want {
				t.Errorf("found %v, err %v, output\n%s\nwant\n%s", found, err, b.String(), tt.want)
			}
		})
	}
}

// TestKillOrbStack: kill of a published port OrbStack Helper holds is refused with docker stop
// only; the Helper's own port, and the published port with --no-docker, are refused because
// the Helper is a runtime process. Nothing is signalled and the exit code is 6 (DEV-73).
func TestKillOrbStack(t *testing.T) {
	helper := fproc(fakePID+1, 1, "/")
	helper.Name, helper.Argv, helper.UID = "OrbStack Helper", orbArgv, 501
	ls := orbListeners(helper.PID)
	const stop = "use docker stop devdash-orb-probe"
	const runtime = "(OrbStack Helper) is part of the container runtime, not your service: find the container with docker ps"
	tests := []struct {
		name       string
		args       []string
		containers []model.Container
		refusal    string
	}{
		{"published port", []string{"kill", "18090", "--yes"}, []model.Container{orbProbe}, stop},
		{"published port, tree force", []string{"kill", "18090", "--yes", "--tree", "--force"}, []model.Container{orbProbe}, stop},
		{"the helper's own port", []string{"kill", "32222", "--yes"}, []model.Container{orbProbe}, runtime},
		{"the helper's own port, tree force", []string{"kill", "59838", "--yes", "--tree", "--force"}, []model.Container{orbProbe}, runtime},
		{"published port, no docker", []string{"kill", "18090", "--yes", "--no-docker"}, nil, runtime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := stubKill(t, true, "y\ny\n", exited)
			withContainers(t, tt.containers...)
			var stdout, stderr bytes.Buffer
			f := &collector.Fake{Steps: []collector.Step{fstep([]collector.Process{helper}, ls...)}}
			code := run(tt.args, &stdout, &stderr, f)
			if code != exitRefused || len(*plans) != 0 {
				t.Errorf("exit %d, %d plans; want 6 and none\nstdout:\n%s\nstderr:\n%s", code, len(*plans), stdout.String(), stderr.String())
			}
			e := stderr.String()
			if !strings.Contains(e, tt.refusal) || tt.refusal == runtime && strings.Contains(e, "docker stop") ||
				tt.refusal == stop && strings.Contains(e, "container runtime") ||
				strings.Count(e, "refused:") != 1 || !strings.Contains(e, "nothing was signalled") {
				t.Errorf("stderr %q, want the %q refusal only", e, tt.refusal)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want no plan", stdout.String())
			}
		})
	}
}
