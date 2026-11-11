//go:build linux

package socket

import (
	"os"
	"syscall"
)

// ipBindAddressNoPort is IP_BIND_ADDRESS_NO_PORT (Linux 4.2+, <linux/in.h>),
// which the syscall package does not define.
const ipBindAddressNoPort = 24

// Linux binds by name (SO_BINDTODEVICE), so no index lookup is needed.
const interfaceByIndex = false

// applyControl sets Interface, RoutingMark, DontFragment and
// BindAddressNoPort. Linux applies the IPv4-level options to IPv4-mapped
// traffic on an IPv6 socket, so a dual-stack socket gets both.
func applyControl(fd uintptr, opts Options, ipv6 bool, _ int) error {
	s := int(fd)
	if opts.Interface != "" {
		if err := syscall.BindToDevice(s, opts.Interface); err != nil {
			return os.NewSyscallError("setsockopt SO_BINDTODEVICE "+opts.Interface, err)
		}
	}
	if opts.RoutingMark != 0 {
		if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_MARK, int(opts.RoutingMark)); err != nil {
			return os.NewSyscallError("setsockopt SO_MARK", err)
		}
	}
	if opts.DontFragment {
		if ipv6 {
			if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IPV6, syscall.IPV6_MTU_DISCOVER, syscall.IPV6_PMTUDISC_DO); err != nil {
				return os.NewSyscallError("setsockopt IPV6_MTU_DISCOVER", err)
			}
		}
		if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_DO); err != nil {
			return os.NewSyscallError("setsockopt IP_MTU_DISCOVER", err)
		}
	}
	if opts.BindAddressNoPort {
		if err := syscall.SetsockoptInt(s, syscall.IPPROTO_IP, ipBindAddressNoPort, 1); err != nil {
			return os.NewSyscallError("setsockopt IP_BIND_ADDRESS_NO_PORT", err)
		}
	}
	return nil
}
