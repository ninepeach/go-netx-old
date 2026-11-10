//go:build darwin

package socket

import (
	"net"
	"syscall"
)

// tcpFastOpenDarwin is TCP_FASTOPEN from <netinet/tcp.h> (macOS 10.11+),
// which the syscall package does not define. It enables server-side Fast
// Open on a listener; its value is a boolean, so Backlog is not used.
const tcpFastOpenDarwin = 0x105

var platformCapabilities = Capabilities{FastOpenListen: true}

// macOS accepts SO_REUSEPORT but does not balance load across the sockets:
// one of them receives everything. It is supported so the same
// configuration runs on a development Mac and a Linux server.
//
// TCP Fast Open is not set here: macOS refuses TCP_FASTOPEN with EINVAL
// before listen(2) (measured on macOS 26), so afterListen sets it.
func applyOptions(fd uintptr, opts Options) error {
	if opts.ReusePort {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1); err != nil {
			return err
		}
	}
	return nil
}

// afterListen enables Fast Open on the listening socket, the only state in
// which macOS accepts TCP_FASTOPEN.
func afterListen(ln net.Listener, opts Options) error {
	if !opts.FastOpen.Enabled {
		return nil
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil
	}
	raw, err := tl.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpFastOpenDarwin, 1)
	}); err != nil {
		return err
	}
	return fastOpenResult(setErr)
}
