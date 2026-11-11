//go:build !linux

package socket_test

import (
	"net/netip"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

// checkOriginalDst: only Linux has SO_ORIGINAL_DST (D-30).
func checkOriginalDst(t *testing.T, got netip.AddrPort, err error, _ netip.AddrPort) {
	t.Helper()
	if !socket.IsUnsupported(err) || got.IsValid() {
		t.Fatalf("OriginalDestination = %v, %v; want UnsupportedOptionError", got, err)
	}
}
