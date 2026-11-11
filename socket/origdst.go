package socket

import (
	"errors"
	"net/netip"
	"syscall"
)

// OriginalDestination returns the address a TCP connection was sent to
// before netfilter NAT (iptables or nftables REDIRECT, DNAT) changed it,
// read from conntrack with SO_ORIGINAL_DST (IPv4) or IP6T_SO_ORIGINAL_DST
// (IPv6). Pass the accepted connection, for example a *net.TCPConn or the
// Conn embedded in a *tcp.Conn. IPv4 clients of a dual-stack listener are
// looked up in IPv4 conntrack and reported unmapped. No privilege is
// needed.
//
// It fails when conntrack has no entry for the connection (ENOENT) or is
// not loaded (ENOPROTOOPT). Without NAT conntrack reports the address the
// connection already has. Connections diverted with TPROXY need no lookup:
// their LocalAddr is the original destination (see Options.Transparent).
// Linux only: other platforms return an UnsupportedOptionError (D-30).
func OriginalDestination(c syscall.Conn) (netip.AddrPort, error) {
	if isNil(c) {
		return netip.AddrPort{}, errors.New("socket: OriginalDestination: nil connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var ap netip.AddrPort
	var lookupErr error
	if err := raw.Control(func(fd uintptr) { ap, lookupErr = originalDst(fd) }); err != nil {
		return netip.AddrPort{}, err
	}
	if lookupErr != nil {
		return netip.AddrPort{}, lookupErr
	}
	return ap, nil
}
