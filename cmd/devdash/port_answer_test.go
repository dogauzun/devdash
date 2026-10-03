package main

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// stubTerminal makes stdout a terminal of width columns (0: width unknown) for one test, or not
// a terminal when tty is false.
func stubTerminal(t *testing.T, tty bool, width int) {
	t.Helper()
	oldTTY, oldWidth := stdoutTerminal, stdoutWidth
	t.Cleanup(func() { stdoutTerminal, stdoutWidth = oldTTY, oldWidth })
	stdoutTerminal = func(io.Writer) bool { return tty }
	stdoutWidth = func(io.Writer) int { return width }
}

// answerFixture is the snapshot the port answer tests start from, taken at t: the main
// repository shop (the Here project), its linked worktree shop-login and another repository
// api; python3 (15669) holds 5173 from the worktree, as in the spec's example.
func answerFixture() model.Snapshot {
	t := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	any4 := netip.IPv4Unspecified()
	return model.Snapshot{
		TakenAt: t,
		Projects: []model.Project{
			{ID: "/code/shop", Root: "/code/shop", Name: "shop", Branch: "main", Here: true},
			{ID: "/wt/shop-login", Root: "/wt/shop-login", Name: "shop", Branch: "feat/login", Worktree: true, MainRepo: "/code/shop"},
			{ID: "/code/api", Root: "/code/api", Name: "api", ShortSHA: "1a2b3c4"},
		},
		Processes: []model.Process{{
			PID: 15669, PPID: 1, Name: "python3", ProjectID: "/wt/shop-login", Cwd: "/wt/shop-login",
			Argv:      []string{"uvicorn", "app:main", "--reload", "--port", "5173"},
			StartTime: t.Add(-3*time.Hour - 20*time.Minute), Tags: model.TagOrphaned | model.TagCwdDeleted,
			Listeners: []model.Listener{{Proto: "tcp4", Addr: any4, Port: 5173}},
		}},
	}
}

