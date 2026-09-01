package drivers

import "sync"

// VLAN id bounds. 0 is the untagged native VLAN and 4095 is reserved, which
// is where the hard ceiling of 4094 pods per node in nested-vlan mode comes
// from. It is a property of 802.1Q, not a tunable.
const (
	minVLANID = 1
	maxVLANID = 4094
)

// vlanAllocator hands out trunk segmentation ids, one independent pool per
// trunk.
//
// The Python implementation used kuryr-lib's segmentation driver, which kept
// a process-global set and deliberately never released an id whose subport
// had failed to attach, on the theory that leaking an id was safer than
// reusing one. That is defensible but it slowly starves a long-lived node.
// Here every failure path returns its ids explicitly, and the allocator is
// re-seeded from Neutron whenever a trunk is first seen, so the source of
// truth stays with Neutron rather than with controller memory.
type vlanAllocator struct {
	mu    sync.Mutex
	inUse map[string]map[int]bool // trunk id -> set of taken ids
	// next remembers where the last scan stopped per trunk, so allocating on
	// a busy node does not rescan from 1 every time.
	next map[string]int
}

func newVLANAllocator() *vlanAllocator {
	return &vlanAllocator{
		inUse: map[string]map[int]bool{},
		next:  map[string]int{},
	}
}

// seed records the ids a trunk already has attached. It is additive: ids the
// allocator handed out but Neutron has not yet reported stay reserved, which
// prevents a double allocation in the window between create and attach.
func (a *vlanAllocator) seed(trunkID string, used map[int]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	set, ok := a.inUse[trunkID]
	if !ok {
		set = map[int]bool{}
		a.inUse[trunkID] = set
	}
	for id := range used {
		set[id] = true
	}
}

// allocate reserves up to count free ids on a trunk. It may return fewer than
// requested, including none, when the trunk is close to full; callers are
// expected to cope rather than assume.
func (a *vlanAllocator) allocate(trunkID string, count int) []int {
	if count <= 0 {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	set, ok := a.inUse[trunkID]
	if !ok {
		set = map[int]bool{}
		a.inUse[trunkID] = set
	}

	start := a.next[trunkID]
	if start < minVLANID || start > maxVLANID {
		start = minVLANID
	}

	out := make([]int, 0, count)
	// Sweep the whole space once, wrapping, so ids freed below the cursor are
	// still found.
	id := start
	for scanned := 0; scanned < maxVLANID && len(out) < count; scanned++ {
		if !set[id] {
			set[id] = true
			out = append(out, id)
		}
		id++
		if id > maxVLANID {
			id = minVLANID
		}
	}
	a.next[trunkID] = id

	return out
}

// release returns ids to the free pool.
func (a *vlanAllocator) release(trunkID string, ids ...int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	set, ok := a.inUse[trunkID]
	if !ok {
		return
	}
	for _, id := range ids {
		delete(set, id)
	}
	if len(set) == 0 {
		delete(a.inUse, trunkID)
		delete(a.next, trunkID)
	}
}

// forget drops all state for a trunk, for use when a node disappears.
func (a *vlanAllocator) forget(trunkID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, trunkID)
	delete(a.next, trunkID)
}

// countInUse reports how many ids are reserved on a trunk.
func (a *vlanAllocator) countInUse(trunkID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inUse[trunkID])
}
