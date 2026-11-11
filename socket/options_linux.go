//go:build linux

package socket

import "syscall"

var platformCapabilities = Capabilities{FastOpenListen: true, ReusePortBalanced: true, BindAddressNoPort: true}

const (
	tcpFastOpen = 23
	soReusePort = 15
)

func applyOptions(fd uintptr, opts Options) error {
	if opts.ReusePort {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); err != nil {
			return err
		}
	}
	if !opts.FastOpen.Enabled {
		return nil
	}
	backlog := opts.FastOpen.Backlog
	if backlog <= 0 {
		backlog = 128
	}
	return fastOpenResult(syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpFastOpen, backlog))
}
