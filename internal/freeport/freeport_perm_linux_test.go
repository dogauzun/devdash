package freeport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// The other-user test needs three uids: root runs the test and owns cleanup, the listener runs
// as listenerUID, and the search runs as searchUID, which cannot see listenerUID's sockets
// through a snapshot without root. Neither unprivileged uid needs to exist in /etc/passwd.
const (
	listenerUID = 65534 // nobody
	searchUID   = 65533

	listenerEnv   = "DEVDASH_FREE_LISTENER" // set in the listener helper
	searchEnv     = "DEVDASH_FREE_SEARCH"   // the ports to search from, comma-separated, in the search helper
	listenerLine  = "free-listener: "       // prefix of the listener's "network port" lines on stdout
	searchResult  = "free-search: "         // prefix of the search helper's JSON line on stdout
	listenerReady = "free-listener-ready"
)

// searchReport is what the search helper prints for each port, for the parent to check.
type searchReport struct {
	UID   int
	Port  uint16
	Probe bool // Probe(Port)
	Found uint16
	OK    bool
	Err   string
}

// TestFindLiveOtherUser is the Release 1.0 gate for `free` (spec, phase 7): a listener of
// another user that the snapshot does not show is skipped, because the bind catches it. Run as
// root, it starts a helper as uid 65534 that listens on 127.0.0.1:0 (and [::1]:0 where the host
// has IPv6), checks with the real collector that those sockets are the helper's, then runs
// Find with the real Probe and an empty snapshot as uid 65533: each port must read as taken
// and the search must answer another port. Nothing listens but the test's own helper, and
// nothing is signalled but by root's cleanup of that helper.
func TestFindLiveOtherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a listener and a search as two other uids (CI runs it with sudo)")
	}
	bin := helperBinary(t)

	listener := exec.Command(bin, "-test.run=^TestFindLiveOtherUserListener$", "-test.count=1")
	listener.Dir = filepath.Dir(bin)
	listener.Env = append(os.Environ(), listenerEnv+"=1")
	listener.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: listenerUID, Gid: listenerUID}}
	stdin, err := listener.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := listener.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	listener.Stderr = os.Stderr
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Closing stdin ends the helper; Kill is for a helper that hangs. Not yet reaped, so
		// the pid still names the test's own child.
		_ = stdin.Close()
		_ = listener.Process.Kill()
		_ = listener.Wait()
	})

	var ports []uint16
	for sc := bufio.NewScanner(stdout); sc.Scan(); {
		line := sc.Text()
		if line == listenerReady {
			break
		}
		if v, ok := strings.CutPrefix(line, listenerLine); ok {
			_, ps, _ := strings.Cut(v, " ")
			p, err := strconv.ParseUint(ps, 10, 16)
			if err != nil || p == 0 {
				t.Fatalf("listener line %q", line)
			}
			ports = append(ports, uint16(p))
		}
	}
	if len(ports) == 0 {
		t.Fatal("the listener helper reported no port")
	}

	// The sockets are the helper's, as uid 65534: the port the search skips is one the test opened.
	raw, err := collector.New().Collect(context.Background(), collector.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range ports {
		if !ownedBy(raw, port, listener.Process.Pid, listenerUID) {
			t.Fatalf("port %d is not held by the listener helper (pid %d, uid %d)", port, listener.Process.Pid, listenerUID)
		}
	}

	var list []string
	for _, p := range ports {
		list = append(list, strconv.Itoa(int(p)))
	}
	search := exec.Command(bin, "-test.run=^TestFindLiveOtherUserSearch$", "-test.count=1")
	search.Dir = filepath.Dir(bin)
	search.Env = append(os.Environ(), searchEnv+"="+strings.Join(list, ","))
	search.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: searchUID, Gid: searchUID}}
	out, err := search.CombinedOutput()
	if err != nil {
		t.Fatalf("search helper (uid %d, %s; every directory above it must be searchable by that uid): %v\n%s", searchUID, bin, err, out)
	}
	var reports []searchReport
	for line := range strings.Lines(string(out)) {
		if js, ok := strings.CutPrefix(line, searchResult); ok {
			var r searchReport
			if err := json.Unmarshal([]byte(js), &r); err != nil {
				t.Fatal(err)
			}
			reports = append(reports, r)
		}
	}
	if len(reports) != len(ports) {
		t.Fatalf("search helper reported %d ports, want %d; output:\n%s", len(reports), len(ports), out)
	}
	for i, r := range reports {
		if r.UID != searchUID {
			t.Fatalf("search ran as uid %d, want %d; output:\n%s", r.UID, searchUID, out)
		}
		if r.Port != ports[i] || r.Probe || r.Err != "" {
			t.Errorf("port %d (searched %d): Probe %v, err %q; want taken, no error", ports[i], r.Port, r.Probe, r.Err)
		}
		if !r.OK || r.Found == r.Port || r.Found < r.Port || r.Found > Last(r.Port) {
			t.Errorf("Find(%d) = %d, %v; want another port in %d-%d", r.Port, r.Found, r.OK, r.Port, Last(r.Port))
		}
	}
}

