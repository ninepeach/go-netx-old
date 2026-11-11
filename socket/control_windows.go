//go:build windows

package socket

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
)

// From <ws2ipdef.h>; the syscall package does not define them.
const (
	ipDontFragment = 14 // IP_DONTFRAGMENT
	ipv6DontFrag   = 14 // IPV6_DONTFRAG
	ipUnicastIf    = 31 // IP_UNICAST_IF
	ipv6UnicastIf  = 31 // IPV6_UNICAST_IF
)

// Windows binds by interface index.
const interfaceByIndex = true

// applyControl sets Interface and DontFragment. Windows applies IPv4-level
// options to IPv4 traffic on a dual-stack IPv6 socket, so an IPv6 socket
// gets both. IP_UNICAST_IF takes the index in network byte order,
// IPV6_UNICAST_IF in host order. RoutingMark is Linux-only;
// BindAddressNoPort runs without (D-18).
func applyControl(fd uintptr, opts Options, ipv6 bool, ifindex int) error {
	s := syscall.Handle(fd)
	if opts.RoutingMark != 0 {
		return &UnsupportedOptionError{Option: "RoutingMark (SO_MARK)", Platform: runtime.GOOS}
	}
	if opts.Interface != "" {
		if ipv6 {
			if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IPV6, ipv6UnicastIf, ifindex); err != nil {
				return os.NewSyscallError("setsockopt IPV6_UNICAST_IF", err)
			}
		}
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], uint32(ifindex))
		if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IP, ipUnicastIf, int(binary.NativeEndian.Uint32(be[:]))); err != nil {
			return os.NewSyscallError("setsockopt IP_UNICAST_IF", err)
		}
	}
	if opts.DontFragment {
		if ipv6 {
			if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IPV6, ipv6DontFrag, 1); err != nil {
				return os.NewSyscallError("setsockopt IPV6_DONTFRAG", err)
			}
		}
		if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IP, ipDontFragment, 1); err != nil {
			return os.NewSyscallError("setsockopt IP_DONTFRAGMENT", err)
		}
	}
	return nil
}
