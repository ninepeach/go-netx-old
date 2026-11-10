package socket_test

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
)

// One listener configuration with Fast Open works on every platform: where
// Fast Open is unavailable the listener runs without it.
// Checks: SOCK-06
func TestFastOpenConfigurationRunsEverywhere(t *testing.T) {
	opts := socket.Options{FastOpen: socket.FastOpenOptions{Enabled: true, Backlog: 16}}
	ln, err := socket.Listen(context.Background(), "tcp", "127.0.0.1:0", opts)
	if err != nil {
		t.Fatalf("Listen with Fast Open on %s: %v", runtime.GOOS, err)
	}
	defer ln.Close()
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
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ok" {
		t.Fatalf("read %q, %v", b, err)
	}
}

// Checks: SOCK-27
func TestSupportedMatchesPlatform(t *testing.T) {
	want := map[string]socket.Capabilities{
		"linux":   {FastOpenListen: true, ReusePortBalanced: true},
		"darwin":  {FastOpenListen: true},
		"freebsd": {ReusePortBalanced: true},
	}[runtime.GOOS]
	if got := socket.Supported(); got != want {
		t.Fatalf("Supported() on %s = %+v, want %+v", runtime.GOOS, got, want)
	}
}

// TCP-only options on UDP are a configuration error on every platform.
// Checks: SOCK-18
func TestTCPOnlyOptionsOnUDPAreRefused(t *testing.T) {
	for _, o := range []socket.Options{
		{FastOpen: socket.FastOpenOptions{Enabled: true}},
		{KeepAlive: socket.KeepAlive{Idle: 1}},
		{KeepAlive: socket.KeepAlive{Disable: true}},
	} {
		lc := socket.ListenConfig(o)
		pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
		if err == nil {
			pc.Close()
			t.Fatalf("ListenPacket %+v: silently ignored", o)
		}
		if !socket.IsUnsupported(err) {
			t.Fatalf("ListenPacket %+v: %v", o, err)
		}
		d := socket.Dialer(net.Dialer{}, o)
		c, err := d.DialContext(context.Background(), "udp", "127.0.0.1:9")
		if err == nil {
			c.Close()
			t.Fatalf("Dial %+v: silently ignored", o)
		}
		if !socket.IsUnsupported(err) {
			t.Fatalf("Dial %+v: %v", o, err)
		}
	}
	// Options that do apply to UDP still work.
	lc := socket.ListenConfig(socket.Options{RecvBuffer: 1 << 16})
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc.Close()
}

// Checks: SOCK-28
func TestDialerRefusesFastOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := socket.Dialer(net.Dialer{}, socket.Options{FastOpen: socket.FastOpenOptions{Enabled: true}})
	c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		c.Close()
		t.Fatal("Dial with Fast Open succeeded; a dead peer could look connected")
	}
	var uoe *socket.UnsupportedOptionError
	if !errors.As(err, &uoe) || uoe.Option != "TCP Fast Open" || uoe.Platform != "dialed connections" {
		t.Fatalf("err = %v, want UnsupportedOptionError for dialed connections", err)
	}
}

// Checks: SOCK-29
func TestDialerKeepsCallerControl(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	// An option go-netx refuses on dial shows that its control ran after
	// the caller's.
	refused := socket.Options{FastOpen: socket.FastOpenOptions{Enabled: true}}

	var calls []string
	base := net.Dialer{
		Control: func(string, string, syscall.RawConn) error { calls = append(calls, "Control"); return nil },
		ControlContext: func(context.Context, string, string, syscall.RawConn) error {
			calls = append(calls, "ControlContext")
			return nil
		},
	}
	d := socket.Dialer(base, refused)
	if _, err := d.DialContext(context.Background(), "tcp", addr); !socket.IsUnsupported(err) {
		t.Fatalf("options skipped when ControlContext is set: %v", err)
	}
	if len(calls) != 1 || calls[0] != "ControlContext" {
		t.Fatalf("calls %v, want only ControlContext (net ignores Control when both are set)", calls)
	}
	if base.Control == nil || base.ControlContext == nil {
		t.Fatal("base dialer was modified")
	}

	calls = nil
	d = socket.Dialer(net.Dialer{Control: base.Control}, refused)
	if _, err := d.DialContext(context.Background(), "tcp", addr); !socket.IsUnsupported(err) || len(calls) != 1 || calls[0] != "Control" {
		t.Fatalf("Control only: err %v calls %v", err, calls)
	}

	// The caller's error stops the dial before the options are applied.
	mine := errors.New("caller refused")
	d = socket.Dialer(net.Dialer{Control: func(string, string, syscall.RawConn) error { return mine }}, refused)
	if _, err := d.DialContext(context.Background(), "tcp", addr); !errors.Is(err, mine) {
		t.Fatalf("caller error: %v", err)
	}

	// Both run and the dial succeeds when nothing refuses.
	calls = nil
	d = socket.Dialer(net.Dialer{ControlContext: base.ControlContext}, socket.Options{KeepAlive: socket.KeepAlive{Idle: time.Minute}})
	c, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil || len(calls) != 1 {
		t.Fatalf("dial: %v calls %v", err, calls)
	}
	c.Close()
}

// Checks: SOCK-30
func TestSetBuffersRejectsNil(t *testing.T) {
	var typed *net.UDPConn
	for name, c := range map[string]socket.BufferSetter{"nil": nil, "typed nil": typed} {
		for _, opts := range []socket.Options{{}, {RecvBuffer: 4096}, {SendBuffer: 4096}} {
			if err := socket.SetBuffers(c, opts); err == nil {
				t.Fatalf("SetBuffers(%s, %+v) = nil, want an error", name, opts)
			}
		}
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := socket.SetBuffers(c, socket.Options{RecvBuffer: 4096}); err != nil {
		t.Fatalf("real connection: %v", err)
	}
}
