package collector

import (
	"encoding/binary"
	"net/netip"
)

// unembedScope undoes the KAME convention XNU keeps inside the kernel: a scoped IPv6 address
// (link-local unicast fe80::/10, link-local multicast ff02::/16, interface-local multicast
// ff01::/16) carries its interface index in bytes 2-3, big-endian, so fe80::1%lo0 is stored as
// fe80:1::1. When those bytes are non-zero they are cleared and the index becomes the zone,
// named by zone (the interface name, or the decimal index), as netstat prints it (lsof prints the embedded form).
// Any other address is returned as is. It is pure so it can be tested on any OS.
func unembedScope(a netip.Addr, zone func(index uint32) string) netip.Addr {
	scoped := a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast()
	if !scoped || !a.Is6() || a.Is4In6() { // a v4-mapped 169.254/16 is link-local too
		return a
	}
	b := a.As16()
	idx := binary.BigEndian.Uint16(b[2:])
	if idx == 0 {
		return a
	}
	b[2], b[3] = 0, 0
	return netip.AddrFrom16(b).WithZone(zone(uint32(idx)))
}
