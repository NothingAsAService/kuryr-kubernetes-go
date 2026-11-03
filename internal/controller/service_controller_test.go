package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/openstack"
	"opendev.org/openstack/kuryr-kubernetes/pkg/vif"
)

// The service reconciler only needs three decisions from its drivers, so
// fixed answers are enough to exercise everything it does with them.
type fixedProject struct{ id string }

func (f fixedProject) ProjectFor(context.Context, client.Object) (string, error) {
	return f.id, nil
}

type fixedSubnets struct{ subnets vif.SubnetSet }

func (f fixedSubnets) SubnetsFor(context.Context, client.Object, string) (vif.SubnetSet, error) {
	return f.subnets, nil
}

type fixedSGs struct{ ids []string }

func (f fixedSGs) SecurityGroupsFor(context.Context, client.Object, string) ([]string, error) {
	return f.ids, nil
}

func newService(mods ...func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			UID:       types.UID("svc-uid-1"),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
			Selector:  map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Protocol:   corev1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromString("web-port"),
			}},
		},
	}
	for _, m := range mods {
		m(svc)
	}
	return svc
}

func newEndpointSlice(mods ...func(*discoveryv1.EndpointSlice)) *discoveryv1.EndpointSlice {
	ready := true
	name := "http"
	port := int32(8080)
	protocol := corev1.ProtocolTCP

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abcde",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "web"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.1.5"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     &name,
			Port:     &port,
			Protocol: &protocol,
		}},
	}
	for _, m := range mods {
		m(slice)
	}
	return slice
}

func newServiceReconciler(t *testing.T, objs ...client.Object) *ServiceReconciler {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	return &ServiceReconciler{
		Client:  c,
		Scheme:  scheme,
		Config:  config.Defaults(),
		Project: fixedProject{id: "project-1"},
		Subnets: fixedSubnets{subnets: vif.SubnetSet{
			"service-subnet": &openstack.Subnet{ID: "service-subnet", CIDR: "10.96.0.0/12"},
		}},
		SGs: fixedSGs{ids: []string{"sg-default"}},
	}
}

func reconcileService(t *testing.T, r *ServiceReconciler, svc *corev1.Service) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func readKLB(t *testing.T, r *ServiceReconciler, svc *corev1.Service) *kuryrv1.KuryrLoadBalancer {
	t.Helper()
	var klb kuryrv1.KuryrLoadBalancer
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if err := r.Get(context.Background(), key, &klb); err != nil {
		t.Fatalf("reading the KuryrLoadBalancer: %v", err)
	}
	return &klb
}

func TestServiceCreatesKuryrLoadBalancer(t *testing.T) {
	svc := newService()
	r := newServiceReconciler(t, svc, newEndpointSlice())

	reconcileService(t, r, svc)
	klb := readKLB(t, r, svc)

	if !controllerutil.ContainsFinalizer(klb, kuryrv1.KuryrLoadBalancerFinalizer) {
		t.Error("KuryrLoadBalancer has no finalizer")
	}
	if len(klb.OwnerReferences) != 1 || klb.OwnerReferences[0].Name != svc.Name {
		t.Errorf("ownerReferences = %v, want the Service", klb.OwnerReferences)
	}
	if klb.Spec.IP != "10.96.0.10" {
		t.Errorf("VIP = %q, want the clusterIP", klb.Spec.IP)
	}
	if klb.Spec.SubnetID != "service-subnet" {
		t.Errorf("subnet = %q, want the subnet holding the clusterIP", klb.Spec.SubnetID)
	}
	if klb.Spec.ProjectID != "project-1" {
		t.Errorf("project = %q, want project-1", klb.Spec.ProjectID)
	}
	if len(klb.Spec.Ports) != 1 || klb.Spec.Ports[0].Port != 80 {
		t.Fatalf("ports = %+v, want the Service's single port", klb.Spec.Ports)
	}
	if got := klb.Spec.Ports[0].TargetPort; got != "web-port" {
		t.Errorf("targetPort = %q, want the named port preserved", got)
	}
}

