// Command baseline measures go-netx at gateway scale on one host: idle
// connection memory, request/response latency over TCP, and a client that
// stops reading. Client and server share the host, so latency includes
// both ends. Run it with `make baseline`; docs/PERFORMANCE.md records the
// results and the machine they came from.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/limit"
	"github.com/ninepeach/go-netx/stream"
	"github.com/ninepeach/go-netx/tcp"
)

func rssKB() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var v int64
			fmt.Sscanf(strings.TrimSpace(l[6:]), "%d", &v)
			return v
		}
	}
	return 0
}

type memSnap struct {
	heap, stack, sys uint64
	rss              int64
	g                int
}

func snap() memSnap {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return memSnap{m.HeapInuse, m.StackInuse, m.Sys, rssKB(), runtime.NumGoroutine()}
}

// idle: n connections, each server handler blocked in a read.
func idle(n int, mode string) {
	budget := limit.NewBudget(1 << 30)
	pool := buf.NewPool(budget)
	var ready sync.WaitGroup
	ready.Add(n)
	h := func(_ context.Context, c *tcp.Conn) {
		switch mode {
		case "stream":
			r := stream.NewReader(c, pool, stream.ReaderOptions{})
			defer r.Release()
			ready.Done()
			r.Peek(4)
		case "stream-after-10k", "stream-after-10k-shrink":
			// One 10KB frame, consumed, then idle: the case Shrink is for.
			r := stream.NewReader(c, pool, stream.ReaderOptions{})
			defer r.Release()
			if _, err := r.Peek(10 << 10); err != nil {
				ready.Done()
				return
			}
			r.Discard(10 << 10)
			if mode == "stream-after-10k-shrink" {
				r.Shrink()
			}
			ready.Done()
			r.Peek(4)
		case "bufio4k":
			r := bufio.NewReaderSize(c, 4096)
			ready.Done()
			r.Peek(4)
		case "raw":
			b := make([]byte, 4)
			ready.Done()
			c.Read(b)
		}
	}
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", h, tcp.Options{})
	if err != nil {
		panic(err)
	}
	go s.Serve(context.Background())
	before := snap()
	conns := make([]net.Conn, 0, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			panic(fmt.Sprintf("dial %d: %v", i, err))
		}
		if strings.HasPrefix(mode, "stream-after") {
			c.Write(make([]byte, 10<<10))
		}
		conns = append(conns, c)
	}
	ready.Wait()
	dialDur := time.Since(start)
	after := snap()
	per := func(a, b uint64) float64 { return float64(int64(a)-int64(b)) / float64(n) }
	fmt.Printf("idle mode=%-7s conns=%d connect=%v (%.0f conn/s) heap/conn=%.0fB stack/conn=%.0fB heap+stack/conn=%.0fB rss/conn(both ends)=%.0fB goroutines=%d pool=%dB\n",
		mode, n, dialDur.Round(time.Millisecond), float64(n)/dialDur.Seconds(),
		per(after.heap, before.heap), per(after.stack, before.stack),
		per(after.heap+after.stack, before.heap+before.stack),
		float64(after.rss-before.rss)*1024/float64(n), after.g, budget.Used())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t0 := time.Now()
	for _, c := range conns {
		c.Close()
	}
	err = s.Shutdown(ctx)
	cancel()
	fmt.Printf("  close %d conns + Shutdown: %v err=%v\n", n, time.Since(t0).Round(time.Millisecond), err)
}

// echo: c clients, each sending length-prefixed frames of size bytes and
// waiting for the echo, for dur. Server uses stream.Reader + Writer.
func echo(clients, size int, dur time.Duration) {
	pool := buf.NewPool(nil)
	h := func(_ context.Context, c *tcp.Conn) {
		r := stream.NewReader(c, pool, stream.ReaderOptions{})
		defer r.Release()
		w := stream.NewWriter(c)
		for {
			hdr, err := r.Peek(4)
			if err != nil {
				return
			}
			n := 4 + int(binary.BigEndian.Uint32(hdr))
			f, err := r.Peek(n)
			if err != nil {
				return
			}
			w.Add(pool.Copy(f))
			r.Discard(n)
			if r.Len() == 0 {
				if _, err := w.Flush(); err != nil {
					return
				}
			}
		}
	}
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", h, tcp.Options{})
	if err != nil {
		panic(err)
	}
	go s.Serve(context.Background())
	defer s.Close()
	frame := make([]byte, 4+size)
	binary.BigEndian.PutUint32(frame, uint32(size))
	var ops atomic.Int64
	lat := make([][]time.Duration, clients)
	var wg sync.WaitGroup
	deadline := time.Now().Add(dur)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", s.Addr().String())
			if err != nil {
				panic(err)
			}
			defer c.Close()
			in := make([]byte, len(frame))
			for time.Now().Before(deadline) {
				t := time.Now()
				c.Write(frame)
				if _, err := io.ReadFull(c, in); err != nil {
					panic(err)
				}
				lat[i] = append(lat[i], time.Since(t))
				ops.Add(1)
			}
		}(i)
	}
	wg.Wait()
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(a, b int) bool { return all[a] < all[b] })
	q := func(p float64) time.Duration { return all[int(p*float64(len(all)-1))] }
	fmt.Printf("echo clients=%d frame=%dB: %.0f req/s  p50=%v p99=%v p99.9=%v max=%v\n",
		clients, size, float64(ops.Load())/dur.Seconds(), q(.5).Round(time.Microsecond), q(.99).Round(time.Microsecond), q(.999).Round(time.Microsecond), all[len(all)-1].Round(time.Microsecond))
}

