//go:build linux

package sys

import (
	"syscall"
	"testing"
)

// DropCount parses kernel control data, so any byte sequence must be safe:
// no panic, no out-of-bounds read, and a reported count must come from a
// well-formed SO_RXQ_OVFL message inside the buffer.
//
// Checks: NF-07
func FuzzDropCount(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3})
	f.Add(cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{7, 0, 0, 0}))
	f.Add(append(cmsg(syscall.SOL_SOCKET, syscall.SCM_TIMESTAMP, make([]byte, 16)),
		cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{1, 2, 3, 4})...))
	f.Fuzz(func(t *testing.T, oob []byte) {
		// Copy into an 8-aligned buffer, as the kernel's would be.
		aligned := make([]byte, len(oob))
		copy(aligned, oob)
		n, ok := DropCount(aligned)
		if !ok && n != 0 {
			t.Fatalf("not found but n=%d", n)
		}
	})
}

// Destination parses the same kernel control data: any byte sequence is
// safe, and a reported address is a valid, unmapped one taken from a
// well-formed IP_ORIGDSTADDR or IPV6_ORIGDSTADDR message.
//
// Checks: NF-07
func FuzzDestination(f *testing.F) {
	f.Add([]byte{})
	f.Add(cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddr4([4]byte{198, 18, 0, 1}, 443)))
	f.Add(cmsg(syscall.SOL_IPV6, ipv6OrigDstAddr, sockaddr6([16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}, 80)))
	f.Add(append(cmsg(syscall.SOL_SOCKET, soRxqOvfl, []byte{7, 0, 0, 0}),
		cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddr4([4]byte{1, 2, 3, 4}, 5))...))
	f.Fuzz(func(t *testing.T, oob []byte) {
		aligned := make([]byte, len(oob))
		copy(aligned, oob)
		ap, ok := Destination(aligned)
		switch {
		case !ok && ap.IsValid():
			t.Fatalf("not found but returned %v", ap)
		case ok && (!ap.IsValid() || ap.Addr().Is4In6() || ap.Addr().Zone() != ""):
			t.Fatalf("found an invalid or mapped address %v", ap)
		}
	})
}