func TestServiceCollectsReadyBackends(t *testing.T) {
	svc := newService()
	notReady := false
	slice := newEndpointSlice(func(s *discoveryv1.EndpointSlice) {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{"10.0.1.6"},
			Conditions: discoveryv1.EndpointConditions{Ready: &notReady},
		})
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			// An unset Ready means ready, per the EndpointSlice API.
			Addresses: []string{"10.0.1.7"},
		})
	})

	r := newServiceReconciler(t, svc, slice)
	reconcileService(t, r, svc)
	klb := readKLB(t, r, svc)

	if len(klb.Spec.Subsets) != 1 {
		t.Fatalf("got %d subsets, want 1", len(klb.Spec.Subsets))
	}
	got := map[string]bool{}
	for _, addr := range klb.Spec.Subsets[0].Addresses {
		got[addr.IP] = true
	}
	if !got["10.0.1.5"] || !got["10.0.1.7"] {
		t.Errorf("addresses = %v, want both ready backends", got)
	}
	if got["10.0.1.6"] {
		t.Error("a backend that is not ready was included")
	}
	if len(klb.Spec.Subsets[0].Ports) != 1 || klb.Spec.Subsets[0].Ports[0].Port != 8080 {
		t.Errorf("subset ports = %+v, want the resolved container port 8080",
			klb.Spec.Subsets[0].Ports)
	}
}

func TestServiceReconcileIsIdempotent(t *testing.T) {
	svc := newService()
	r := newServiceReconciler(t, svc, newEndpointSlice())

	reconcileService(t, r, svc)
	reconcileService(t, r, svc)

	if gen := readKLB(t, r, svc).Generation; gen > 1 {
		t.Errorf("generation = %d, want 1: the second pass rewrote an unchanged object", gen)
	}
}

func TestServiceSkipsHeadlessAndUnsupported(t *testing.T) {
	cases := map[string]func(*corev1.Service){
		"headless":          func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone },
		"no cluster IP yet": func(s *corev1.Service) { s.Spec.ClusterIP = "" },
		"ExternalName": func(s *corev1.Service) {
			s.Spec.Type = corev1.ServiceTypeExternalName
			s.Spec.ExternalName = "example.com"
		},
		"NodePort": func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeNodePort },
	}

	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newService(mod)
			r := newServiceReconciler(t, svc)

			reconcileService(t, r, svc)

			var klb kuryrv1.KuryrLoadBalancer
			key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
			if err := r.Get(context.Background(), key, &klb); err == nil {
				t.Error("a KuryrLoadBalancer was created for a Service that needs none")
			}
		})
	}
}

func TestServiceSkipsKubernetesAPIService(t *testing.T) {
	// The API server's own Service is served over TLS by the control plane.
	svc := newService(func(s *corev1.Service) {
		s.Name = "kubernetes"
		s.Namespace = "default"
	})
	r := newServiceReconciler(t, svc)

	reconcileService(t, r, svc)

	var klb kuryrv1.KuryrLoadBalancer
	key := types.NamespacedName{Namespace: "default", Name: "kubernetes"}
	if err := r.Get(context.Background(), key, &klb); err == nil {
		t.Error("a KuryrLoadBalancer was created for the Kubernetes API Service")
	}
}

func TestServiceReleasesLoadBalancerWhenTypeChanges(t *testing.T) {
	// A Service edited to be headless no longer needs a load balancer; the
	// KuryrLoadBalancer has to go so its finalizer releases Octavia.
	svc := newService()
	r := newServiceReconciler(t, svc, newEndpointSlice())
	reconcileService(t, r, svc)

	var current corev1.Service
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if err := r.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("reading the Service: %v", err)
	}
	current.Spec.ClusterIP = corev1.ClusterIPNone
	if err := r.Update(context.Background(), &current); err != nil {
		t.Fatalf("updating the Service: %v", err)
	}

	reconcileService(t, r, svc)

	klb := readKLB(t, r, svc)
	if klb.DeletionTimestamp.IsZero() {
		t.Error("the KuryrLoadBalancer was not deleted after the Service became headless")
	}
}

