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

// sock is a listener with its kernel socket handle (soi_so in the fd walk, xso_so in the PCB
// list: both are the same VM_KERNEL_ADDRPERM-obfuscated socket pointer), so a socket shared
// across fork is recognised as one socket and SO_REUSEPORT siblings as distinct ones.
type sock struct {
	Listener
	so uint64
}

// decodeSocketFDInfo decodes struct socket_fdinfo and reports a TCP listener
// (soi_so at 160, soi_kind SOCKINFO_TCP = 2 at 256, tcpsi_state TSI_S_LISTEN = 1 at 344,
// insi_lport at 268 as an int holding a network-order u_short, insi_vflag at 288, insi_laddr at 312).
func decodeSocketFDInfo(b []byte) (sock, bool) {
	if len(b) < sizeofSocketFDInfo || le.Uint32(b[256:]) != 2 || le.Uint32(b[344:]) != 1 {
		return sock{}, false
	}
	proto, addr := inpAddr(b[288], b[312:328])
	return sock{Listener{Proto: proto, Addr: addr, Port: binary.BigEndian.Uint16(b[268:])}, le.Uint64(b[160:])}, proto != ""
}

// inpAddr picks an inpcb's local address by its vflag, as netstat does, so the fd walk and the
// PCB list name one socket identically. (soi_family would call an AF_INET6 socket bound to
// ::ffff:127.0.0.1 tcp6; the kernel clears INP_IPV6 on a v4-mapped bind, so it is tcp4 here.)
// A dual-stack socket bound to :: has both flags (netstat's tcp46) and is reported once, as tcp6.
// laddr is an in6_addr, or an in_addr_4in6 with the IPv4 address after 12 bytes of padding.
func inpAddr(vflag byte, laddr []byte) (string, netip.Addr) {
	switch {
	case vflag&0x2 != 0: // INP_IPV6
		return "tcp6", netip.AddrFrom16([16]byte(laddr))
	case vflag&0x1 != 0: // INP_IPV4
		return "tcp4", netip.AddrFrom4([4]byte(laddr[12:]))
	}
	return "", netip.Addr{}
}

// decodeProcArgs2 decodes kern.procargs2: int32 argc, the exec path NUL-padded to a multiple
// of 8 bytes counted from its start (XNU's exec_extract_strings pads it to the pointer size),
// then exactly argc NUL-terminated strings, empty ones included, then the environment, which is
// never read. Trailing empty strings are dropped and an all-empty argv is nil, as on Linux,
// where /proc/[pid]/cmdline cannot tell them from a blanked argv.
func decodeProcArgs2(b []byte) ([]string, bool) {
	if len(b) < 4 {
		return nil, false
	}
	argc := int(int32(le.Uint32(b)))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 || argc < 0 || roundup8(uint32(i+1)) > len(b) {
		return nil, false
	}
	b = b[roundup8(uint32(i+1)):]
	argv := make([]string, 0, min(argc, len(b))) // each string takes at least its NUL
	for range argc {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, false // truncated
		}
		argv = append(argv, string(b[:i]))
		b = b[i+1:]
	}
	for len(argv) > 0 && argv[len(argv)-1] == "" {
		argv = argv[:len(argv)-1]
	}
	if len(argv) == 0 {
		return nil, true
	}
	return argv, true
}

// PCB list record kinds (XSO_* in bsd/sys/socketvar.h). netstat waits for all six per TCP PCB.
const (
	xsoSocket  = 0x01
	xsoInpcb   = 0x10
	xsoTcpcb   = 0x20
	xsoAllTCP  = 0x3f // SOCKET|RCVBUF|SNDBUF|STATS|INPCB|TCPCB
	sizeofXgen = 24   // struct xinpgen
)

// decodePCBList decodes net.inet.tcp.pcblist_n and returns the LISTEN sockets, with
// so_last_pid as the owner, and the number of PCBs whose so_last_pid is not self (pass -1 to
// count all). A list withheld from devdash still holds devdash's own sockets, so zero others
// means withheld.
func decodePCBList(b []byte, self int) (ls []sock, others int) {
	walkPCBList(b, func(_ []byte, s sock, listen bool) {
		if s.PID != self {
			others++
		}
		if listen {
			ls = append(ls, s)
		}
	})
	return ls, others
}

// walkPCBList walks net.inet.tcp.pcblist_n: a struct xinpgen, then per PCB a group of records
// each starting with {u32 len, u32 kind} and padded to 8 bytes, then a closing xinpgen. It
// mirrors netstat's protopr loop, calls fn with each complete group's bytes, and returns the
// offset where the walk stopped (the closing xinpgen).
func walkPCBList(b []byte, fn func(group []byte, s sock, listen bool)) (end int) {
	if len(b) < sizeofXgen {
		return len(b)
	}
	var which uint32
	var cur sock
	state, start := 0, 0
	off := roundup8(le.Uint32(b))
	for off+8 <= len(b) {
		n, kind := le.Uint32(b[off:]), le.Uint32(b[off+4:])
		if n <= sizeofXgen || off+int(n) > len(b) {
			break
		}
		rec := b[off : off+int(n)]
		if which == 0 {
			start = off
		}
		off = min(off+roundup8(n), len(b))
		if kind &= xsoAllTCP; kind == 0 || which&kind != 0 {
			continue
		}
		which |= kind
		switch kind {
		case xsoSocket:
			cur.so, cur.PID = decodeXsocketN(rec)
		case xsoInpcb:
			cur.Proto, cur.Addr, cur.Port = decodeXinpcbN(rec)
		case xsoTcpcb:
			state = decodeXtcpcbN(rec)
		}
		if which != xsoAllTCP {
			continue
		}
		fn(b[start:off], cur, state == 1 && cur.Proto != "") // TCPS_LISTEN
		which, cur, state = 0, sock{}, 0
	}
	return min(off, len(b))
}

// decodeXsocketN decodes struct xsocket_n: xso_so (the socket handle) at 8 and so_last_pid at 68
// (so_uid at 64 is not needed).
func decodeXsocketN(b []byte) (so uint64, lastPID int) {
	if len(b) < 72 {
		return 0, 0
	}
	return le.Uint64(b[8:]), int(int32(le.Uint32(b[68:])))
}

// decodeXinpcbN decodes struct xinpcb_n: inp_lport (network order) at 18, inp_vflag at 44,
// inp_dependladdr at 64.
func decodeXinpcbN(b []byte) (proto string, addr netip.Addr, port uint16) {
	if len(b) < 80 {
		return "", netip.Addr{}, 0
	}
	proto, addr = inpAddr(b[44], b[64:80])
	return proto, addr, binary.BigEndian.Uint16(b[18:])
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
