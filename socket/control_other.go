//go:build !linux && !darwin && !freebsd && !windows

package socket

import "runtime"

const interfaceByIndex = false

// applyControl: Interface, RoutingMark and DontFragment change behaviour
// and are refused here; BindAddressNoPort runs without (D-18).
func applyControl(_ uintptr, opts Options, _ bool, _ int) error {
	switch {
	case opts.Interface != "":
		return &UnsupportedOptionError{Option: "Interface", Platform: runtime.GOOS}
	case opts.RoutingMark != 0:
		return &UnsupportedOptionError{Option: "RoutingMark (SO_MARK)", Platform: runtime.GOOS}
	case opts.DontFragment:
		return &UnsupportedOptionError{Option: "DontFragment", Platform: runtime.GOOS}
	}
	return nil
}
