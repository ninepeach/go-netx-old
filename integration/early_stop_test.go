package integration_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ninepeach/go-netx"
	"github.com/ninepeach/go-netx/tcp"
	"github.com/ninepeach/go-netx/udp"
)

// serveAfterShutdown makes Serve wait until Shutdown has returned, so the
// stop deterministically lands before the transport's Serve starts: the
// order a SIGTERM during OnStart produces, without relying on scheduling.
type serveAfterShutdown struct {
	netx.Service
	once sync.Once
	shut chan struct{}
}

func (s *serveAfterShutdown) Serve(ctx context.Context) error {
	<-s.shut
	return s.Service.Serve(ctx)
}

func (s *serveAfterShutdown) Shutdown(ctx context.Context) error {
	err := s.Service.Shutdown(ctx)
	s.once.Do(func() { close(s.shut) })
	return err
}

// A stop requested while OnStart runs, before any transport's Serve has
// started, is a clean exit: LoopContext returns nil and OnError is not
// called. Before the fix it returned "tcp: closed" / "udp: closed" (REG-34).
//
// Checks: INT-10
func TestStopBeforeServeIsCleanExit(t *testing.T) {
	services := map[string]func(t *testing.T) netx.Service{
		"tcp": func(t *testing.T) netx.Service {
			s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(context.Context, *tcp.Conn) {}, tcp.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"udp": func(t *testing.T) netx.Service {
			c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", func(*udp.Conn, *udp.Packet) {}, udp.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
	}
	services["group"] = func(t *testing.T) netx.Service {
		return netx.Group(services["tcp"](t), services["udp"](t))
	}
	for name, newService := range services {
		for _, forced := range []bool{false, true} {
			for i := 0; i < 50; i++ {
				svc := newService(t)
				if forced {
					svc = &serveAfterShutdown{Service: svc, shut: make(chan struct{})}
				}
				srv := netx.NewServer(svc)
				var reported error
				srv.OnError = func(err error) { reported = err }
				srv.OnStart = func(context.Context) error { return srv.Stop() }
				if err := srv.LoopContext(context.Background()); err != nil || reported != nil {
					t.Fatalf("%s (Serve forced after Shutdown: %v), run %d: LoopContext = %v, OnError got %v", name, forced, i, err, reported)
				}
			}
		}
	}
}
