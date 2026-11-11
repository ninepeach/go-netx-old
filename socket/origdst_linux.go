//go:build linux

package socket

import (
	"encoding/binary"
	"net/netip"
	"os"
	"syscall"
	"unsafe"
)

// soOriginalDst is SO_ORIGINAL_DST (<linux/netfilter_ipv4.h>), the same
// number as IP6T_SO_ORIGINAL_DST (<linux/netfilter_ipv6/ip6_tables.h>).
const soOriginalDst = 80

// originalDst asks conntrack at the level of the connection's family. An
// IPv4-mapped connection on an IPv6 socket is IPv4 traffic, tracked by
// IPv4 conntrack, so it is asked at SOL_IP.
func originalDst(fd uintptr) (netip.AddrPort, error) {
	s := int(fd)
	local, err := syscall.Getsockname(s)
	if err != nil {
		return netip.AddrPort{}, os.NewSyscallError("getsockname", err)
	}
	level, label := syscall.SOL_IP, "getsockopt SO_ORIGINAL_DST"
	if sa, ok := local.(*syscall.SockaddrInet6); ok && !netip.AddrFrom16(sa.Addr).Is4In6() {
		level, label = syscall.SOL_IPV6, "getsockopt IP6T_SO_ORIGINAL_DST"
	}
	// The kernel writes a sockaddr_in or sockaddr_in6 and accepts any
	// buffer at least that long; IPv6MTUInfo (a sockaddr_in6 and a
	// uint32) is the syscall package's getsockopt that holds either.
	info, err := syscall.GetsockoptIPv6MTUInfo(s, level, soOriginalDst)
	if err != nil {
		return netip.AddrPort{}, os.NewSyscallError(label, err)
	}
	b := (*[syscall.SizeofSockaddrInet6]byte)(unsafe.Pointer(&info.Addr))[:]
	port := binary.BigEndian.Uint16(b[2:4])
	switch binary.NativeEndian.Uint16(b[0:2]) {
	case syscall.AF_INET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port), nil
	case syscall.AF_INET6:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])).Unmap(), port), nil
	}
	return netip.AddrPort{}, os.NewSyscallError(label, syscall.EAFNOSUPPORT)
}
