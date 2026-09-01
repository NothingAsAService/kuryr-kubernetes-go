package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
	"opendev.org/openstack/kuryr-kubernetes/pkg/drivers"
)

// Annotations that let a Service override the Octavia listener timeouts.
// They keep the Python names so an existing manifest still works.
const (
	ClientTimeoutAnnotation = "openstack.org/kuryr-timeout-client-data"
	MemberTimeoutAnnotation = "openstack.org/kuryr-timeout-member-data"
)

// ServiceReconciler turns a Service and its EndpointSlices into one
// KuryrLoadBalancer.
//
// The Python implementation split this in two: a Service handler wrote the
// frontend half of the spec and an Endpoints handler wrote the backend half,
// both patching the same object. That is two writers on one resource, and it
// produced conflicts that were retried through the whole pipeline. Here one
// controller reads both and writes the spec once, so the object is always
// internally consistent and a conflict means someone else edited it.
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Config

	Project ProjectResolver
	Subnets SubnetResolver
	SGs     SecurityGroupResolver

	// Provider is the Octavia provider to build with, empty for the cloud
	// default. It is resolved once at startup because it needs an API call.
	Provider string
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=openstack.org,resources=kuryrloadbalancers,verbs=get;list;watch;create;update;patch;delete

// Reconcile keeps a KuryrLoadBalancer in step with its Service.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			// The KuryrLoadBalancer is owned by the Service, so Kubernetes
			// deletes it and its finalizer releases Octavia.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading Service %s: %w", req.NamespacedName, err)
	}

	if !svc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if reason := skipService(&svc); reason != "" {
		logger.V(1).Info("skipping Service", "reason", reason)
		return r.deleteIfPresent(ctx, &svc)
	}

	spec, err := r.buildSpec(ctx, &svc)
	if err != nil {
		if errors.Is(err, drivers.ErrRetryLater) {
			logger.V(1).Info("Service is not ready to be built yet", "reason", err)
			return ctrl.Result{RequeueAfter: portActivationRequeue}, nil
		}
		return ctrl.Result{}, err
	}

	klb := &kuryrv1.KuryrLoadBalancer{
		ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, klb, func() error {
		// The finalizer goes on with the first write: Octavia resources may
		// exist before this controller runs again, and a delete arriving in
		// between would leak a load balancer.
		controllerutil.AddFinalizer(klb, kuryrv1.KuryrLoadBalancerFinalizer)
		if err := controllerutil.SetControllerReference(&svc, klb, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		klb.Spec = *spec
		return nil
	})
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: recheckRequeue}, nil
		}
		return ctrl.Result{}, fmt.Errorf("writing KuryrLoadBalancer %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	if result != controllerutil.OperationResultNone {
		backends := 0
		for i := range spec.Subsets {
			backends += len(spec.Subsets[i].Addresses)
		}
		logger.Info("KuryrLoadBalancer written",
			"result", result, "ports", len(spec.Ports), "backends", backends)
	}
	return ctrl.Result{}, nil
}

// skipService returns why a Service is not backed by a load balancer, or an
// empty string when it is.
func skipService(svc *corev1.Service) string {
	switch svc.Spec.Type {
	case corev1.ServiceTypeClusterIP, corev1.ServiceTypeLoadBalancer:
	default:
		return fmt.Sprintf("Service type %s is not backed by Octavia", svc.Spec.Type)
	}

	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return "headless Service, there is nothing to load balance"
	}

	// The API server's own Service points at the control plane over TLS and
	// is not something Kuryr should put a load balancer in front of.
	if svc.Namespace == "default" && svc.Name == "kubernetes" {
		return "the Kubernetes API Service is served by the control plane itself"
	}

	return ""
}

// deleteIfPresent removes a KuryrLoadBalancer for a Service that should no
// longer have one, which happens when a Service changes type.
func (r *ServiceReconciler) deleteIfPresent(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	var klb kuryrv1.KuryrLoadBalancer
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if err := r.Get(ctx, key, &klb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading KuryrLoadBalancer %s: %w", key, err)
	}
	if !klb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.Delete(ctx, &klb); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting KuryrLoadBalancer %s: %w", key, err)
	}
	log.FromContext(ctx).Info("Service no longer needs a load balancer, releasing it")
	return ctrl.Result{}, nil
}

