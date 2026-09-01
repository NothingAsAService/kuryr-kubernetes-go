// Package e2e checks the CRDs against a real API server.
//
// What is worth testing here is exactly what a unit test cannot reach: whether
// the schemas controller-gen produced are the ones the API server enforces. A
// required field that stopped being required, or an enum that lost its values,
// still compiles and still passes every unit test -- and then accepts garbage
// on a live cluster.
//
// The suite deliberately pins itself to the kind-kuryr-e2e context rather than
// using whatever the current context happens to be. It creates and deletes
// objects, and the current context is not something a test should gamble on.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// kindContext is the cluster `make setup-test-e2e` creates.
const kindContext = "kind-kuryr-e2e"

var (
	k8s        client.Client
	discovery0 *discovery.DiscoveryClient
	testNS     = "kuryr-e2e"
)

func TestMain(m *testing.M) {
	cfg, err := restConfig()
	if err != nil {
		// No cluster: `go test ./...` must stay green for someone who has not
		// run `make setup-test-e2e`, so this is a skip rather than a failure.
		fmt.Printf("skipping e2e: %v\n", err)
		os.Exit(0)
	}

	scheme := runtime.NewScheme()
	if err := kuryrv1.AddToScheme(scheme); err != nil {
		fmt.Printf("building scheme: %v\n", err)
		os.Exit(1)
	}

	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Printf("building client: %v\n", err)
		os.Exit(1)
	}
	discovery0, err = discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		fmt.Printf("building discovery client: %v\n", err)
		os.Exit(1)
	}

	raw, err := client.New(cfg, client.Options{})
	if err != nil {
		fmt.Printf("building raw client: %v\n", err)
		os.Exit(1)
	}
	if err := installCRDs(context.Background(), raw); err != nil {
		fmt.Printf("installing CRDs: %v\n", err)
		os.Exit(1)
	}
	if err := ensureNamespace(context.Background(), raw); err != nil {
		fmt.Printf("creating namespace: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func restConfig() (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kindContext},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no %s context available (run 'make setup-test-e2e'): %w", kindContext, err)
	}
	return cfg, nil
}

