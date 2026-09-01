package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

const testKey = "default/web-0"

func kuryrPort(uid types.UID, active bool, mods ...func(*kuryrv1.KuryrPort)) *kuryrv1.KuryrPort {
	kp := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Spec:       kuryrv1.KuryrPortSpec{PodUID: uid, PodNodeName: "worker-1"},
		Status: kuryrv1.KuryrPortStatus{
			VIFs: map[string]kuryrv1.PortVIF{
				kuryrv1.DefaultIfName: {
					Default: true,
					VIF: kuryrv1.VIF{
						Type:    kuryrv1.VIFTypeOVS,
						ID:      "port-1",
						Address: "fa:16:3e:01:02:03",
						Bridge:  "br-int",
						Active:  active,
						Network: kuryrv1.Network{
							ID:      "net-1",
							Subnets: []kuryrv1.Subnet{{CIDR: "10.0.1.0/24", IPs: []string{"10.0.1.42"}}},
						},
					},
				},
			},
		},
	}
	for _, m := range mods {
		m(kp)
	}
	return kp
}

func TestUpsertAndGet(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", false))

	e, ok := r.Get(testKey)
	if !ok {
		t.Fatal("expected an entry after Upsert")
	}
	if e.PodUID != "uid-1" {
		t.Errorf("podUID = %q, want uid-1", e.PodUID)
	}
	if len(e.VIFs) != 1 {
		t.Fatalf("got %d VIFs, want 1", len(e.VIFs))
	}
	if e.AllActive() {
		t.Error("VIF is not active, so AllActive must be false")
	}
}

func TestGetReturnsACopy(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", false))

	e, _ := r.Get(testKey)
	e.VIFs[kuryrv1.DefaultIfName] = kuryrv1.VIF{ID: "tampered"}
	e.ContainerID = "tampered"

	fresh, _ := r.Get(testKey)
	if fresh.VIFs[kuryrv1.DefaultIfName].ID != "port-1" {
		t.Error("mutating a returned entry must not affect the registry")
	}
	if fresh.ContainerID == "tampered" {
		t.Error("mutating a returned entry must not affect the registry")
	}
}

func TestWaitForVIFsReturnsImmediatelyWhenPresent(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", false))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	e, err := r.WaitForVIFs(ctx, testKey, "uid-1")
	if err != nil {
		t.Fatalf("WaitForVIFs: %v", err)
	}
	if len(e.VIFs) != 1 {
		t.Errorf("got %d VIFs, want 1", len(e.VIFs))
	}
}

func TestWaitForVIFsWakesOnUpsert(t *testing.T) {
	r := New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.WaitForVIFs(ctx, testKey, "uid-1")
		done <- err
	}()

	// Give the waiter a moment to block, then deliver the data.
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	r.Upsert(testKey, kuryrPort("uid-1", false))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForVIFs: %v", err)
		}
		// The Python implementation polled once a second here. The point of
		// the broadcast channel is that the wakeup is immediate.
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("waiter took %v to wake; it should be near-instant", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke after Upsert")
	}
}

func TestWaitForVIFsTimesOut(t *testing.T) {
	r := New()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := r.WaitForVIFs(ctx, testKey, "uid-1")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestWaitForVIFsFailsFastWhenPodGone(t *testing.T) {
	r := New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.WaitForVIFs(ctx, testKey, "uid-1")
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	r.MarkPodGone(testKey)

	select {
	case err := <-done:
		if !errors.Is(err, ErrPodGone) {
			t.Fatalf("err = %v, want ErrPodGone", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a deleted pod should fail the waiter immediately, not wait out the timeout")
	}
}

func TestWaitForVIFsRejectsUIDMismatch(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-old", false))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := r.WaitForVIFs(ctx, testKey, "uid-new")
	var mismatch *ErrUIDMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ErrUIDMismatch", err)
	}
	if mismatch.Expected != "uid-new" || mismatch.Found != "uid-old" {
		t.Errorf("mismatch = %+v, want expected uid-new found uid-old", mismatch)
	}
}

func TestWaitForVIFsAllowsStaticPodUIDMismatch(t *testing.T) {
	// A static pod's mirror object in the API has a different UID than the
	// one the runtime reports, so the check has to be skipped for it.
	r := New()
	r.Upsert(testKey, kuryrPort("mirror-uid", false, func(kp *kuryrv1.KuryrPort) {
		kp.Spec.PodStatic = true
	}))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := r.WaitForVIFs(ctx, testKey, "runtime-uid"); err != nil {
		t.Fatalf("a static pod must not fail the UID check: %v", err)
	}
}

func TestWaitForActive(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", false))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.WaitForActive(ctx, testKey)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("waiter returned early with %v; the VIF is not active yet", err)
	default:
	}

	// The controller flips the port to ACTIVE.
	r.Upsert(testKey, kuryrPort("uid-1", true))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForActive: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke when the VIF became active")
	}
}

