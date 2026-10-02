//go:build linux

package collector

import (
	"net/netip"
	"slices"
	"testing"
)

func TestParseStat(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want procStat
		err  bool
	}{
		{
			name: "plain",
			in:   "1234 (node) S 1 1234 1234 0 -1 4194560 100 0 0 0 7 3 0 0 20 0 1 0 5000 1000000 200 18446744073709551615\n",
			want: procStat{name: "node", state: 'S', ppid: 1, flags: 4194560, utime: 7, stime: 3, starttime: 5000},
		},
		{
			name: "spaces and parens in comm",
			in:   "77 (a b) (c) R 42 77 77 0 -1 4194304 0 0 0 0 11 22 0 0 20 0 1 0 333 0 0",
			want: procStat{name: "a b) (c", state: 'R', ppid: 42, flags: 4194304, utime: 11, stime: 22, starttime: 333},
		},
		{
			name: "kernel thread",
			in:   "2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0 0 0 20 0 1 0 0 0 0",
			want: procStat{name: "kthreadd", state: 'S', ppid: 0, flags: 2129984},
		},
		{name: "truncated", in: "1 (x) S 0 1 1", err: true},
		{name: "no parens", in: "1 x S 0 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 0", err: true},
		{name: "empty", in: "", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStat([]byte(tt.in))
			if (err != nil) != tt.err {
				t.Fatalf("err = %v, want error %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if st, _ := parseStat([]byte(tests[2].in)); st.flags&pfKthread == 0 {
		t.Error("kthreadd flags do not carry PF_KTHREAD")
	}
	if st, _ := parseStat([]byte(tests[0].in)); st.flags&pfKthread != 0 {
		t.Error("user process flags carry PF_KTHREAD")
	}
}

func TestParseStatusUID(t *testing.T) {
	in := "Name:\tbash\nUmask:\t0022\nState:\tS (sleeping)\nPid:\t9\nPPid:\t1\nUid:\t1000\t0\t0\t0\nGid:\t1000\t1000\t1000\t1000\n"
	if uid, err := parseStatusUID([]byte(in)); err != nil || uid != 1000 {
		t.Errorf("got %d, %v; want real uid 1000", uid, err)
	}
	if _, err := parseStatusUID([]byte("Name:\tx\n")); err == nil {
		t.Error("missing Uid line: want error")
	}
}

func TestParseCmdline(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil}, // kernel thread or zombie
		{"\x00", nil},
		{"sleep\x00300\x00", []string{"sleep", "300"}},
		{"node\x00\x00server.js\x00", []string{"node", "", "server.js"}},
		{"nginx: worker process", []string{"nginx: worker process"}}, // rewritten argv, no NUL
	}
	for _, tt := range tests {
		if got := parseCmdline([]byte(tt.in)); !slices.Equal(got, tt.want) {
			t.Errorf("parseCmdline(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseStatmAndBtime(t *testing.T) {
	if n, err := parseStatmResident([]byte("2513 431 380 29 0 109 0\n")); err != nil || n != 431 {
		t.Errorf("statm: got %d, %v; want 431", n, err)
	}
	if bt, err := parseBtime([]byte("cpu  1 2 3\nintr 5\nbtime 1759392000\nprocesses 9\n")); err != nil || bt != 1759392000 {
		t.Errorf("btime: got %d, %v", bt, err)
	}
}

// Fixture rows are as printed on a little-endian host (amd64, arm64).
func TestParseNetTCP(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 23456 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1F90 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1000        0 23457 1 0000000000000000 20 4 30 10 -1
   3: 0A00A8C0:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 222 1 0000000000000000 100 0 0 10 0
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 333 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 444 1 0000000000000000 100 0 0 10 0
   2: 0000000000000000FFFF00000100007F:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 555 1 0000000000000000 100 0 0 10 0
   3: 000080FE000000000000000001000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 666 1 0000000000000000 100 0 0 10 0
`
	l := func(proto, addr string, port uint16, inode uint64) tcpListen {
		return tcpListen{Listener{Proto: proto, Addr: netip.MustParseAddr(addr), Port: port}, inode}
	}
	want := []tcpListen{
		l("tcp4", "127.0.0.1", 8080, 23456),
		l("tcp4", "0.0.0.0", 22, 111),
		l("tcp4", "192.168.0.10", 80, 222),
		l("tcp6", "::", 22, 333),
		l("tcp6", "::1", 3000, 444),
		l("tcp6", "::ffff:127.0.0.1", 8081, 555),
		l("tcp6", "fe80::1", 80, 666),
	}
	got := append(parseNetTCP([]byte(tcp), "tcp4"), parseNetTCP([]byte(tcp6), "tcp6")...)
	if !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := parseNetTCP([]byte("   0: 0100007F:ZZZZ 00000000:0000 0A 0 0 0 0 0 1\n   1: 017F:0016 0:0 0A 0 0 0 0 0 1\n"), "tcp4"); got != nil {
		t.Errorf("malformed rows: got %v, want none", got)
	}
}

func TestParseSocketLink(t *testing.T) {
	tests := []struct {
		in    string
		inode uint64
		ok    bool
	}{
		{"socket:[23456]", 23456, true},
		{"pipe:[23456]", 0, false},
		{"anon_inode:[eventpoll]", 0, false},
		{"/dev/null", 0, false},
		{"socket:[]", 0, false},
	}
	for _, tt := range tests {
		if inode, ok := parseSocketLink(tt.in); inode != tt.inode || ok != tt.ok {
			t.Errorf("parseSocketLink(%q) = %d, %v", tt.in, inode, ok)
		}
	}
}

func TestHidepidOption(t *testing.T) {
	tests := []struct {
		mounts, want string
	}{
		{"proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0\n", ""},
		{"proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2 0 0\n", "hidepid=2"},
		{"proc /proc proc rw,relatime,hidepid=invisible 0 0\n", "hidepid=invisible"},
		{"proc /proc proc rw,relatime,hidepid=0 0 0\n", ""},
		{"proc /proc proc rw,hidepid=2 0 0\nproc /proc proc rw,relatime 0 0\n", ""}, // last mount wins
		{"proc /proc/sys proc ro,hidepid=2 0 0\nsysfs /sys sysfs ro 0 0\n", ""},
	}
	for _, tt := range tests {
		if got := hidepidOption([]byte(tt.mounts), "/proc"); got != tt.want {
			t.Errorf("hidepidOption(%q) = %q, want %q", tt.mounts, got, tt.want)
		}
	}
}
