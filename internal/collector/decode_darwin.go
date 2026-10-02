//go:build darwin

package collector

import (
	"bytes"
	"encoding/binary"
	"net/netip"
)

// One decoder per kernel struct, each taking the raw bytes the kernel returned, so they can be
// tested against recorded blobs. Offsets are from the MacOSX27.sdk headers (sys/proc_info.h) and,
// for the PCB list structs that are not in the public SDK, from XNU's bsd/sys/socketvar.h,
// bsd/netinet/in_pcb.h and bsd/netinet/tcp_var.h, which declare them under #pragma pack(4)
// (so u_int64_t fields are only 4-aligned). Offsets were checked with clang offsetof on arm64,
// the PCB ones against a real blob from Apple-signed sysctl(8); the proc_info layouts match
// gopsutil v4's darwin definitions where it has them. Darwin is little-endian on both
// arm64 and amd64; ports are stored in network byte order.

var le = binary.LittleEndian

// decodeTaskInfo decodes struct proc_taskinfo: resident size in bytes and total user+system
// time in mach absolute time units (pti_total_user at 16, pti_total_system at 24).
func decodeTaskInfo(b []byte) (rss, cpuTicks uint64, ok bool) {
	if len(b) < sizeofProcTaskInfo {
		return 0, 0, false
	}
	return le.Uint64(b[8:]), le.Uint64(b[16:]) + le.Uint64(b[24:]), true
}

// decodeVnodePathInfo decodes struct proc_vnodepathinfo and returns the cwd
// (pvi_cdir.vip_path, a NUL-terminated MAXPATHLEN array at offset 152).
func decodeVnodePathInfo(b []byte) (string, bool) {
	if len(b) < sizeofVnodePathInfo {
		return "", false
	}
	p := cstring(b[152 : 152+1024])
	return p, p != ""
}

// decodeFDList decodes an array of struct proc_fdinfo {int32 proc_fd; uint32 proc_fdtype}
// and returns the socket fds (PROX_FDTYPE_SOCKET = 2).
func decodeFDList(b []byte) []int {
	var fds []int
	for ; len(b) >= sizeofProcFDInfo; b = b[sizeofProcFDInfo:] {
		if le.Uint32(b[4:]) == 2 {
			fds = append(fds, int(int32(le.Uint32(b))))
		}
	}
	return fds
}

// decodeSocketFDInfo decodes struct socket_fdinfo and reports a TCP listener
// (soi_kind SOCKINFO_TCP = 2 at 256, tcpsi_state TSI_S_LISTEN = 1 at 344).
func decodeSocketFDInfo(b []byte) (Listener, bool) {
	if len(b) < sizeofSocketFDInfo || le.Uint32(b[256:]) != 2 || le.Uint32(b[344:]) != 1 {
		return Listener{}, false
	}
	l := Listener{Port: binary.BigEndian.Uint16(b[268:])} // insi_lport: an int holding a network-order u_short
	switch le.Uint32(b[184:]) {                           // soi_family
	case 2: // AF_INET: in4in6_addr, the IPv4 address follows 12 bytes of padding
		l.Proto, l.Addr = "tcp4", netip.AddrFrom4([4]byte(b[312+12:]))
	case 30: // AF_INET6
		l.Proto, l.Addr = "tcp6", netip.AddrFrom16([16]byte(b[312:]))
	default:
		return Listener{}, false
	}
	return l, true
}

// decodeProcArgs2 decodes kern.procargs2: int32 argc, the exec path, NUL padding,
// then argc NUL-terminated strings (followed by the environment, which is ignored).
func decodeProcArgs2(b []byte) ([]string, bool) {
	if len(b) < 4 {
		return nil, false
	}
	argc := int(int32(le.Uint32(b)))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // skip the exec path
	if i < 0 || argc < 0 {
		return nil, false
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	argv := make([]string, 0, argc)
	for range argc {
		if len(b) == 0 {
			break
		}
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			i = len(b)
		}
		argv = append(argv, string(b[:i]))
		b = b[min(i+1, len(b)):]
	}
	return argv, len(argv) > 0
}

