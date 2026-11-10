package udp_test

import (
	"net"
	"testing"
	"time"
)

// Checks: UDP-04
func TestZZDiagZeroLengthWindows(t *testing.T) {
	try := func(name string, send func(cl *net.UDPConn, to *net.UDPAddr) error, recv func(s *net.UDPConn) (int, error)) {
		s, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		defer s.Close()
		cl, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		defer cl.Close()
		serr := send(cl, s.LocalAddr().(*net.UDPAddr))
		s.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, rerr := recv(s)
		t.Logf("%s: send err=%v recv n=%d err=%v", name, serr, n, rerr)
	}
	readFrom := func(s *net.UDPConn) (int, error) { n, _, err := s.ReadFromUDP(make([]byte, 100)); return n, err }
	readMsg := func(s *net.UDPConn) (int, error) {
		n, _, _, _, err := s.ReadMsgUDPAddrPort(make([]byte, 100), make([]byte, 64))
		return n, err
	}
	writeTo := func(cl *net.UDPConn, to *net.UDPAddr) error { _, err := cl.WriteToUDP(nil, to); return err }
	writeMsg := func(cl *net.UDPConn, to *net.UDPAddr) error { _, _, err := cl.WriteMsgUDP(nil, nil, to); return err }
	try("WriteTo/ReadFrom", writeTo, readFrom)
	try("WriteTo/ReadMsg", writeTo, readMsg)
	try("WriteMsg/ReadFrom", writeMsg, readFrom)
	try("WriteMsg/ReadMsg", writeMsg, readMsg)
	// connected Write(nil)
	s, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer s.Close()
	cl, _ := net.DialUDP("udp4", nil, s.LocalAddr().(*net.UDPAddr))
	defer cl.Close()
	_, werr := cl.Write(nil)
	s.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, rerr := readMsg(s)
	t.Logf("connected Write(nil)/ReadMsg: send err=%v recv n=%d err=%v", werr, n, rerr)
	t.Error("diagnostics")
}
