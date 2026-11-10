//go:build darwin

package udp

import (
	"net"
	"syscall"
)

// minSendBuffer is the send buffer a UDP socket needs on macOS to send a
// MaxDatagram datagram. macOS refuses a datagram larger than SO_SNDBUF
// with EMSGSIZE, and the default SO_SNDBUF is net.inet.udp.maxdgram, 9216
// bytes (measured on macOS 26), so without this WriteTo of anything over
// 9216 bytes fails on a Mac while it works on Linux.
const minSendBuffer = 64 << 10

// raiseSendBuffer raises SO_SNDBUF to minSendBuffer unless the caller set
// SendBuffer (which socket then verified) or the default is already larger.
func raiseSendBuffer(uc *net.UDPConn, sendBuffer int) error {
	if sendBuffer > 0 {
		return nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		cur, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
		if err != nil || cur >= minSendBuffer {
			opErr = err
			return
		}
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, minSendBuffer)
	}); err != nil {
		return err
	}
	return opErr
}