// TestWriteAnswer: on a terminal each process holder gets its command, where and tags lines once,
// after its last v1 line, indented by seven spaces, and the answer ends with the next free port
// (spec "Release 1.0", the port answer; DEV-121).
func TestWriteAnswer(t *testing.T) {
	any4, any6, lo4 := netip.IPv4Unspecified(), netip.IPv6Unspecified(), netip.MustParseAddr("127.0.0.1")
	with := func(f func(*model.Snapshot)) model.Snapshot { s := answerFixture(); f(&s); return s }
	proc := func(s *model.Snapshot) *model.Process { return &s.Processes[0] }
	vite := model.Process{PID: 7, PPID: 6, Name: "node", ProjectID: "/code/shop", Cwd: "/code/shop/web",
		Argv: []string{"node", "vite"}, StartTime: answerFixture().TakenAt.Add(-45 * time.Second),
		Listeners: []model.Listener{{Proto: "tcp6", Addr: any6, Port: 5173}}}
	tests := []struct {
		name  string
		s     model.Snapshot
		port  uint16
		width int
		want  string
	}{
		{"the spec's example", with(func(s *model.Snapshot) {
			s.Projects[0].Here, s.Projects[1].Here = false, true
		}), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"another worktree of the Here repository", answerFixture(), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"the Here project is a linked worktree, the holder its main repository", with(func(s *model.Snapshot) {
			s.Projects[0].Here, s.Projects[1].Here = false, true
			proc(s).ProjectID, proc(s).Tags = "/code/shop", 0
		}), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ main, up 3h, this repo, other worktree\n" +
				"next free: 5174\n"},
		{"another repository, detached, one tag", with(func(s *model.Snapshot) {
			proc(s).ProjectID, proc(s).Tags = "/code/api", model.TagOrphaned
		}), 5173, 80,
			"15669  python3  api  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       api @ 1a2b3c4, up 3h\n" +
				"       orphaned\n" +
				"next free: 5174\n"},
		{"devdash run outside every repository", with(func(s *model.Snapshot) {
			s.Projects[0].Here = false
			proc(s).Tags = 0
		}), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h\n" +
				"next free: 5174\n"},
		{"no project: the cwd (a removed worktree)", with(func(s *model.Snapshot) {
			proc(s).ProjectID, proc(s).Tags = "", model.TagCwdDeleted
		}), 5173, 80,
			"15669  python3  -  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       /wt/shop-login, up 3h\n" +
				"       cwd deleted\n" +
				"next free: 5174\n"},
		{"nothing known: no command line, where is -", with(func(s *model.Snapshot) {
			p := proc(s)
			p.ProjectID, p.Cwd, p.Argv, p.Tags, p.StartTime = "", "", nil, 0, time.Time{}
		}), 5173, 80,
			"15669  python3  -  0.0.0.0:5173\n" +
				"       -\n" +
				"next free: 5174\n"},
		{"two sockets on N: the lines follow the last one, once", with(func(s *model.Snapshot) {
			proc(s).Listeners = append(proc(s).Listeners, model.Listener{Proto: "tcp6", Addr: netip.MustParseAddr("::1"), Port: 5173})
			proc(s).Tags = 0
		}), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"15669  python3  shop  [::1]:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"next free: 5174\n"},
		{"two holders keep the v1 alignment", with(func(s *model.Snapshot) {
			proc(s).Tags = 0
			s.Processes = append(s.Processes, vite)
		}), 5173, 80,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"7      node     shop  [::]:5173\n" +
				"       node vite\n" +
				"       shop @ main, up 45s, this repo\n" +
				"next free: 5174\n"},
		{"the command is cut to the terminal width", answerFixture(), 5173, 33,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload…\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"a command that fits exactly is not cut", answerFixture(), 5173, 7 + 37, // the command is 37 cells
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"one cell less is cut", answerFixture(), 5173, 7 + 36,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 51…\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"wide characters count two cells", with(func(s *model.Snapshot) {
			proc(s).Argv, proc(s).Tags = []string{"serve", "日本語のパス"}, 0
		}), 5173, 7 + 12,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       serve 日本…\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"next free: 5174\n"},
		{"width unknown: not cut", answerFixture(), 5173, 0,
			"15669  python3  shop  0.0.0.0:5173\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"       orphaned, cwd deleted\n" +
				"next free: 5174\n"},
		{"control characters do not reach the terminal", with(func(s *model.Snapshot) {
			p := proc(s)
			p.ProjectID, p.Tags, p.Cwd = "", 0, "/tmp/\x1b[2Jx"
			p.Argv = []string{"evil", "\x1b]0;pwned\x07"}
		}), 5173, 80,
			"15669  python3  -  0.0.0.0:5173\n" +
				"       evil ?]0;pwned?\n" +
				"       /tmp/?[2Jx, up 3h\n" +
				"next free: 5174\n"},
		{"container and unknown-owner lines keep their v1 form", model.Snapshot{
			TakenAt:    answerFixture().TakenAt,
			Containers: []model.Container{{ID: "db0123456789", Name: "shop-db-1", Image: "postgres:16", ComposeProject: "shop"}},
			Processes: []model.Process{
				{PID: 20, Name: "docker-proxy", Argv: []string{"/usr/bin/docker-proxy"}, StartTime: answerFixture().TakenAt.Add(-time.Hour),
					ContainerID: "db0123456789", Listeners: []model.Listener{{Proto: "tcp4", Addr: any4, Port: 5432, ContainerID: "db0123456789"}}},
				{PID: 0, Name: "unknown", Listeners: []model.Listener{{Proto: "tcp4", Addr: lo4, Port: 5432}}},
			},
		}, 5432, 80,
			"20  shop-db-1  shop  0.0.0.0:5432    container (postgres:16) via docker-proxy\n" +
				"0   unknown    -     127.0.0.1:5432  owner unknown: run with sudo to see it\n" +
				"next free: 5433\n"},
		{"a container publishing N with no socket", model.Snapshot{
			Containers: []model.Container{{ID: "db0123456789", Name: "shop-db-1", Image: "postgres:16",
				Ports: []model.PortMapping{{HostIP: any4, HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}}},
		}, 5432, 80,
			"-  shop-db-1  -  0.0.0.0:5432  container (postgres:16), no listening socket\n" +
				"next free: 5433\n"},
		{"N = 65535: no next free line", with(func(s *model.Snapshot) {
			proc(s).Listeners[0].Port, proc(s).Tags = 65535, 0
		}), 65535, 80,
			"15669  python3  shop  0.0.0.0:65535\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := writeAnswer(&stdout, &stderr, tt.s, tt.port, tt.width); code != 0 || stdout.String() != tt.want || stderr.Len() != 0 {
				t.Errorf("exit %d, stderr %q, output\n%s\nwant\n%s", code, stderr.String(), stdout.String(), tt.want)
			}
		})
	}
}

