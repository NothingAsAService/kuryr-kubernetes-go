package controller

import "time"

// recheckRequeue is how long to wait before reconciling again when a pass has
// made its own view of the world stale: a write that lost a race to another
// writer, an object that already existed, or one this pass has just deleted
// and means to replace.
//
// None of those is a failure, so returning an error would only log noise and
// pull in the rate limiter's exponential backoff. All the next pass needs is
// fresh data. The delay is short for that reason, and non-zero only to keep a
// contended object from spinning against the API server.
//
// The other requeue intervals in this package wait on something outside the
// cluster instead -- Neutron activating a port, Octavia finishing a build --
// and live beside the reconciler that waits on them.
const recheckRequeue = 100 * time.Millisecond
