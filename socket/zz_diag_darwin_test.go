package socket_test

import (
	"syscall"
	"testing"
)

func TestZZDiagFastOpenDarwin(t *testing.T) {
	mode, serr := syscall.SysctlUint32("net.inet.tcp.fastopen")
	t.Logf("sysctl net.inet.tcp.fastopen=%d err=%v", mode, serr)
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	t.Logf("before bind: %v", syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, 0x105, 1))
	syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	t.Logf("after bind: %v", syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, 0x105, 1))
	syscall.Listen(fd, 16)
	t.Logf("after listen: %v", syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, 0x105, 1))
	v, gerr := syscall.GetsockoptInt(fd, syscall.IPPROTO_TCP, 0x105)
	t.Logf("get after listen: %d %v", v, gerr)
	u, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	defer syscall.Close(u)
	sb, _ := syscall.GetsockoptInt(u, syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	md, merr := syscall.SysctlUint32("net.inet.udp.maxdgram")
	t.Logf("udp SO_SNDBUF default=%d maxdgram=%d %v", sb, md, merr)
	t.Logf("set SO_SNDBUF 65535+: %v", syscall.SetsockoptInt(u, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 65536))
	syscall.Bind(u, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	sa, _ := syscall.Getsockname(u)
	t.Logf("sendto 65507 after raising SO_SNDBUF: %v", syscall.Sendto(u, make([]byte, 65507), 0, sa))
}