// TestWriteAnswerNextFree: the next free port is the search `devdash free N+1` makes, with the
// snapshot's holders skipped; none in range names the range; a probe that fails leaves the line
// out, says why on stderr and keeps exit 0 (DEV-114); a free port is answered without a search.
func TestWriteAnswerNextFree(t *testing.T) {
	boom := errors.New("probing port 5175: bind: operation not permitted")
	base := answerFixture()
	base.Processes[0].Tags = 0
	head := "15669  python3  shop  0.0.0.0:5173\n" +
		"       uvicorn app:main --reload --port 5173\n" +
		"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n"
	at := func(port uint16) model.Snapshot {
		s := answerFixture()
		s.Processes[0].Tags, s.Processes[0].Listeners[0].Port = 0, port
		return s
	}
	tests := []struct {
		name   string
		s      model.Snapshot
		port   uint16
		taken  []uint16
		fail   uint16
		code   int
		stdout string
		stderr string
		asked  []uint16
	}{
		{"probe says taken", base, 5173, []uint16{5174}, 0, 0, head + "next free: 5175\n", "", []uint16{5174, 5175}},
		{"a holder in the snapshot is skipped unprobed", func() model.Snapshot {
			s := base
			s.Processes = append(slices.Clone(s.Processes), model.Process{PID: 9, Name: "redis", Listeners: []model.Listener{{Proto: "tcp4", Port: 5174}}})
			return s
		}(), 5173, nil, 0, 0, head + "next free: 5175\n", "", []uint16{5175}},
		{"none in range", base, 5173, rangeOf(5174, 5273), 0, 0, head + "next free: none in 5174-5273\n", "", rangeOf(5174, 5273)},
		{"none up to 65535", at(65500), 65500, rangeOf(65501, 65535), 0, 0,
			"15669  python3  shop  0.0.0.0:65500\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"next free: none in 65501-65535\n", "", rangeOf(65501, 65535)},
		{"N = 65534 searches 65535 only", at(65534), 65534, nil, 0, 0,
			"15669  python3  shop  0.0.0.0:65534\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n" +
				"next free: 65535\n", "", []uint16{65535}},
		{"N = 65535 searches nothing", at(65535), 65535, nil, 0, 0,
			"15669  python3  shop  0.0.0.0:65535\n" +
				"       uvicorn app:main --reload --port 5173\n" +
				"       shop @ feat/login (worktree), up 3h, this repo, other worktree\n", "", nil},
		{"probe fails", base, 5173, []uint16{5174}, 5175, 0, head, "devdash: next free: " + boom.Error() + "\n", []uint16{5174, 5175}},
		{"free: no search", base, 5174, nil, 0, 1, "free\n", "", nil},
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
			if code := writeAnswer(&stdout, &stderr, tt.s, tt.port, 80); code != tt.code || stdout.String() != tt.stdout || stderr.String() != tt.stderr {
				t.Errorf("exit %d, stderr %q, stdout\n%s\nwant %d, %q,\n%s", code, stderr.String(), stdout.String(), tt.code, tt.stderr, tt.stdout)
			}
			if !slices.Equal(*asked, tt.asked) {
				t.Errorf("probed %v, want %v", *asked, tt.asked)
			}
		})
	}
}

// TestRunPortTerminal: `devdash port N` with a terminal on stdout gives the port answer; piped,
// it prints exactly what the v0.1.1 writer (writePort, unchanged since that tag) prints for the
// same snapshot, probes nothing and writes nothing to stderr.
func TestRunPortTerminal(t *testing.T) {
	asked := stubProbe(t, func(uint16) (bool, error) { return true, nil })
	stubTerminal(t, true, 80)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"port", "3000"}, &stdout, &stderr, fake()); code != 0 || stderr.Len() != 0 ||
		stdout.String() != "42  node  -  127.0.0.1:3000\n       node app.js\n       -, up 1h\nnext free: 3001\n" {
		t.Errorf("terminal: exit %d, stderr %q, stdout\n%s", code, stderr.String(), stdout.String())
	}

	for _, port := range []string{"3000", "3001"} {
		stubTerminal(t, false, 80)
		*asked = nil
		stdout.Reset()
		code := run([]string{"port", port}, &stdout, &stderr, fake())
		s, err := engine.Snapshot(t.Context(), engine.Options{Collector: fake(), Resolver: model.NewResolver("", nil)})
		if err != nil {
			t.Fatal(err)
		}
		var v1 bytes.Buffer
		p, _ := parsePort(port)
		found, _ := writePort(&v1, s, p)
		if want := map[bool]int{true: 0, false: 1}[found]; code != want || stdout.String() != v1.String() || stderr.Len() != 0 || len(*asked) != 0 {
			t.Errorf("piped port %s: exit %d, stdout %q, stderr %q, probed %v; want %d, %q, nothing, nothing", port, code, stdout.String(), stderr.String(), *asked, want, v1.String())
		}
	}
}

// TestRunPortTerminalWriteFails: an answer that cannot be written is exit 5, as piped.
func TestRunPortTerminalWriteFails(t *testing.T) {
	stubTerminal(t, true, 80)
	var stderr bytes.Buffer
	if code := run([]string{"port", "3000"}, failWriter{}, &stderr, fake()); code != 5 || stderr.String() != "devdash: disk full\n" {
		t.Errorf("exit %d, stderr %q; want 5 and the error", code, stderr.String())
	}
}
