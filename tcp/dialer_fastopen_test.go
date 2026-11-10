package tcp_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
	"github.com/ninepeach/go-netx/tcp"
)

func isDialFastOpenError(err error) bool {
	var uoe *socket.UnsupportedOptionError
	return errors.As(err, &uoe) && uoe.Option == "TCP Fast Open" && uoe.Platform == "dialed connections"
}

// Fast Open on a dialer is a configuration error (D-19). Validate reports
// it at startup; a Dialer built anyway fails every dial at once, before
// resolving the address or opening a socket, and reports it through
// OnDial.
//
// Checks: DIAL-09
func TestDialerFastOpenFailsBeforeAnyNetworkWork(t *testing.T) {
	opts := tcp.DialOptions{Socket: socket.Options{FastOpen: socket.FastOpenOptions{Enabled: true}}}
	if err := opts.Validate(); !isDialFastOpenError(err) {
		t.Fatalf("Validate = %v, want UnsupportedOptionError for dialed connections", err)
	}
	var events []tcp.DialEvent
	opts.OnDial = func(e tcp.DialEvent) { events = append(events, e) }
	d := tcp.NewDialer(opts)

	// An unresolvable name: a DNS error would mean the dial got that far.
	if c, err := d.DialContext(context.Background(), "tcp", "netx-fastopen-check.invalid:80"); !isDialFastOpenError(err) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("dial = %v, want the Fast Open error before name resolution", err)
	}
	// A live listener: it must see no connection attempt.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String()); !isDialFastOpenError(err) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("dial = %v, want the Fast Open error", err)
	}
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if c, err := ln.Accept(); err == nil {
		c.Close()
		t.Fatal("the dialer connected although its options are invalid")
	}
	if len(events) != 2 || !isDialFastOpenError(events[0].Err) || !isDialFastOpenError(events[1].Err) ||
		events[1].Address != ln.Addr().String() {
		t.Fatalf("OnDial events = %+v", events)
	}
}

// Options a dialer can use pass Validate and dial normally.
//
// Checks: DIAL-10
func TestDialOptionsValidateAcceptsUsableOptions(t *testing.T) {
	for _, o := range []tcp.DialOptions{
		{},
		{Timeout: time.Second, KeepAlive: 10 * time.Second},
		{Socket: socket.Options{KeepAlive: socket.KeepAlive{Idle: 30 * time.Second, Interval: 5 * time.Second, Count: 3}}},
		{Socket: socket.Options{FastOpen: socket.FastOpenOptions{Backlog: 16}}}, // Backlog alone does not enable it
	} {
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v", o, err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := tcp.NewDialer(tcp.DialOptions{Timeout: 2 * time.Second}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