func TestUpsertReplacesOnNewIncarnation(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", true))
	r.SetContainerID(testKey, "container-1")

	// A StatefulSet pod comes back with the same name and a new UID.
	r.Upsert(testKey, kuryrPort("uid-2", false))

	e, _ := r.Get(testKey)
	if e.PodUID != "uid-2" {
		t.Errorf("podUID = %q, want uid-2", e.PodUID)
	}
	if e.ContainerID != "" {
		t.Errorf("containerID = %q; state from the previous pod must not carry over", e.ContainerID)
	}
	if e.AllActive() {
		t.Error("the new incarnation starts inactive")
	}
}

func TestEntryDeletedExactlyOnceWatcherLast(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", true))

	// CNI DEL finishes first.
	r.MarkUnplugged(testKey)
	if _, ok := r.Get(testKey); !ok {
		t.Fatal("entry removed before the KuryrPort was deleted; the watcher would lose track")
	}

	// Then the KuryrPort disappears.
	r.MarkKuryrPortDeleted(testKey)
	if _, ok := r.Get(testKey); ok {
		t.Fatal("entry should be gone once both sides are done")
	}
}

func TestEntryDeletedExactlyOnceCNILast(t *testing.T) {
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", true))

	// The KuryrPort disappears first.
	r.MarkKuryrPortDeleted(testKey)
	if _, ok := r.Get(testKey); !ok {
		t.Fatal("entry removed before the interfaces were torn down; DEL would have nothing to work from")
	}

	r.MarkUnplugged(testKey)
	if _, ok := r.Get(testKey); ok {
		t.Fatal("entry should be gone once both sides are done")
	}
}

func TestMarkPodGoneDoesNotClobberProvisionedPod(t *testing.T) {
	// A pod being deleted normally has VIFs; the sentinel is only for the
	// case where the controller never got as far as provisioning.
	r := New()
	r.Upsert(testKey, kuryrPort("uid-1", true))
	r.MarkPodGone(testKey)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.WaitForVIFs(ctx, testKey, "uid-1"); err != nil {
		t.Fatalf("a provisioned pod must not be turned into a gone sentinel: %v", err)
	}
}

func TestKeyedLockSerialisesSameKey(t *testing.T) {
	r := New()

	var (
		mu      sync.Mutex
		active  int
		maxSeen int
	)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := r.Lock(testKey)
			defer unlock()

			mu.Lock()
			active++
			if active > maxSeen {
				maxSeen = active
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Errorf("saw %d concurrent holders of the same key lock, want 1", maxSeen)
	}
}

func TestKeyedLockAllowsDifferentKeys(t *testing.T) {
	r := New()

	first := r.Lock("default/a")
	defer first()

	done := make(chan struct{})
	go func() {
		unlock := r.Lock("default/b")
		unlock()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("locking a different key blocked; pods on a node must proceed in parallel")
	}
}

func TestKeyedLockReleasesMemory(t *testing.T) {
	r := New()
	for i := 0; i < 100; i++ {
		unlock := r.Lock(testKey)
		unlock()
	}
	r.keyed.mu.Lock()
	n := len(r.keyed.locks)
	r.keyed.mu.Unlock()
	if n != 0 {
		t.Errorf("%d key locks retained after release, want 0", n)
	}
}

func TestConcurrentMutationIsRaceFree(t *testing.T) {
	r := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Upsert(testKey, kuryrPort("uid-1", j%2 == 0))
				r.SetContainerID(testKey, "c")
				r.Get(testKey)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = r.WaitForActive(ctx, testKey)
	}()
	wg.Wait()
}
