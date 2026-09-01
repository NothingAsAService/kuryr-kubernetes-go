//go:build !linux

package binding

import (
	"errors"
	"runtime"
)

// ErrUnsupportedPlatform is returned by every binding operation off Linux.
//
// Kuryr binds pod interfaces with netlink and network namespaces, neither of
// which exists elsewhere. These stubs are here so the daemon, the CNI plugin
// and the whole test suite still compile on a developer machine; the shipped
// binaries are built for linux.
var ErrUnsupportedPlatform = errors.New("kuryr binding requires Linux, not " + runtime.GOOS)

// ConfigureL3 is unavailable off Linux.
func ConfigureL3(Request) error { return ErrUnsupportedPlatform }

// ConfigureL3With is unavailable off Linux.
func ConfigureL3With(Request, Options) error { return ErrUnsupportedPlatform }