// PCB list record kinds (XSO_* in bsd/sys/socketvar.h). netstat waits for all six per TCP PCB.
const (
	xsoSocket  = 0x01
	xsoInpcb   = 0x10
	xsoTcpcb   = 0x20
	xsoAllTCP  = 0x3f // SOCKET|RCVBUF|SNDBUF|STATS|INPCB|TCPCB
	sizeofXgen = 24   // struct xinpgen
)

// pcbListener is one LISTEN entry from net.inet.tcp.pcblist_n.
type pcbListener struct {
	Listener
	UID int
}

// decodePCBList walks net.inet.tcp.pcblist_n: a struct xinpgen, then per PCB a group of
// records each starting with {u32 len, u32 kind} and padded to 8 bytes, then a closing xinpgen.
// It mirrors netstat's protopr loop and returns the LISTEN sockets and the total PCB count.
func decodePCBList(b []byte) (ls []pcbListener, pcbs int) {
	if len(b) < sizeofXgen {
		return nil, 0
	}
	var which uint32
	var cur pcbListener
	var state int
	for off := roundup8(le.Uint32(b)); off+8 <= len(b); {
		n, kind := le.Uint32(b[off:]), le.Uint32(b[off+4:])
		if n <= sizeofXgen || off+int(n) > len(b) {
			break
		}
		rec := b[off : off+int(n)]
		off += roundup8(n)
		if kind &= xsoAllTCP; kind == 0 || which&kind != 0 {
			continue
		}
		which |= kind
		switch kind {
		case xsoSocket:
			cur.PID, cur.UID = decodeXsocketN(rec)
		case xsoInpcb:
			cur.Proto, cur.Addr, cur.Port = decodeXinpcbN(rec)
		case xsoTcpcb:
			state = decodeXtcpcbN(rec)
		}
		if which != xsoAllTCP {
			continue
		}
		pcbs++
		if state == 1 && cur.Proto != "" { // TCPS_LISTEN
			ls = append(ls, cur)
		}
		which, cur, state = 0, pcbListener{}, 0
	}
	return ls, pcbs
}

// decodeXsocketN decodes struct xsocket_n: so_uid at 64, so_last_pid at 68.
func decodeXsocketN(b []byte) (lastPID, uid int) {
	if len(b) < 72 {
		return 0, -1
	}
	return int(int32(le.Uint32(b[68:]))), int(le.Uint32(b[64:]))
}

// decodeXinpcbN decodes struct xinpcb_n: inp_lport (network order) at 18, inp_vflag at 44,
// inp_dependladdr at 64 (in6_addr, or in_addr_4in6 with the IPv4 address at +12).
func decodeXinpcbN(b []byte) (proto string, addr netip.Addr, port uint16) {
	if len(b) < 80 {
		return "", netip.Addr{}, 0
	}
	port = binary.BigEndian.Uint16(b[18:])
	switch vflag := b[44]; {
	case vflag&0x2 != 0: // INP_IPV6 (also set on dual-stack sockets, which netstat calls tcp46)
		return "tcp6", netip.AddrFrom16([16]byte(b[64:])), port
	case vflag&0x1 != 0: // INP_IPV4
		return "tcp4", netip.AddrFrom4([4]byte(b[64+12:])), port
	}
	return "", netip.Addr{}, 0
}

// decodeXtcpcbN decodes struct xtcpcb_n: t_state at 36 (after t_segq, t_dupacks, t_timer[4]).
func decodeXtcpcbN(b []byte) int {
	if len(b) < 40 {
		return -1
	}
	return int(int32(le.Uint32(b[36:])))
}

func roundup8(n uint32) int { return int((n + 7) &^ 7) }

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
