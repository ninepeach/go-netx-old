//go:build freebsd

package socket

import (
	"os"
	"runtime"
	"syscall"
)

// FreeBSD has no per-socket interface binding.
const interfaceByIndex = false

// applyControl sets DontFragment with the option for the socket's family.
// Interface and RoutingMark have no FreeBSD equivalent (SO_SETFIB selects a
// routing table, which is a different thing); BindAddressNoPort runs
// without (D-18).
func applyControl(fd uintptr, opts Options, ipv6 bool, _ int) error {
	if opts.Interface != "" {
		return &UnsupportedOptionError{Option: "Interface", Platform: runtime.GOOS}
	}
	if opts.RoutingMark != 0 {
		return &UnsupportedOptionError{Option: "RoutingMark (SO_MARK)", Platform: runtime.GOOS}
	}
	if opts.DontFragment {
		level, name, label := syscall.IPPROTO_IP, syscall.IP_DONTFRAG, "IP_DONTFRAG"
		if ipv6 {
			level, name, label = syscall.IPPROTO_IPV6, syscall.IPV6_DONTFRAG, "IPV6_DONTFRAG"
		}
		if err := syscall.SetsockoptInt(int(fd), level, name, 1); err != nil {
			return os.NewSyscallError("setsockopt "+label, err)
		}
	}
	return nil
}
