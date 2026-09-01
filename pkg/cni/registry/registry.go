// Package registry holds the CNI daemon's view of the pods on its node.
//
// This replaces the shared dictionary in kuryr_kubernetes/cni/daemon/service.py
// and the locking scattered through cni/handlers.py and
// cni/plugins/k8s_cni_registry.py.
//
// The Python daemon ran as several OS processes under cotyledon, so its
// registry had to be a multiprocessing.Manager().dict() guarded by on-disk
// locks, and every mutation had to replace the whole sub-dictionary because
// values in a Manager dict do not update in place. The Go daemon is one
// process with goroutines, so all of that disappears: a plain map under a
// mutex, and entries that can be mutated directly.
//
// The other thing that disappears is polling. The Python CNI ADD path waited
// on the registry with @retrying.retry(wait_fixed=1000), which meant up to a
// second of dead time on every pod start even when the data arrived
// immediately. Here waiters block on a broadcast channel and wake the instant
// the watcher writes.
package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// Errors the CNI server translates into specific exit codes for the runtime.
var (
	// ErrPodGone means the pod was deleted while we were waiting for its
	// ports. The runtime should stop asking.
	ErrPodGone = errors.New("pod was deleted before its ports were ready")

	// ErrTimeout means the data never arrived. Usually the controller is
	// down, wedged, or cannot reach Neutron.
	ErrTimeout = errors.New("timed out waiting for the controller")
)

// ErrUIDMismatch means the KuryrPort on record belongs to a different
// incarnation of a pod with the same name, which is routine with StatefulSets.
type ErrUIDMismatch struct {
	Name     string
	Expected types.UID
	Found    types.UID
}

func (e *ErrUIDMismatch) Error() string {
	return fmt.Sprintf("KuryrPort %s is for pod UID %s, but the runtime asked for %s",
		e.Name, e.Found, e.Expected)
}

// Entry is what the daemon knows about one pod.
type Entry struct {
	// PodUID identifies the incarnation these VIFs belong to.
	PodUID types.UID
	// Static marks a pod whose API mirror object carries a different UID
	// than the one the runtime reports, so the UID check must be skipped.
	Static bool
	// VIFs is the binding information, by interface name. Empty until the
	// controller has provisioned the ports.
	VIFs map[string]kuryrv1.VIF

	// ContainerID records which CNI ADD created the interfaces. A DEL
	// carrying a different id belongs to an older, failed ADD and must be
	// ignored, or it would unplug a running pod.
	ContainerID string

	// unplugged records that CNI DEL has finished tearing the interfaces down.
	unplugged bool
	// deleted records that the KuryrPort has gone from the API.
	deleted bool
	// podGone marks a pod that was deleted before any KuryrPort existed. A
	// waiter that sees this fails immediately instead of waiting out the
	// full timeout.
	podGone bool
}

// AllActive reports whether every VIF has reached ACTIVE in Neutron.
func (e *Entry) AllActive() bool {
	if len(e.VIFs) == 0 {
		return false
	}
	for _, v := range e.VIFs {
		if !v.Active {
			return false
		}
	}
	return true
}

func (e *Entry) clone() *Entry {
	out := *e
	if e.VIFs != nil {
		out.VIFs = make(map[string]kuryrv1.VIF, len(e.VIFs))
		for k, v := range e.VIFs {
			out.VIFs[k] = *v.DeepCopy()
		}
	}
	return &out
}

// Registry is the daemon's node-local state.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry

	// changed is closed and replaced on every mutation. A waiter takes a
	// reference to it before checking its predicate, so it cannot miss an
	// update that lands between the check and the wait.
	changed chan struct{}

	// keyed serialises the ADD and DEL paths for a single pod. Different
	// pods proceed in parallel.
	keyed keyedMutex
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		entries: map[string]*Entry{},
		changed: make(chan struct{}),
	}
}

// Key is the registry key for a pod: namespace/name, the same identity CNI
// passes in K8S_POD_NAMESPACE and K8S_POD_NAME.
func Key(namespace, name string) string { return namespace + "/" + name }

// Lock takes the per-pod lock and returns the release function. The CNI
// server holds it across a whole ADD or DEL so the two cannot interleave for
// one pod.
func (r *Registry) Lock(key string) func() { return r.keyed.lock(key) }

