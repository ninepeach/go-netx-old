package socket_test

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

// Checks: SOCK-26
func TestFastOpenOnDarwin(t *testing.T) {
	opts := socket.Options{FastOpen: socket.FastOpenOptions{Enabled: true}}
	ln, err := socket.Listen(context.Background(), "tcp", "127.0.0.1:0", opts)
	if err != nil {
		t.Fatal(err) // never an error: without kernel support it runs without
	}
	defer ln.Close()

	// ListenConfig alone cannot set it (macOS needs a listening socket),
	// but must not fail either.
	lc := socket.ListenConfig(opts)
	if l2, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0"); err != nil {
		t.Fatal("ListenConfig with Fast Open:", err)
	} else {
		l2.Close()
	}

	// net.inet.tcp.fastopen: bit 1 client, bit 2 server.
	mode, serr := syscall.SysctlUint32("net.inet.tcp.fastopen")
	t.Logf("net.inet.tcp.fastopen = %d (%v)", mode, serr)
	if serr == nil && mode&2 != 0 {
		raw, _ := ln.(*net.TCPListener).SyscallConn()
		var v int
		var gerr error
		raw.Control(func(fd uintptr) { v, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, 0x105) })
		if gerr != nil || v == 0 {
			t.Fatalf("server Fast Open enabled in the kernel, but TCP_FASTOPEN on the listener = %d, %v", v, gerr)
		}
	}

	// Either way the listener serves.
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := make([]byte, 2)
	if n, err := c.Read(b); err != nil || string(b[:n]) != "ok" {
		t.Fatalf("read %q, %v", b[:n], err)
	}
}
