package collector

import (
	"net/netip"
	"strconv"
	"testing"
)

func TestUnembedScope(t *testing.T) {
	names := map[uint32]string{1: "lo0", 4: "en0"}
	zone := func(i uint32) string {
		if n, ok := names[i]; ok {
			return n
		}
		return strconv.FormatUint(uint64(i), 10)
	}
	tests := []struct{ in, want string }{
		{"fe80:1::1", "fe80::1%lo0"},                                     // embedded scope, as XNU stores it
		{"fe80:4::aede:48ff:fe00:1122", "fe80::aede:48ff:fe00:1122%en0"}, // a real interface address
		{"fe80:1234::1", "fe80::1%4660"},                                 // unknown interface: the decimal index
		{"febf:1::1", "febf::1%lo0"},                                     // the end of fe80::/10
		{"ff02:1::fb", "ff02::fb%lo0"},                                   // link-local multicast
		{"ff01:4::1", "ff01::1%en0"},                                     // interface-local multicast
		{"fe80::1", "fe80::1"},                                           // link-local, no embedded scope
		{"2001:db8::1", "2001:db8::1"},                                   // global: bytes 2-3 are address
		{"fec0:1::1", "fec0:1::1"},                                       // site-local, just past fe80::/10
		{"ff05:1::1", "ff05:1::1"},                                       // site-local multicast
		{"::1", "::1"},
		{"::", "::"},
		{"127.0.0.1", "127.0.0.1"},
		{"::ffff:169.254.0.1", "::ffff:169.254.0.1"}, // v4 link-local, v4-mapped: nothing embedded
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := unembedScope(netip.MustParseAddr(tt.in), zone)
			if want := netip.MustParseAddr(tt.want); got != want {
				t.Errorf("unembedScope(%s) = %s, want %s", tt.in, got, want)
			}
		})
	}
}