func TestServiceRejectsClusterIPOutsideTheServiceSubnet(t *testing.T) {
	// Silently building the load balancer on the wrong subnet would produce a
	// VIP nothing can reach, so this has to be an error an operator sees.
	svc := newService(func(s *corev1.Service) { s.Spec.ClusterIP = "172.30.0.1" })
	r := newServiceReconciler(t, svc)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
	})
	if err == nil {
		t.Fatal("expected an error for a clusterIP outside every service subnet")
	}
}

func TestServiceTimeoutAnnotations(t *testing.T) {
	svc := newService(func(s *corev1.Service) {
		s.Annotations = map[string]string{
			ClientTimeoutAnnotation: "60000",
			MemberTimeoutAnnotation: "not-a-number",
		}
	})
	r := newServiceReconciler(t, svc)
	r.Config.OctaviaDefaults.TimeoutMemberData = 12345

	reconcileService(t, r, svc)
	klb := readKLB(t, r, svc)

	if klb.Spec.TimeoutClientData != 60000 {
		t.Errorf("client timeout = %d, want the annotated 60000", klb.Spec.TimeoutClientData)
	}
	// A typo in an optional tuning knob must not stop the Service working.
	if klb.Spec.TimeoutMemberData != 12345 {
		t.Errorf("member timeout = %d, want the configured default after an unparseable annotation",
			klb.Spec.TimeoutMemberData)
	}
}

func TestServiceIgnoresFQDNSlices(t *testing.T) {
	// An FQDN endpoint names something outside the cluster, which Octavia
	// cannot register as a member.
	svc := newService()
	slice := newEndpointSlice(func(s *discoveryv1.EndpointSlice) {
		s.AddressType = discoveryv1.AddressTypeFQDN
		s.Endpoints[0].Addresses = []string{"backend.example.com"}
	})

	r := newServiceReconciler(t, svc, slice)
	reconcileService(t, r, svc)

	if subsets := readKLB(t, r, svc).Spec.Subsets; len(subsets) != 0 {
		t.Errorf("subsets = %+v, want none from an FQDN slice", subsets)
	}
}

func TestSliceToServiceMapping(t *testing.T) {
	slice := newEndpointSlice()
	reqs := sliceToService(context.Background(), slice)
	if len(reqs) != 1 || reqs[0].Name != "web" || reqs[0].Namespace != "default" {
		t.Fatalf("requests = %v, want the owning Service", reqs)
	}

	orphan := newEndpointSlice(func(s *discoveryv1.EndpointSlice) { s.Labels = nil })
	if reqs := sliceToService(context.Background(), orphan); len(reqs) != 0 {
		t.Errorf("requests = %v, want none for a slice with no service name", reqs)
	}
}

// A pod whose security groups change must refresh the groups of every
// Service in its namespace, and only those.
func TestServiceRefreshesOnPodSecurityGroupChange(t *testing.T) {
	here := newService()
	elsewhere := newService(func(s *corev1.Service) { s.Namespace = "other" })
	r := newServiceReconciler(t, here, elsewhere)

	kp := &kuryrv1.KuryrPort{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: here.Namespace}}
	got := r.servicesInNamespace(context.Background(), kp)
	if len(got) != 1 || got[0].Name != here.Name || got[0].Namespace != here.Namespace {
		t.Errorf("enqueued %v, want only %s/%s", got, here.Namespace, here.Name)
	}

	changed := kp.DeepCopy()
	changed.Status.SecurityGroups = []string{"sg-policy"}
	if !securityGroupsChanged.Update(event.UpdateEvent{ObjectOld: kp, ObjectNew: changed}) {
		t.Error("a security group change was filtered out")
	}
	activated := kp.DeepCopy()
	activated.Status.Conditions = []metav1.Condition{{Type: "Active"}}
	if securityGroupsChanged.Update(event.UpdateEvent{ObjectOld: kp, ObjectNew: activated}) {
		t.Error("a status write that leaves the groups alone should not refresh Services")
	}
}
