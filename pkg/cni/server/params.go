// Package server implements the CNI daemon HTTP API.
//
// This replaces the Flask application in kuryr_kubernetes/cni/daemon/service.py
// and the plugin logic in cni/plugins/k8s_cni_registry.py.
//
// The wire format is unchanged from the Python daemon: the CNI plugin POSTs
// its environment as JSON to /addNetwork or /delNetwork on a loopback socket.
// Keeping it means the existing plugin binary, and anyone's debugging habits
// with curl, still work.
package server

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Params is the CNI invocation as the plugin forwards it.
type Params struct {
	Command     string `json:"CNI_COMMAND"`
	ContainerID string `json:"CNI_CONTAINERID"`
	NetNS       string `json:"CNI_NETNS"`
	IfName      string `json:"CNI_IFNAME"`
	Args        string `json:"CNI_ARGS"`
	Path        string `json:"CNI_PATH"`
	// Config is the contents of the CNI network configuration. Kuryr does not
	// read anything out of it today, but it is part of the contract and
	// useful in logs.
	Config any `json:"config_kuryr,omitempty"`
}

// K8sArgs are the Kubernetes-specific values the runtime packs into CNI_ARGS.
type K8sArgs struct {
	Namespace string
	Name      string
	UID       types.UID
	// InfraContainerID is the sandbox container, which differs from
	// CNI_CONTAINERID on some runtimes.
	InfraContainerID string
}

// ParseArgs decodes the semicolon-separated KEY=VALUE form of CNI_ARGS.
//
// K8S_POD_UID is absent on older runtimes; the caller has to cope rather than
// reject, which is why it is not validated here.
func ParseArgs(raw string) K8sArgs {
	var out K8sArgs
	for _, pair := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "K8S_POD_NAMESPACE":
			out.Namespace = value
		case "K8S_POD_NAME":
			out.Name = value
		case "K8S_POD_UID":
			out.UID = types.UID(value)
		case "K8S_POD_INFRA_CONTAINER_ID":
			out.InfraContainerID = value
		}
	}
	return out
}

// Validate rejects a request that cannot be acted on at all.
func (p *Params) Validate() (K8sArgs, error) {
	args := ParseArgs(p.Args)
	if args.Namespace == "" || args.Name == "" {
		return args, fmt.Errorf("CNI_ARGS is missing K8S_POD_NAMESPACE or K8S_POD_NAME")
	}
	if p.IfName == "" {
		return args, fmt.Errorf("CNI_IFNAME is empty")
	}
	// CNI_NETNS is legitimately empty on a DEL for a sandbox that never came
	// up, so it is only required for ADD.
	if p.Command == "ADD" && p.NetNS == "" {
		return args, fmt.Errorf("CNI_NETNS is empty on an ADD request")
	}
	return args, nil
}
