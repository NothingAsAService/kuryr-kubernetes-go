package drivers

import (
	"sync"
	"testing"
)

func TestVLANAllocatorHandsOutDistinctIDs(t *testing.T) {
	a := newVLANAllocator()

	ids := a.allocate("trunk", 5)
	if len(ids) != 5 {
		t.Fatalf("got %d ids, want 5", len(ids))
	}
	seen := map[int]bool{}
	for _, id := range ids {
		if id < minVLANID || id > maxVLANID {
			t.Errorf("id %d out of range", id)
		}
		if seen[id] {
			t.Errorf("id %d handed out twice", id)
		}
		seen[id] = true
	}
}

func TestVLANAllocatorRespectsSeed(t *testing.T) {
	a := newVLANAllocator()
	a.seed("trunk", map[int]bool{1: true, 2: true, 3: true})

	ids := a.allocate("trunk", 2)
	for _, id := range ids {
		if id <= 3 {
			t.Errorf("allocated id %d, which was seeded as in use", id)
		}
	}
}

func TestVLANAllocatorReleaseMakesIDsReusable(t *testing.T) {
	a := newVLANAllocator()

	first := a.allocate("trunk", 3)
	a.release("trunk", first...)
	if got := a.countInUse("trunk"); got != 0 {
		t.Fatalf("after release %d ids still in use, want 0", got)
	}

	// The cursor has moved past the released ids, so reallocation must wrap
	// around to find them again.
	second := a.allocate("trunk", 3)
	if len(second) != 3 {
		t.Fatalf("got %d ids after release, want 3", len(second))
	}
}

func TestVLANAllocatorExhaustion(t *testing.T) {
	a := newVLANAllocator()

	all := a.allocate("trunk", maxVLANID)
	if len(all) != maxVLANID {
		t.Fatalf("got %d ids, want the full %d", len(all), maxVLANID)
	}
	// A full trunk must report failure by returning nothing, not by blocking
	// or by handing out a duplicate.
	if extra := a.allocate("trunk", 1); len(extra) != 0 {
		t.Errorf("allocated %v from a full trunk", extra)
	}

	a.release("trunk", all[10])
	again := a.allocate("trunk", 1)
	if len(again) != 1 || again[0] != all[10] {
		t.Errorf("after freeing %d got %v, want that id back", all[10], again)
	}
}

func TestVLANAllocatorTrunksAreIndependent(t *testing.T) {
	a := newVLANAllocator()

	first := a.allocate("trunk-a", 3)
	second := a.allocate("trunk-b", 3)

	// Two different nodes may legitimately use the same VLAN ids; the tags
	// are local to each trunk.
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("trunk-a got %v and trunk-b got %v; ids should be independent per trunk",
				first, second)
			break
		}
	}
}

func TestVLANAllocatorIsConcurrencySafe(t *testing.T) {
	a := newVLANAllocator()

	const goroutines = 32
	const perGoroutine = 20

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got []int
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := a.allocate("trunk", perGoroutine)
			mu.Lock()
			got = append(got, ids...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(got) != goroutines*perGoroutine {
		t.Fatalf("got %d ids total, want %d", len(got), goroutines*perGoroutine)
	}
	seen := map[int]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("id %d was handed to two goroutines", id)
		}
		seen[id] = true
	}
}

func TestVLANAllocatorForget(t *testing.T) {
	a := newVLANAllocator()
	a.allocate("trunk", 10)
	a.forget("trunk")
	if got := a.countInUse("trunk"); got != 0 {
		t.Errorf("after forget %d ids still tracked, want 0", got)
	}
}
