//go:build !linux

package socket

import (
	"net/netip"
	"runtime"
)

func originalDst(uintptr) (netip.AddrPort, error) {
	return netip.AddrPort{}, &UnsupportedOptionError{Option: "OriginalDestination", Platform: runtime.GOOS}
}