// installCRDs applies config/crd/bases, which is what makes this a test of the
// manifests the project ships rather than of a schema built in memory.
func installCRDs(ctx context.Context, c client.Client) error {
	dir := filepath.Join("..", "..", "config", "crd", "bases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, obj); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := createOrReplace(ctx, c, obj); err != nil {
			return fmt.Errorf("applying %s: %w", e.Name(), err)
		}
		names = append(names, obj.GetName())
	}

	// A CRD is not usable the moment it is created; the API server has to
	// register the endpoint first, and a create against it before then fails
	// with a confusing NotFound.
	deadline := time.Now().Add(60 * time.Second)
	for _, name := range names {
		for {
			established, err := crdEstablished(ctx, c, name)
			if err != nil {
				return err
			}
			if established {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("CRD %s was not established in time", name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

// createOrReplace installs an object whether or not a previous run left one
// behind, so the suite is repeatable against the same cluster.
func createOrReplace(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	err := c.Create(ctx, obj.DeepCopy())
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		return err
	}
	// An update is rejected without the version it is replacing, which is how
	// the API server refuses to overwrite a concurrent change.
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

func crdEstablished(ctx context.Context, c client.Client, name string) (bool, error) {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		return false, err
	}

	conds, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil || !found {
		return false, nil
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Established" && cond["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

func ensureNamespace(ctx context.Context, c client.Client) error {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(testNS)
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// TestCRDsAreServed is the check every other test depends on: the four kinds
// the controllers watch have to exist before any of them can start.
func TestCRDsAreServed(t *testing.T) {
	gv := kuryrv1.GroupVersion.String()
	list, err := discovery0.ServerResourcesForGroupVersion(gv)
	if err != nil {
		t.Fatalf("the API server does not serve %s: %v", gv, err)
	}

	want := map[string]string{
		"KuryrPort":          "kp",
		"KuryrNetwork":       "kns",
		"KuryrNetworkPolicy": "knp",
		"KuryrLoadBalancer":  "klb",
	}
	seen := map[string][]string{}
	for _, r := range list.APIResources {
		// Subresources come back under the same Kind as their parent with no
		// short names of their own, so kuryrports/status would otherwise
		// overwrite kuryrports.
		if strings.Contains(r.Name, "/") {
			continue
		}
		seen[r.Kind] = r.ShortNames
	}

	for kind, short := range want {
		shortNames, ok := seen[kind]
		if !ok {
			t.Errorf("%s is not served", kind)
			continue
		}
		// The short name is what an operator types during an incident, so a
		// silent rename is worth catching.
		found := false
		for _, s := range shortNames {
			if s == short {
				found = true
			}
		}
		if !found {
			t.Errorf("%s short names are %v, want one of them to be %q", kind, shortNames, short)
		}
	}
}

// TestKuryrPortRoundTrips proves the schema accepts what the controller
// actually writes, status subresource included.
func TestKuryrPortRoundTrips(t *testing.T) {
	ctx := context.Background()
	kp := &kuryrv1.KuryrPort{
		ObjectMeta: metav1.ObjectMeta{Name: "roundtrip", Namespace: testNS},
		Spec: kuryrv1.KuryrPortSpec{
			PodUID:      types.UID("11111111-1111-1111-1111-111111111111"),
			PodNodeName: "worker-1",
		},
	}
	if err := k8s.Create(ctx, kp); err != nil {
		t.Fatalf("creating KuryrPort: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, kp) })

	kp.Status.VIFs = map[string]kuryrv1.PortVIF{
		kuryrv1.DefaultIfName: {
			Default: true,
			VIF: kuryrv1.VIF{
				Type:    kuryrv1.VIFTypeOVS,
				ID:      "22222222-2222-2222-2222-222222222222",
				Address: "fa:16:3e:00:00:01",
				Bridge:  "br-int",
				Active:  true,
				Network: kuryrv1.Network{
					ID: "33333333-3333-3333-3333-333333333333",
					Subnets: []kuryrv1.Subnet{{
						ID:   "44444444-4444-4444-4444-444444444444",
						CIDR: "10.0.0.0/24",
						IPs:  []string{"10.0.0.5"},
					}},
				},
			},
		},
	}
	if err := k8s.Status().Update(ctx, kp); err != nil {
		t.Fatalf("writing KuryrPort status: %v", err)
	}

	var back kuryrv1.KuryrPort
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(kp), &back); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if back.Spec.PodNodeName != "worker-1" {
		t.Errorf("podNodeName came back as %q", back.Spec.PodNodeName)
	}
	vif, ok := back.Status.VIFs[kuryrv1.DefaultIfName]
	if !ok {
		t.Fatalf("status.vifs lost the %s entry", kuryrv1.DefaultIfName)
	}
	if vif.VIF.Bridge != "br-int" || len(vif.VIF.Network.Subnets) != 1 {
		t.Errorf("the VIF did not survive the round trip: %+v", vif.VIF)
	}
}

// TestKuryrPortRequiresItsSpecFields is the negative half. Without it the
// round-trip test above would still pass on a schema that validates nothing.
func TestKuryrPortRequiresItsSpecFields(t *testing.T) {
	ctx := context.Background()

	// Typed structs always serialise every field, so an omission has to be
	// built by hand.
	kp := &unstructured.Unstructured{}
	kp.SetAPIVersion(kuryrv1.GroupVersion.String())
	kp.SetKind("KuryrPort")
	kp.SetName("missing-fields")
	kp.SetNamespace(testNS)
	if err := unstructured.SetNestedMap(kp.Object, map[string]any{"podUid": "abc"}, "spec"); err != nil {
		t.Fatalf("building the object: %v", err)
	}

	err := k8s.Create(ctx, kp)
	if err == nil {
		_ = k8s.Delete(ctx, kp)
		t.Fatal("the API server accepted a KuryrPort with no podNodeName")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("rejected with %v, want an Invalid error naming the missing field", err)
	}
}

// TestKuryrNetworkPolicyRejectsABadDirection checks an enum, which is the
// other kind of constraint the CRDs rely on.
func TestKuryrNetworkPolicyRejectsABadDirection(t *testing.T) {
	ctx := context.Background()

	knp := &unstructured.Unstructured{}
	knp.SetAPIVersion(kuryrv1.GroupVersion.String())
	knp.SetKind("KuryrNetworkPolicy")
	knp.SetName("bad-direction")
	knp.SetNamespace(testNS)
	rules := []any{map[string]any{
		"rule": map[string]any{
			"direction": "sideways",
			"etherType": "IPv4",
		},
	}}
	if err := unstructured.SetNestedSlice(knp.Object, rules, "spec", "ingressRules"); err != nil {
		t.Fatalf("building the object: %v", err)
	}

	err := k8s.Create(ctx, knp)
	if err == nil {
		_ = k8s.Delete(ctx, knp)
		t.Fatal("the API server accepted a rule with direction=sideways")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("rejected with %v, want an Invalid error naming the enum", err)
	}
}
