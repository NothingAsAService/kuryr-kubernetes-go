package openstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// fakeNeutron is enough of the ports API to drive a read-modify-write.
//
// It enforces the If-Match precondition the way Neutron does, which is the
// whole point: the interesting behaviour is what happens when the port moves
// under a caller between the read and the write.
type fakeNeutron struct {
	mu sync.Mutex

	pairs    []AddressPair
	revision int

	// bumpBeforeWrite simulates another writer landing between our read and
	// our write. It is decremented each time, so a value of 1 makes exactly
	// the first write conflict.
	bumpBeforeWrite int

	gets int
	puts int
}

func (f *fakeNeutron) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			f.gets++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"port": f.portBody()})

		case http.MethodPut:
			f.puts++

			var body struct {
				Port struct {
					AllowedAddressPairs []AddressPair `json:"allowed_address_pairs"`
				} `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if f.bumpBeforeWrite > 0 {
				f.bumpBeforeWrite--
				f.revision++
			}

			if match := r.Header.Get("If-Match"); match != "" {
				want := strings.TrimPrefix(match, "revision_number=")
				if want != strconv.Itoa(f.revision) {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
			}

			f.pairs = body.Port.AllowedAddressPairs
			f.revision++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"port": f.portBody()})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (f *fakeNeutron) portBody() map[string]any {
	return map[string]any{
		"id":                    "parent-port",
		"network_id":            "net-1",
		"mac_address":           "fa:16:3e:00:00:01",
		"status":                PortStatusActive,
		"revision_number":       f.revision,
		"allowed_address_pairs": f.pairs,
		"fixed_ips":             []map[string]string{{"subnet_id": "sub-1", "ip_address": "10.0.0.5"}},
	}
}

func newTestClient(t *testing.T, f *fakeNeutron) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	provider := &gophercloud.ProviderClient{}
	provider.HTTPClient = *srv.Client()

	return &Client{
		Provider: provider,
		Network: &gophercloud.ServiceClient{
			ProviderClient: provider,
			Endpoint:       srv.URL + "/",
		},
	}
}

func TestUpdateAllowedAddressPairsAdds(t *testing.T) {
	f := &fakeNeutron{}
	c := newTestClient(t, f)

	want := AddressPair{IPAddress: "10.10.0.7", MACAddress: "fa:16:3e:aa:bb:cc"}
	err := c.UpdateAllowedAddressPairs(context.Background(), "parent-port",
		func(pairs []AddressPair) []AddressPair { return append(pairs, want) })
	if err != nil {
		t.Fatalf("UpdateAllowedAddressPairs: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pairs) != 1 || f.pairs[0] != want {
		t.Errorf("pairs = %v, want exactly %v", f.pairs, want)
	}
}

// A pod that is already allowed must not cost a write. Every pod on the node
// shares this port, so a needless write is a needless chance to conflict with
// one of the others.
func TestUpdateAllowedAddressPairsSkipsAWriteThatChangesNothing(t *testing.T) {
	existing := AddressPair{IPAddress: "10.10.0.7", MACAddress: "fa:16:3e:aa:bb:cc"}
	f := &fakeNeutron{pairs: []AddressPair{existing}}
	c := newTestClient(t, f)

	err := c.UpdateAllowedAddressPairs(context.Background(), "parent-port",
		func(pairs []AddressPair) []AddressPair { return pairs })
	if err != nil {
		t.Fatalf("UpdateAllowedAddressPairs: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 0 {
		t.Errorf("made %d writes for a no-op change, want 0", f.puts)
	}
}

// Removing the last pair has to send [], not null: Neutron reads null as
// "leave them alone", so a null would silently keep the address allowed.
func TestUpdateAllowedAddressPairsClearsTheLastOne(t *testing.T) {
	f := &fakeNeutron{pairs: []AddressPair{{IPAddress: "10.10.0.7", MACAddress: "fa:16:3e:aa:bb:cc"}}}
	c := newTestClient(t, f)

	err := c.UpdateAllowedAddressPairs(context.Background(), "parent-port",
		func([]AddressPair) []AddressPair { return nil })
	if err != nil {
		t.Fatalf("UpdateAllowedAddressPairs: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pairs) != 0 {
		t.Errorf("pairs = %v, want empty", f.pairs)
	}
}

// Two pods starting at once on one node both read the port, both add their own
// address and both write back. Without the revision guard the second write
// would drop the first pod's entry and cut its traffic off, with nothing in any
// log to say why.
func TestUpdateAllowedAddressPairsRetriesOnAConcurrentWrite(t *testing.T) {
	f := &fakeNeutron{bumpBeforeWrite: 1}
	c := newTestClient(t, f)

	want := AddressPair{IPAddress: "10.10.0.8", MACAddress: "fa:16:3e:dd:ee:ff"}
	err := c.UpdateAllowedAddressPairs(context.Background(), "parent-port",
		func(pairs []AddressPair) []AddressPair { return append(pairs, want) })
	if err != nil {
		t.Fatalf("UpdateAllowedAddressPairs should have retried past the conflict: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 2 {
		t.Errorf("made %d writes, want 2 (one rejected, one accepted)", f.puts)
	}
	if len(f.pairs) != 1 || f.pairs[0] != want {
		t.Errorf("pairs = %v, want %v", f.pairs, want)
	}
}

// A port being rewritten continuously is not something to retry forever: the
// caller is holding a CNI ADD open while this runs.
func TestUpdateAllowedAddressPairsGivesUpOnEndlessConflict(t *testing.T) {
	f := &fakeNeutron{bumpBeforeWrite: addressPairRetries + 5}
	c := newTestClient(t, f)

	err := c.UpdateAllowedAddressPairs(context.Background(), "parent-port",
		func(pairs []AddressPair) []AddressPair {
			return append(pairs, AddressPair{IPAddress: "10.10.0.9"})
		})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v, want it to wrap ErrRevisionConflict", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != addressPairRetries {
		t.Errorf("made %d attempts, want %d", f.puts, addressPairRetries)
	}
}
