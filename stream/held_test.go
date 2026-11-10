package stream

import "github.com/ninepeach/go-netx/buf"

// heldSince returns a function reporting how many buffers obtained since
// this call are still unreleased. buf counts outstanding buffers only in
// netxdebug builds (make race runs them), so in other builds it reports 0
// and checks that something is held must be guarded with buf.Debug.
func heldSince() func() int64 {
	before := buf.Outstanding()
	return func() int64 { return buf.Outstanding() - before }
}