// TestFindLiveOtherUserListener is TestFindLiveOtherUser's listener; without the environment
// variable it does nothing. It prints its ports, then holds them until stdin closes.
func TestFindLiveOtherUserListener(t *testing.T) {
	if os.Getenv(listenerEnv) == "" {
		return
	}
	if os.Getuid() != listenerUID {
		t.Fatalf("listener runs as uid %d, want %d", os.Getuid(), listenerUID)
	}
	for _, a := range []struct{ network, addr string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		ln, err := net.Listen(a.network, a.addr)
		if err != nil {
			if a.network == "tcp6" {
				continue // no IPv6 on this host
			}
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		fmt.Printf("%s%s %d\n", listenerLine, a.network, ln.Addr().(*net.TCPAddr).Port)
	}
	fmt.Println(listenerReady)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// TestFindLiveOtherUserSearch is TestFindLiveOtherUser's search; without the environment
// variable it does nothing. The snapshot is empty: one taken without root does not show
// another user's listener here, so only the probe can find it.
func TestFindLiveOtherUserSearch(t *testing.T) {
	v := os.Getenv(searchEnv)
	if v == "" {
		return
	}
	if os.Getuid() != searchUID || os.Geteuid() != searchUID {
		t.Fatalf("search runs as uid %d euid %d, want %d", os.Getuid(), os.Geteuid(), searchUID)
	}
	for ps := range strings.SplitSeq(v, ",") {
		p, err := strconv.ParseUint(ps, 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		r := searchReport{UID: os.Getuid(), Port: uint16(p)}
		var errs []error
		r.Probe, err = Probe(r.Port)
		errs = append(errs, err)
		r.Found, r.OK, err = Find(model.Snapshot{}, r.Port, Probe)
		if err = errors.Join(append(errs, err)...); err != nil {
			r.Err = err.Error()
		}
		js, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("%s%s\n", searchResult, js)
	}
}

// ownedBy reports whether the sample has a listener on port held by pid, a process of uid.
func ownedBy(raw collector.Result, port uint16, pid, uid int) bool {
	for _, l := range raw.Listeners {
		if l.Port != port || l.PID != pid {
			continue
		}
		for _, p := range raw.Processes {
			if p.PID == pid && p.UID == uid {
				return true
			}
		}
	}
	return false
}

// helperBinary copies the running test binary into a new directory that every uid can enter
// and returns its path: `go test` builds it in a root-only directory, and the helpers' uids
// must exec it. Running it directly needs no Go cache.
func helperBinary(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "devdash-free-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "freeport.test")
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	// Explicit modes: MkdirTemp makes 0700, and the umask may strip more.
	if err := errors.Join(os.Chmod(dir, 0o755), os.Chmod(bin, 0o755)); err != nil {
		t.Fatal(err)
	}
	return bin
}