// slow: one client never reads while normal clients run request/response.
// The server pushes 256KB responses with a write deadline.
func slow(normal int, dur time.Duration) {
	budget := limit.NewBudget(1 << 30)
	pool := buf.NewPool(budget)
	payload := make([]byte, 256<<10)
	var slowErr atomic.Value
	var slowAt atomic.Int64
	h := func(_ context.Context, c *tcp.Conn) {
		r := stream.NewReader(c, pool, stream.ReaderOptions{})
		defer r.Release()
		w := stream.NewWriter(c)
		for {
			hdr, err := r.Peek(1)
			if err != nil {
				return
			}
			isSlow := hdr[0] == 'S'
			r.Discard(1)
			if isSlow {
				t := time.Now()
				for {
					c.SetWriteDeadline(time.Now().Add(2 * time.Second))
					b := pool.Get(len(payload))
					copy(b.B, payload)
					w.Add(b)
					if _, err := w.Flush(); err != nil {
						slowErr.Store(err.Error())
						slowAt.Store(int64(time.Since(t)))
						return
					}
				}
			}
			b := pool.Copy([]byte("ok"))
			w.Add(b)
			if _, err := w.Flush(); err != nil {
				return
			}
		}
	}
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", h, tcp.Options{})
	if err != nil {
		panic(err)
	}
	go s.Serve(context.Background())
	sc, _ := net.Dial("tcp", s.Addr().String())
	sc.Write([]byte("S")) // never reads
	var wg sync.WaitGroup
	var lat []time.Duration
	var mu sync.Mutex
	deadline := time.Now().Add(dur)
	for i := 0; i < normal; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := net.Dial("tcp", s.Addr().String())
			defer c.Close()
			in := make([]byte, 2)
			var l []time.Duration
			for time.Now().Before(deadline) {
				t := time.Now()
				c.Write([]byte("n"))
				if _, err := io.ReadFull(c, in); err != nil {
					panic(err)
				}
				l = append(l, time.Since(t))
			}
			mu.Lock()
			lat = append(lat, l...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	q := func(p float64) time.Duration { return lat[int(p*float64(len(lat)-1))] }
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("slow: 1 non-reading client + %d normal for %v: normal %.0f req/s p50=%v p99=%v; slow handler: %v after %v; pool in use now %dB\n",
		normal, dur, float64(len(lat))/dur.Seconds(), q(.5).Round(time.Microsecond), q(.99).Round(time.Microsecond), slowErr.Load(), time.Duration(slowAt.Load()).Round(time.Millisecond), budget.Used())

	// Shutdown while a handler is blocked writing to a non-reading client.
	sc2, _ := net.Dial("tcp", s.Addr().String())
	defer sc2.Close()
	sc2.Write([]byte("S"))
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t0 := time.Now()
	err = s.Shutdown(ctx)
	cancel()
	time.Sleep(50 * time.Millisecond)
	fmt.Printf("  Shutdown(500ms) with a handler blocked on a slow client: returned after %v (%v); pool in use after %dB\n",
		time.Since(t0).Round(time.Millisecond), err, budget.Used())
	sc.Close()
}

// runIdle runs one idle mode in a fresh process, so the modes do not see
// each other's freed memory.
func runIdle(n int, mode string) {
	cmd := exec.Command(os.Args[0], "-idle", strconv.Itoa(n), "-only", mode)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println("idle", mode, "failed:", err)
	}
}

func main() {
	n := flag.Int("idle", 9000, "idle connections")
	only := flag.String("only", "", "run one idle mode and exit")
	flag.Parse()
	if *only != "" {
		idle(*n, *only)
		return
	}
	fmt.Printf("go-netx baseline: %s/%s %d CPU, %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	for _, m := range []string{"raw", "stream", "bufio4k", "stream-after-10k", "stream-after-10k-shrink"} {
		runIdle(*n, m)
	}
	for _, c := range []int{1, 16, 128} {
		echo(c, 64, 5*time.Second)
	}
	echo(16, 4096, 5*time.Second)
	slow(16, 5*time.Second)
}
