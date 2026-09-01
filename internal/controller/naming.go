package controller

import (
	"fmt"
	"strings"
)

// maxOpenStackNameLen is the limit Neutron puts on a resource name.
const maxOpenStackNameLen = 255

// openStackName builds a name for an OpenStack resource that stands for a
// Kubernetes one.
//
// A Kubernetes name can be 253 characters, so a UID and a prefix can push the
// result past Neutron's limit. The UID is what makes the name unique and
// traceable, so the readable part is what gets truncated. This matches the
// Python get_resource_name so an operator sees the same names either way.
func openStackName(uid, name string) string {
	prefix := ""
	if uid != "" {
		prefix = uid + "/"
	}

	if len(prefix)+len(name) <= maxOpenStackNameLen {
		return prefix + name
	}

	room := maxOpenStackNameLen - len(prefix)
	if room <= 0 {
		// A UID alone should never reach the limit, but truncate rather than
		// panic on a slice if it somehow does.
		return prefix[:maxOpenStackNameLen]
	}
	return prefix + name[:room]
}

// namespaceNetworkName is the Neutron network backing a namespace.
func namespaceNetworkName(namespace, uid string) string {
	return openStackName(uid, namespace)
}

// namespaceSubnetName is the Neutron subnet backing a namespace.
func namespaceSubnetName(namespace, uid string) string {
	return openStackName(uid, namespace)
}

// policySecurityGroupName is the Neutron security group backing a
// NetworkPolicy.
func policySecurityGroupName(namespace, name, uid string) string {
	return openStackName(uid, strings.Join([]string{namespace, name}, "/"))
}

// loadBalancerName is the Octavia load balancer backing a Service.
func loadBalancerName(namespace, name, uid string) string {
	return openStackName(uid, strings.Join([]string{namespace, name}, "/"))
}

// listenerName identifies a listener, and the pool behind it, within a load
// balancer. Protocol and port are what make a listener unique, so the name
// says exactly that.
func listenerName(lbName, protocol string, port int32) string {
	return truncateName(fmt.Sprintf("%s:%s:%d", lbName, protocol, port))
}

// memberName identifies one backend within a pool.
func memberName(lbName, address string, port int32) string {
	return truncateName(fmt.Sprintf("%s/%s:%d", lbName, address, port))
}

// truncateName keeps a derived name inside Neutron's limit. The suffix is
// what distinguishes one resource from another, so the front is what goes.
func truncateName(s string) string {
	if len(s) <= maxOpenStackNameLen {
		return s
	}
	return s[len(s)-maxOpenStackNameLen:]
}
