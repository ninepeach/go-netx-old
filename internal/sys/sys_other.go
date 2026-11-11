//go:build !linux

package sys

import (
	"errors"
	"net/netip"
	"syscall"
)

// OOBSize is zero where kernel drop counts are unavailable.
const OOBSize = 0

// EnableDropCount is unsupported on this platform.
func EnableDropCount(syscall.RawConn) bool { return false }

// DropCount is unsupported on this platform.
func DropCount([]byte) (uint32, bool) { return 0, false }

// DestinationSupported is false: per-datagram destination addresses are
// read on Linux only.
const DestinationSupported = false

// EnableDestination is unsupported on this platform.
func EnableDestination(syscall.RawConn) error { return errors.ErrUnsupported }

// Destination is unsupported on this platform.
func Destination([]byte) (netip.AddrPort, bool) { return netip.AddrPort{}, false }
