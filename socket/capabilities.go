package socket

// Capabilities reports which performance options the current platform
// implements. An option it lacks runs without the optimisation instead of
// failing, so one configuration works on a development Mac and a Linux
// server (D-18); a kernel setting can also turn off an implemented one
// (Linux net.ipv4.tcp_fastopen, macOS net.inet.tcp.fastopen), which runs
// the same way. Options that change behaviour still fail when unsupported.
type Capabilities struct {
	// FastOpenListen: TCP Fast Open on listeners. Dialers never use Fast
	// Open (D-19).
	FastOpenListen bool
	// ReusePortBalanced: ReusePort spreads connections and datagrams
	// across the sockets. macOS binds but does not spread.
	ReusePortBalanced bool
	// BindAddressNoPort: Options.BindAddressNoPort defers the local port
	// to connect (Linux).
	BindAddressNoPort bool
}

// Supported returns the current platform's Capabilities.
func Supported() Capabilities { return platformCapabilities }