// notify must be called with the write lock held.
func (r *Registry) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// Upsert records the VIFs the controller has provisioned for a pod. It is
// driven by the KuryrPort watcher.
func (r *Registry) Upsert(key string, kp *kuryrv1.KuryrPort) {
	r.mu.Lock()
	defer r.mu.Unlock()

	vifs := make(map[string]kuryrv1.VIF, len(kp.Status.VIFs))
	for ifname, pv := range kp.Status.VIFs {
		vifs[ifname] = *pv.VIF.DeepCopy()
	}

	existing, ok := r.entries[key]
	if !ok || existing.PodUID != kp.Spec.PodUID || existing.podGone {
		// A new pod, or a new incarnation of one we already knew. Anything
		// recorded for the previous incarnation is meaningless now.
		r.entries[key] = &Entry{
			PodUID: kp.Spec.PodUID,
			Static: kp.Spec.PodStatic,
			VIFs:   vifs,
		}
		r.notify()
		return
	}

	existing.VIFs = vifs
	existing.Static = kp.Spec.PodStatic
	r.notify()
}

// MarkKuryrPortDeleted records that the KuryrPort has left the API.
//
// The entry is removed only once the interfaces are also down. Whichever of
// the watcher and the CNI DEL path finishes second does the removal, which is
// how the entry is deleted exactly once.
func (r *Registry) MarkKuryrPortDeleted(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok {
		return
	}
	if e.unplugged {
		delete(r.entries, key)
	} else {
		e.deleted = true
	}
	r.notify()
}

// MarkUnplugged records that CNI DEL has finished for a pod. It is the mirror
// of MarkKuryrPortDeleted.
func (r *Registry) MarkUnplugged(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok {
		return
	}
	if e.deleted {
		delete(r.entries, key)
	} else {
		e.unplugged = true
	}
	r.notify()
}

// MarkPodGone records that a pod was deleted without ever getting a
// KuryrPort. Without this a concurrent CNI ADD would wait out its whole
// timeout for data that is never coming.
func (r *Registry) MarkPodGone(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok {
		if len(e.VIFs) > 0 {
			// The controller did provision ports; this is an ordinary delete
			// and the normal teardown path applies.
			return
		}
		e.podGone = true
	} else {
		r.entries[key] = &Entry{podGone: true}
	}
	r.notify()
}

// Forget drops an entry unconditionally.
func (r *Registry) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
	r.notify()
}

// Get returns a copy of an entry.
func (r *Registry) Get(key string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok {
		return nil, false
	}
	return e.clone(), true
}

// SetContainerID records which ADD call owns the interfaces.
func (r *Registry) SetContainerID(key, containerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		e.ContainerID = containerID
		r.notify()
	}
}

// Len reports how many pods are tracked, for metrics and health.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// WaitForVIFs blocks until the controller has provisioned ports for a pod.
//
// podUID is checked so that a CNI ADD for a recreated pod does not adopt the
// ports of its predecessor. Static pods are exempt: their mirror object in
// the API carries a different UID than the one the runtime reports, so the
// check would never pass.
func (r *Registry) WaitForVIFs(ctx context.Context, key string, podUID types.UID) (*Entry, error) {
	return r.waitFor(ctx, key, func(e *Entry) (bool, error) {
		if e.podGone {
			return false, ErrPodGone
		}
		if len(e.VIFs) == 0 {
			return false, nil
		}
		if podUID != "" && !e.Static && e.PodUID != podUID {
			return false, &ErrUIDMismatch{Name: key, Expected: podUID, Found: e.PodUID}
		}
		return true, nil
	})
}

// WaitForActive blocks until every VIF for a pod is ACTIVE in Neutron.
//
// The daemon holds the CNI ADD open until this returns. Returning earlier
// would let the pod start on an interface whose dataplane is not programmed
// yet, which shows up as silent packet loss rather than an error.
func (r *Registry) WaitForActive(ctx context.Context, key string) (*Entry, error) {
	return r.waitFor(ctx, key, func(e *Entry) (bool, error) {
		if e.podGone {
			return false, ErrPodGone
		}
		return e.AllActive(), nil
	})
}

// waitFor blocks until pred is satisfied, ctx is done, or pred fails.
func (r *Registry) waitFor(ctx context.Context, key string, pred func(*Entry) (bool, error)) (*Entry, error) {
	for {
		// Take the change channel before reading, so an update landing
		// between the read and the select still wakes us.
		r.mu.RLock()
		changed := r.changed
		e, ok := r.entries[key]
		var snapshot *Entry
		if ok {
			snapshot = e.clone()
		}
		r.mu.RUnlock()

		if snapshot != nil {
			done, err := pred(snapshot)
			if err != nil {
				return nil, err
			}
			if done {
				return snapshot, nil
			}
		}

		select {
		case <-changed:
			// Something moved; re-evaluate.
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrTimeout
			}
			return nil, ctx.Err()
		}
	}
}

// keyedMutex serialises work per key without a global lock and without
// leaking a mutex per key that was ever used.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()

	return func() {
		l.mu.Unlock()

		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
