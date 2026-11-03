package policy

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// ClientResolver answers the translation's questions from the Kubernetes API,
// through the controller-runtime cache. Every read is served from the
// informer cache, so resolving a policy costs no API calls.
type ClientResolver struct {
	Client client.Client

	// NamespaceSubnets says whether each namespace has its own Neutron
	// subnet. When it does, a namespaceSelector collapses to one rule per
	// namespace instead of one per pod. It mirrors the namespace subnet
	// driver being configured.
	NamespaceSubnets bool
}

var _ Resolver = (*ClientResolver)(nil)

// Namespaces returns the names of namespaces matching sel.
//
// A nil selector is not "match everything": in NetworkPolicy it means the
// peer is confined to the policy's own namespace.
func (r *ClientResolver) Namespaces(ctx context.Context, sel *metav1.LabelSelector,
	policyNamespace string) ([]string, error) {

	if sel == nil {
		return []string{policyNamespace}, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("namespaceSelector: %w", err)
	}

	var list corev1.NamespaceList
	if err := r.Client.List(ctx, &list, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("listing namespaces for selector %q: %w", selector, err)
	}

	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].Name)
	}
	return out, nil
}

// Pods returns the pods matching sel in the given namespaces, with their
// addresses and named container ports.
//
// Pods without an address yet are skipped rather than reported as an error:
// a pod that has not been assigned an IP cannot appear in a rule, and it will
// come back through the pod watch once it has one.
func (r *ClientResolver) Pods(ctx context.Context, sel *metav1.LabelSelector,
	namespaces []string) ([]PodRef, error) {

	selector := labels.Everything()
	if sel != nil {
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("podSelector: %w", err)
		}
		selector = s
	}

	var out []PodRef
	for _, ns := range namespaces {
		var list corev1.PodList
		err := r.Client.List(ctx, &list,
			client.InNamespace(ns),
			client.MatchingLabelsSelector{Selector: selector})
		if err != nil {
			return nil, fmt.Errorf("listing pods in namespace %q for selector %q: %w", ns, selector, err)
		}

		for i := range list.Items {
			pod := &list.Items[i]
			// A host-network pod shares the node's address. Writing a rule
			// for it would open the whole node, which the policy never asked
			// for, so leave it out.
			if pod.Spec.HostNetwork {
				continue
			}
			if pod.Status.PodIP == "" {
				continue
			}
			out = append(out, PodRef{
				Namespace:      pod.Namespace,
				Name:           pod.Name,
				IP:             pod.Status.PodIP,
				ContainerPorts: namedContainerPorts(pod),
				Labels:         pod.Labels,
			})
		}
	}

	return out, nil
}

// namedContainerPorts collects a pod's named container ports.
//
// Only named ports are collected: a NetworkPolicy can only refer to a port by
// name, so an unnamed one would be dead weight carried for every pod in every
// peer of every policy.
func namedContainerPorts(pod *corev1.Pod) map[string][]int32 {
	var out map[string][]int32
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == "" {
				continue
			}
			if out == nil {
				out = map[string][]int32{}
			}
			if !slices.Contains(out[port.Name], port.ContainerPort) {
				out[port.Name] = append(out[port.Name], port.ContainerPort)
			}
		}
	}
	return out
}

// NamespaceCIDR returns the subnet backing a namespace, or an empty string
// when namespaces do not have their own subnets.
//
// An empty string is also returned while the namespace's KuryrNetwork exists
// but has not been populated yet. The caller then falls back to pod IPs,
// which is correct if less compact, and the policy is recomputed when the
// KuryrNetwork becomes ready.
func (r *ClientResolver) NamespaceCIDR(ctx context.Context, namespace string) (string, error) {
	if !r.NamespaceSubnets {
		return "", nil
	}

	var list kuryrv1.KuryrNetworkList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("listing KuryrNetworks in namespace %q: %w", namespace, err)
	}
	for i := range list.Items {
		if cidr := list.Items[i].Status.SubnetCIDR; cidr != "" {
			return cidr, nil
		}
	}
	return "", nil
}

// NamedPorts resolves a named container port against the pods the policy
// applies to.
//
// NetworkPolicy names a port on the *target* pods, not on the peer, so the
// selector here is the policy's own podSelector. Different pods may map the
// same name to different numbers, so every distinct number is returned.
func (r *ClientResolver) NamedPorts(ctx context.Context, sel *metav1.LabelSelector,
	namespace, portName string) ([]int32, error) {

	selector := labels.Everything()
	if sel != nil {
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("podSelector: %w", err)
		}
		selector = s
	}

	var list corev1.PodList
	err := r.Client.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing pods in namespace %q to resolve port %q: %w", namespace, portName, err)
	}

	seen := map[int32]bool{}
	var out []int32
	for i := range list.Items {
		for _, container := range list.Items[i].Spec.Containers {
			for _, port := range container.Ports {
				if port.Name != portName || seen[port.ContainerPort] {
					continue
				}
				seen[port.ContainerPort] = true
				out = append(out, port.ContainerPort)
			}
		}
	}
	return out, nil
}

// Services returns the Services in a namespace that have a cluster IP and are
// not being deleted.
func (r *ClientResolver) Services(ctx context.Context, namespace string) ([]ServiceRef, error) {
	var list corev1.ServiceList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Services in namespace %q: %w", namespace, err)
	}
	out := make([]ServiceRef, 0, len(list.Items))
	for i := range list.Items {
		svc := &list.Items[i]
		if !svc.DeletionTimestamp.IsZero() {
			continue
		}
		var ips []string
		for _, ip := range svc.Spec.ClusterIPs {
			if ip != "" && ip != corev1.ClusterIPNone {
				ips = append(ips, ip)
			}
		}
		if len(ips) == 0 {
			continue
		}
		out = append(out, ServiceRef{
			Namespace:  svc.Namespace,
			Name:       svc.Name,
			ClusterIPs: ips,
			Selector:   svc.Spec.Selector,
		})
	}
	return out, nil
}
