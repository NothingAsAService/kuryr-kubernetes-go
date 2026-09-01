package drivers

import "errors"

// ErrRetryLater marks a condition that is expected, transient and nobody's
// fault: a namespace network still being built, a Neutron port not yet
// ACTIVE, a pool still recovering.
//
// The Python implementation expressed this as a ResourceNotReady exception
// that the Retry decorator caught by type. Here it is a sentinel that every
// such error wraps, so a reconciler asks one question instead of listing the
// error types it knows about, and a new one added later is handled without
// touching the reconcilers.
var ErrRetryLater = errors.New("not ready yet, retry later")