// buildSpec assembles the whole desired state of the load balancer.
func (r *ServiceReconciler) buildSpec(ctx context.Context,
	svc *corev1.Service) (*kuryrv1.KuryrLoadBalancerSpec, error) {

	projectID, err := r.Project.ProjectFor(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("resolving project for Service %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	subnetID, err := r.subnetFor(ctx, svc, projectID)
	if err != nil {
		return nil, err
	}

	sgs, err := r.SGs.SecurityGroupsFor(ctx, svc, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolving security groups for Service %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	subsets, err := r.backendsFor(ctx, svc)
	if err != nil {
		return nil, err
	}

	clientTimeout, memberTimeout := r.timeouts(svc)

	spec := &kuryrv1.KuryrLoadBalancerSpec{
		ProjectID:         projectID,
		SubnetID:          subnetID,
		IP:                svc.Spec.ClusterIP,
		LoadBalancerIP:    svc.Spec.LoadBalancerIP,
		Type:              string(svc.Spec.Type),
		Provider:          r.Provider,
		Ports:             servicePorts(svc),
		SecurityGroupIDs:  sgs,
		Subsets:           subsets,
		TimeoutClientData: clientTimeout,
		TimeoutMemberData: memberTimeout,
	}
	return spec, nil
}

// subnetFor returns the subnet the ClusterIP belongs to.
//
// Octavia needs the subnet the VIP address sits on. With one service subnet
// there is only one candidate, but the driver may return several, so the one
// actually containing the address is picked.
func (r *ServiceReconciler) subnetFor(ctx context.Context, svc *corev1.Service, projectID string) (string, error) {
	subnets, err := r.Subnets.SubnetsFor(ctx, svc, projectID)
	if err != nil {
		return "", fmt.Errorf("resolving subnets for Service %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	addr, err := netip.ParseAddr(svc.Spec.ClusterIP)
	if err != nil {
		return "", fmt.Errorf("clusterIP %q of Service %s/%s is unparseable: %w",
			svc.Spec.ClusterIP, svc.Namespace, svc.Name, err)
	}

	var matches []string
	for id, subnet := range subnets {
		prefix, err := netip.ParsePrefix(subnet.CIDR)
		if err != nil {
			return "", fmt.Errorf("subnet %s has an unparseable CIDR %q: %w", id, subnet.CIDR, err)
		}
		if prefix.Contains(addr) {
			matches = append(matches, id)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("clusterIP %s of Service %s/%s is not in any service subnet; "+
			"check that [neutron_defaults]service_subnet matches the cluster's service CIDR",
			svc.Spec.ClusterIP, svc.Namespace, svc.Name)
	default:
		// Overlapping subnets would make the choice arbitrary and the load
		// balancer land somewhere unpredictable.
		slices.Sort(matches)
		return "", fmt.Errorf("clusterIP %s of Service %s/%s is in %d overlapping subnets (%v); "+
			"Kuryr cannot tell which one the VIP belongs on",
			svc.Spec.ClusterIP, svc.Namespace, svc.Name, len(matches), matches)
	}
}

// timeouts returns the listener timeouts, from the Service annotations when
// present and from kuryr.conf otherwise.
//
// An unparseable annotation falls back to the configured value rather than
// failing the Service: a typo in an optional tuning knob should not stop
// traffic from being served.
func (r *ServiceReconciler) timeouts(svc *corev1.Service) (int32, int32) {
	client := r.Config.OctaviaDefaults.TimeoutClientData
	member := r.Config.OctaviaDefaults.TimeoutMemberData

	if v, ok := svc.Annotations[ClientTimeoutAnnotation]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil && parsed >= 0 {
			client = int32(parsed)
		}
	}
	if v, ok := svc.Annotations[MemberTimeoutAnnotation]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil && parsed >= 0 {
			member = int32(parsed)
		}
	}
	return client, member
}

// servicePorts converts the Service ports to the CRD shape, in a stable
// order so an unchanged Service produces an unchanged spec.
func servicePorts(svc *corev1.Service) []kuryrv1.ServicePort {
	out := make([]kuryrv1.ServicePort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		protocol := p.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		out = append(out, kuryrv1.ServicePort{
			Name:     p.Name,
			Protocol: protocol,
			Port:     p.Port,
			// A target port is either a number or a name; storing it as a
			// string keeps both without a union type in the CRD.
			TargetPort: p.TargetPort.String(),
		})
	}
	slices.SortFunc(out, func(a, b kuryrv1.ServicePort) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Protocol, b.Protocol))
	})
	return out
}

// backendsFor collects the ready endpoints of a Service from its
// EndpointSlices.
func (r *ServiceReconciler) backendsFor(ctx context.Context, svc *corev1.Service) ([]kuryrv1.EndpointSubset, error) {
	var sliceList discoveryv1.EndpointSliceList
	err := r.List(ctx, &sliceList,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name})
	if err != nil {
		return nil, fmt.Errorf("listing EndpointSlices of Service %s/%s: %w",
			svc.Namespace, svc.Name, err)
	}

	var out []kuryrv1.EndpointSubset
	for i := range sliceList.Items {
		slice := &sliceList.Items[i]
		// Only IPv4 and IPv6 slices carry addresses; an FQDN slice names
		// something outside the cluster that Octavia cannot reach as a member.
		if slice.AddressType == discoveryv1.AddressTypeFQDN {
			continue
		}

		subset := kuryrv1.EndpointSubset{Ports: sliceServicePorts(slice)}
		for j := range slice.Endpoints {
			ep := &slice.Endpoints[j]
			if !endpointReady(ep) {
				// A backend that is not ready must not receive traffic.
				continue
			}
			for _, addr := range ep.Addresses {
				subset.Addresses = append(subset.Addresses, kuryrv1.EndpointAddress{
					IP:        addr,
					Ready:     true,
					TargetRef: ep.TargetRef,
				})
			}
		}

		if len(subset.Addresses) == 0 {
			continue
		}
		slices.SortFunc(subset.Addresses, func(a, b kuryrv1.EndpointAddress) int {
			return cmp.Compare(a.IP, b.IP)
		})
		out = append(out, subset)
	}

	// EndpointSlices come back in whatever order the cache holds them, and an
	// unstable spec would make this controller rewrite the object forever.
	slices.SortFunc(out, func(a, b kuryrv1.EndpointSubset) int {
		return cmp.Compare(firstAddress(a), firstAddress(b))
	})
	return out, nil
}

