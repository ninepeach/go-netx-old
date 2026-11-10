//go:build !darwin

package socket

import "net"

// afterListen has nothing to do: every option is applied before bind.
func afterListen(net.Listener, Options) error { return nil }
