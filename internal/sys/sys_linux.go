//go:build linux

package sys

import (
	"encoding/binary"
	"net/netip"
	"os"
	"syscall"
	"unsafe"
)

// MsgTrunc is the recvmsg flag reporting a datagram larger than the buffer.
const MsgTrunc = syscall.MSG_TRUNC

// OOBSize is the control buffer size per datagram: room for the drop
// count (EnableDropCount, 24 bytes) and the destination (EnableDestination,
// up to 48 bytes) together.
const OOBSize = 128

const soRxqOvfl = 40 // SO_RXQ_OVFL

// ipv6OrigDstAddr is IPV6_RECVORIGDSTADDR and the IPV6_ORIGDSTADDR control
// message type (<linux/in6.h>).
const ipv6OrigDstAddr = 74

// DestinationSupported reports whether EnableDestination and Destination
// are implemented.
const DestinationSupported = true

// EnableDestination asks the kernel to report each datagram's destination
// address and port (IP_RECVORIGDSTADDR; on an IPv6 socket also
// IPV6_RECVORIGDSTADDR, so both IPv4-mapped and IPv6 traffic carry it).
// Under TPROXY it is the original destination, not the socket's address.
func EnableDestination(rc syscall.RawConn) error {
	var err error
	cerr := rc.Control(func(fd uintptr) {
		s := int(fd)
		var sa syscall.Sockaddr
		if sa, err = syscall.Getsockname(s); err != nil {
			err = os.NewSyscallError("getsockname", err)
			return
		}
		if _, ok := sa.(*syscall.SockaddrInet6); ok {
			if err = syscall.SetsockoptInt(s, syscall.SOL_IPV6, ipv6OrigDstAddr, 1); err != nil {
				err = os.NewSyscallError("setsockopt IPV6_RECVORIGDSTADDR", err)
				return
			}
		}
		if err = syscall.SetsockoptInt(s, syscall.SOL_IP, syscall.IP_RECVORIGDSTADDR, 1); err != nil {
			err = os.NewSyscallError("setsockopt IP_RECVORIGDSTADDR", err)
		}
	})
	if cerr != nil {
		return cerr
	}
	return err
}

// Destination extracts the destination address from control data
// (IP_ORIGDSTADDR or IPV6_ORIGDSTADDR), with IPv4-mapped addresses
// unmapped and without a zone. It does not allocate.
func Destination(oob []byte) (netip.AddrPort, bool) {
	const hdr = int(unsafe.Sizeof(syscall.Cmsghdr{}))
	for len(oob) >= hdr {
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
		l := int(h.Len)
		if l < hdr || l > len(oob) {
			return netip.AddrPort{}, false
		}
		data := oob[hdr:l]
		switch {
		case h.Level == syscall.SOL_IP && h.Type == syscall.IP_ORIGDSTADDR:
			// struct sockaddr_in: family (host order), port, address.
			if len(data) < syscall.SizeofSockaddrInet4 || binary.NativeEndian.Uint16(data) != syscall.AF_INET {
				return netip.AddrPort{}, false
			}
			return netip.AddrPortFrom(netip.AddrFrom4([4]byte(data[4:8])), binary.BigEndian.Uint16(data[2:4])), true
		case h.Level == syscall.SOL_IPV6 && h.Type == ipv6OrigDstAddr:
			// struct sockaddr_in6: family, port, flow info, address, scope.
			if len(data) < syscall.SizeofSockaddrInet6 || binary.NativeEndian.Uint16(data) != syscall.AF_INET6 {
				return netip.AddrPort{}, false
			}
			return netip.AddrPortFrom(netip.AddrFrom16([16]byte(data[8:24])).Unmap(), binary.BigEndian.Uint16(data[2:4])), true
		}
		next := cmsgAlign(l)
		if next > len(oob) {
			return netip.AddrPort{}, false
		}
		oob = oob[next:]
	}
	return netip.AddrPort{}, false
}

// EnableDropCount asks the kernel to report the socket's cumulative receive
// queue overflow count with every datagram.
func EnableDropCount(rc syscall.RawConn) bool {
	ok := false
	_ = rc.Control(func(fd uintptr) {
		ok = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soRxqOvfl, 1) == nil
	})
	return ok
}

// DropCount extracts the cumulative kernel drop count from control data.
// It does not allocate.
func DropCount(oob []byte) (uint32, bool) {
	const hdr = int(unsafe.Sizeof(syscall.Cmsghdr{}))
	for len(oob) >= hdr {
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
		l := int(h.Len)
		if l < hdr || l > len(oob) {
			return 0, false
		}
		if h.Level == syscall.SOL_SOCKET && h.Type == soRxqOvfl && l >= hdr+4 {
			return *(*uint32)(unsafe.Pointer(&oob[hdr])), true
		}
		next := cmsgAlign(l)
		if next > len(oob) {
			return 0, false
		}
		oob = oob[next:]
	}
	return 0, false
}

func cmsgAlign(n int) int {
	const a = int(unsafe.Sizeof(uintptr(0)))
	return (n + a - 1) &^ (a - 1)
}