// endpointReady reports whether an endpoint should receive traffic. An
// unset Ready means ready, per the EndpointSlice API.
func endpointReady(ep *discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

// sliceServicePorts converts EndpointSlice ports to the CRD shape.
func sliceServicePorts(slice *discoveryv1.EndpointSlice) []kuryrv1.ServicePort {
	out := make([]kuryrv1.ServicePort, 0, len(slice.Ports))
	for _, p := range slice.Ports {
		if p.Port == nil {
			// A port with no number is only meaningful for an FQDN slice,
			// which is skipped above; there is nothing to target here.
			continue
		}
		port := kuryrv1.ServicePort{Port: *p.Port, Protocol: corev1.ProtocolTCP}
		if p.Name != nil {
			port.Name = *p.Name
		}
		if p.Protocol != nil {
			port.Protocol = *p.Protocol
		}
		out = append(out, port)
	}
	slices.SortFunc(out, func(a, b kuryrv1.ServicePort) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func firstAddress(s kuryrv1.EndpointSubset) string {
	if len(s.Addresses) == 0 {
		return ""
	}
	return s.Addresses[0].IP
}

// SetupWithManager registers the reconciler.
//
// EndpointSlices are watched and mapped back to their Service, which is what
// keeps the backend half of the spec current as pods come and go.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(sliceToService)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.Kubernetes.MaxConcurrentReconciles,
		}).
		Named("service").
		Complete(r)
}

// sliceToService maps an EndpointSlice onto the Service that owns it.
func sliceToService(_ context.Context, obj client.Object) []reconcile.Request {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}
	name := slice.Labels[discoveryv1.LabelServiceName]
	if name == "" {
		// A slice with no service name is not managed by the endpoint
		// controller and belongs to nothing we track.
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: slice.Namespace, Name: name},
	}}
}
