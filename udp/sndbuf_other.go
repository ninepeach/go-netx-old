//go:build !darwin

package udp

import "net"

// raiseSendBuffer is needed on macOS only; elsewhere the default send
// buffer already holds a MaxDatagram datagram.
func raiseSendBuffer(*net.UDPConn, int) error { return nil }
