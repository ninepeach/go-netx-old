//go:build linux

package sys

import (
	"syscall"
	"testing"
	"unsafe"
)

func cmsg(level, typ int32, data []byte) []byte {
	hdr := int(unsafe.Sizeof(syscall.Cmsghdr{}))
	b := make([]byte, cmsgAlign(hdr+len(data)))
	h := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = level, typ
	h.SetLen(hdr + len(data))
	copy(b[hdr:], data)
	return b
}

// Checks: SYS-03, UDP-30
func TestDropCountParse(t *testing.T) {
	other := cmsg(syscall.SOL_SOCKET, syscall.SCM_TIMESTAMP, make([]byte, 16))
	drops := cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{7, 0, 0, 0})
	if n, ok := DropCount(append(other, drops...)); !ok || n != 7 {
		t.Fatalf("n=%d ok=%v", n, ok)
	}
	if _, ok := DropCount(other); ok {
		t.Fatal("found drops in unrelated cmsg")
	}
	if _, ok := DropCount([]byte{1, 2, 3}); ok {
		t.Fatal("parsed truncated buffer")
	}
	bad := cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{1, 0, 0, 0})
	(*syscall.Cmsghdr)(unsafe.Pointer(&bad[0])).SetLen(1 << 20) // lies about length
	if _, ok := DropCount(bad); ok {
		t.Fatal("trusted oversized length")
	}
}

// Checks: SYS-03
func TestDropCountDoesNotAllocate(t *testing.T) {
	other := cmsg(syscall.SOL_SOCKET, syscall.SCM_TIMESTAMP, make([]byte, 16))
	buf := append(other, cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{9, 0, 0, 0})...)
	allocs := testing.AllocsPerRun(1000, func() {
		if n, ok := DropCount(buf); !ok || n != 9 {
			t.Fatal("parse failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("%.1f allocs per parse", allocs)
	}
}

func sockaddr4(a [4]byte, port uint16) []byte {
	var sa syscall.RawSockaddrInet4
	sa.Family = syscall.AF_INET
	sa.Addr = a
	p := (*[2]byte)(unsafe.Pointer(&sa.Port))
	p[0], p[1] = byte(port>>8), byte(port)
	return (*[syscall.SizeofSockaddrInet4]byte)(unsafe.Pointer(&sa))[:]
}

func sockaddr6(a [16]byte, port uint16) []byte {
	var sa syscall.RawSockaddrInet6
	sa.Family = syscall.AF_INET6
	sa.Addr = a
	p := (*[2]byte)(unsafe.Pointer(&sa.Port))
	p[0], p[1] = byte(port>>8), byte(port)
	return (*[syscall.SizeofSockaddrInet6]byte)(unsafe.Pointer(&sa))[:]
}

// Checks: SYS-07
func TestDestinationParse(t *testing.T) {
	drops := cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{7, 0, 0, 0})
	v4 := cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddr4([4]byte{198, 18, 0, 1}, 443))
	mapped := [16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}
	v6 := cmsg(syscall.SOL_IPV6, ipv6OrigDstAddr, sockaddr6([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1}, 53))

	for _, c := range []struct {
		name string
		oob  []byte
		want string
	}{
		{"ipv4 after drops", append(append([]byte{}, drops...), v4...), "198.18.0.1:443"},
		{"ipv6", v6, "[2001:db8::1]:53"},
		{"ipv4-mapped is unmapped", cmsg(syscall.SOL_IPV6, ipv6OrigDstAddr, sockaddr6(mapped, 80)), "127.0.0.1:80"},
	} {
		got, ok := Destination(c.oob)
		if !ok || got.String() != c.want {
			t.Fatalf("%s: %v %v, want %s", c.name, got, ok, c.want)
		}
		// The drop count is still found next to it.
		if c.name == "ipv4 after drops" {
			if n, ok := DropCount(c.oob); !ok || n != 7 {
				t.Fatalf("drops beside destination: %d %v", n, ok)
			}
		}
	}

	if _, ok := Destination(drops); ok {
		t.Fatal("found a destination in an unrelated cmsg")
	}
	if _, ok := Destination(nil); ok {
		t.Fatal("found a destination in no control data")
	}
	short := cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddr4([4]byte{1, 2, 3, 4}, 1)[:6])
	if _, ok := Destination(short); ok {
		t.Fatal("parsed a sockaddr shorter than sockaddr_in")
	}
	wrongFamily := cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddr6([16]byte{15: 1}, 1))
	if _, ok := Destination(wrongFamily); ok {
		t.Fatal("trusted an IPv4 cmsg holding an IPv6 sockaddr")
	}
	bad := append([]byte{}, v4...)
	(*syscall.Cmsghdr)(unsafe.Pointer(&bad[0])).SetLen(1 << 20)
	if _, ok := Destination(bad); ok {
		t.Fatal("trusted oversized length")
	}

	buf := append(append([]byte{}, drops...), v6...)
	if allocs := testing.AllocsPerRun(1000, func() { Destination(buf) }); allocs != 0 {
		t.Fatalf("%.1f allocs per parse", allocs)
	}
}
