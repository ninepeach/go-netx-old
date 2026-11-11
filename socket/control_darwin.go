//go:build darwin

package socket

import (
	"os"
	"runtime"
	"syscall"
)

// From <netinet/in.h> and <netinet6/in6.h>; the syscall package does not
// define them.
const (
	ipDontFrag   = 28 // IP_DONTFRAG
	ipv6DontFrag = 62 // IPV6_DONTFRAG
)

// macOS binds by interface index.
const interfaceByIndex = true

// applyControl sets Interface and DontFragment with the option for the
// socket's family (macOS refuses IPv4-level options on an IPv6 socket).
// RoutingMark is Linux-only; BindAddressNoPort runs without (D-18).
func applyControl(fd uintptr, opts Options, ipv6 bool, ifindex int) error {
	s := int(fd)
	if opts.RoutingMark != 0 {
		return &UnsupportedOptionError{Option: "RoutingMark (SO_MARK)", Platform: runtime.GOOS}
	}
	if opts.Interface != "" {
		level, name, label := syscall.IPPROTO_IP, syscall.IP_BOUND_IF, "IP_BOUND_IF"
		if ipv6 {
			level, name, label = syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, "IPV6_BOUND_IF"
		}
		if err := syscall.SetsockoptInt(s, level, name, ifindex); err != nil {
			return os.NewSyscallError("setsockopt "+label, err)
		}
	}
	if opts.DontFragment {
		level, name, label := syscall.IPPROTO_IP, ipDontFrag, "IP_DONTFRAG"
		if ipv6 {
			level, name, label = syscall.IPPROTO_IPV6, ipv6DontFrag, "IPV6_DONTFRAG"
		}
		if err := syscall.SetsockoptInt(s, level, name, 1); err != nil {
			return os.NewSyscallError("setsockopt "+label, err)
		}
	}
	return nil
}
