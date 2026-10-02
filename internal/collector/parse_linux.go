//go:build linux

package collector

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// Pure parsers for /proc files. Each takes the file contents and does no I/O,
// so they can be tested against literal strings or a fixture proc root.

// pfKthread is PF_KTHREAD from include/linux/sched.h, set in the stat flags of kernel threads.
const pfKthread = 0x00200000

type procStat struct {
	name      string // comm
	state     byte
	ppid      int
	flags     uint64
	utime     uint64 // clock ticks
	stime     uint64 // clock ticks
	starttime uint64 // clock ticks since boot
}

var errMalformed = errors.New("malformed proc file")

// parseStat parses /proc/[pid]/stat. comm sits between the first '(' and the
// last ')' and may itself contain spaces and parentheses.
func parseStat(b []byte) (procStat, error) {
	open := bytes.IndexByte(b, '(')
	closing := bytes.LastIndexByte(b, ')')
	if open < 0 || closing < open {
		return procStat{}, errMalformed
	}
	// Fields after comm, starting with field 3 (state); field n is f[n-3].
	f := strings.Fields(string(b[closing+1:]))
	if len(f) < 20 || len(f[0]) != 1 {
		return procStat{}, errMalformed
	}
	s := procStat{name: string(b[open+1 : closing]), state: f[0][0]}
	var errs [5]error
	s.ppid, errs[0] = strconv.Atoi(f[1])
	s.flags, errs[1] = strconv.ParseUint(f[6], 10, 64)
	s.utime, errs[2] = strconv.ParseUint(f[11], 10, 64)
	s.stime, errs[3] = strconv.ParseUint(f[12], 10, 64)
	s.starttime, errs[4] = strconv.ParseUint(f[19], 10, 64)
	if err := errors.Join(errs[:]...); err != nil {
		return procStat{}, errMalformed
	}
	return s, nil
}

// parseStatusUID returns the effective uid, the second field of the "Uid:" line of
// /proc/[pid]/status (real, effective, saved, filesystem), as macOS reports cr_uid.
func parseStatusUID(b []byte) (int, error) {
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			f := strings.Fields(string(rest))
			if len(f) < 2 {
				break
			}
			return strconv.Atoi(f[1])
		}
	}
	return 0, errMalformed
}

// parseCmdline splits NUL-separated argv. nil means empty (kernel thread,
// zombie, or a process that blanked its argv).
func parseCmdline(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// parseStatmResident returns the resident page count, the second field of /proc/[pid]/statm.
func parseStatmResident(b []byte) (uint64, error) {
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, errMalformed
	}
	return strconv.ParseUint(f[1], 10, 64)
}

// parseBtime returns the boot time in Unix seconds from the "btime" line of /proc/stat.
func parseBtime(b []byte) (int64, error) {
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			return strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
		}
	}
	return 0, errMalformed
}

type tcpListen struct {
	Listener
	uid   int // the socket's uid, column 8
	inode uint64
}

// parseNetTCP returns the listening sockets (state 0A) of /proc/net/tcp or tcp6.
// proto is "tcp4" or "tcp6" and is copied into each Listener, except that a v4-mapped
// address (::ffff:a.b.c.d) is unmapped and reported as tcp4, as macOS reports it.
// Malformed rows are skipped.
func parseNetTCP(b []byte, proto string) []tcpListen {
	var out []tcpListen
	for line := range bytes.Lines(b) {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode ...
		f := strings.Fields(string(line))
		if len(f) < 10 || f[3] != "0A" {
			continue // header row has f[3] == "st"
		}
		host, port, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		addr, err := parseHexAddr(host)
		if err != nil {
			continue
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(f[7])
		if err != nil {
			continue
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			continue
		}
		l := Listener{Proto: proto, Addr: addr, Port: uint16(p)}
		if addr.Is4In6() {
			l.Proto, l.Addr = "tcp4", addr.Unmap()
		}
		out = append(out, tcpListen{l, uid, inode})
	}
	return out
}

// parseHexAddr decodes the address half of a /proc/net/tcp{,6} local_address.
// The kernel prints the in-memory address as 32-bit words with %08X, so each
// word is in host byte order; writing it back in native order restores the
// network-order bytes. 8 hex digits is IPv4, 32 is IPv6.
func parseHexAddr(s string) (netip.Addr, error) {
	if len(s) != 8 && len(s) != 32 {
		return netip.Addr{}, errMalformed
	}
	var b [16]byte
	for i := 0; i < len(s)/8; i++ {
		w, err := strconv.ParseUint(s[8*i:8*i+8], 16, 32)
		if err != nil {
			return netip.Addr{}, errMalformed
		}
		binary.NativeEndian.PutUint32(b[4*i:], uint32(w))
	}
	if len(s) == 8 {
		return netip.AddrFrom4([4]byte(b[:4])), nil
	}
	return netip.AddrFrom16(b), nil
}

// parseSocketLink returns the inode of an fd link target "socket:[12345]".
func parseSocketLink(target string) (uint64, bool) {
	s, ok := strings.CutPrefix(target, "socket:[")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "]")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// hidepidOption returns the hidepid=... option of the proc mount at mountPoint
// in /proc/mounts, or "" when it is absent or off.
func hidepidOption(mounts []byte, mountPoint string) string {
	opt := ""
	for line := range bytes.Lines(mounts) {
		// device mountpoint fstype options dump pass
		f := strings.Fields(string(line))
		if len(f) < 4 || f[1] != mountPoint || f[2] != "proc" {
			continue
		}
		opt = "" // the last mount on a mount point wins
		for o := range strings.SplitSeq(f[3], ",") {
			if v, ok := strings.CutPrefix(o, "hidepid="); ok && v != "0" && v != "off" {
				opt = o
			}
		}
	}
	return opt
}
