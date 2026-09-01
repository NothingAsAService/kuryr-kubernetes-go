// Command kuryr-cni is the CNI plugin kubelet invokes.
//
// It contains no networking logic at all: it forwards its environment to
// kuryr-daemon over a loopback socket and renders the reply as a CNI result.
// All the privileged work happens in the daemon, which is already running and
// already has the state, so the plugin can stay a short-lived process that
// does nothing but marshal JSON.
//
// This replaces kuryr_cni/pkg/main.go. The reflection-based decoder for
// oslo.versionedobjects that lived in ovo.go is gone: the daemon now returns a
// plain typed VIF, so this is ordinary json.Unmarshal.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	cniversion "github.com/containernetworking/cni/pkg/version"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// Error codes above the range CNI reserves, matching what the daemon returns.
const (
	errDaemonUnreachable uint = 700
	errBadResponse       uint = 701
	errBadAddress        uint = 702
)

// pluginVersion is reported to the runtime.
const pluginVersion = "kuryr-kubernetes CNI plugin (Go)"

// NetConf is the plugin section of the CNI configuration list.
type NetConf struct {
	cnitypes.NetConf

	// DaemonAddress overrides where kuryr-daemon listens. The Python plugin
	// hardcoded localhost:5036 with a FIXME; putting it in the conflist means
	// a non-default [cni_daemon]bind_address no longer needs a rebuild.
	DaemonAddress string `json:"daemonAddress,omitempty"`
}

const defaultDaemonAddress = "127.0.0.1:5036"

// request is the body the daemon expects. The field names are the CNI
// environment variables, unchanged from the Python daemon's API.
type request struct {
	Command     string `json:"CNI_COMMAND"`
	ContainerID string `json:"CNI_CONTAINERID"`
	NetNS       string `json:"CNI_NETNS"`
	IfName      string `json:"CNI_IFNAME"`
	Args        string `json:"CNI_ARGS"`
	Path        string `json:"CNI_PATH"`
	Config      any    `json:"config_kuryr"`
}

// daemonError is the error body the daemon returns.
type daemonError struct {
	Code    uint   `json:"code"`
	Msg     string `json:"msg"`
	Details string `json:"details"`
}

func main() {
	skel.PluginMainFuncs(
		skel.CNIFuncs{Add: cmdAdd, Check: cmdCheck, Del: cmdDel},
		cniversion.All,
		pluginVersion,
	)
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	body, err := call(conf, buildRequest(args, "ADD"), http.StatusAccepted)
	if err != nil {
		return err
	}

	var vif kuryrv1.VIF
	if err := json.Unmarshal(body, &vif); err != nil {
		return &cnitypes.Error{
			Code:    errBadResponse,
			Msg:     "could not decode the VIF returned by kuryr-daemon",
			Details: fmt.Sprintf("%v: %s", err, truncate(body)),
		}
	}

	result, err := toResult(&vif, args)
	if err != nil {
		return err
	}
	return cnitypes.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}
	_, err = call(conf, buildRequest(args, "DEL"), http.StatusNoContent)
	return err
}

// cmdCheck reports success: the daemon already blocks ADD until the port is
// ACTIVE, so there is no separate readiness state for the runtime to poll.
func cmdCheck(*skel.CmdArgs) error { return nil }

func parseConfig(stdin []byte) (*NetConf, error) {
	conf := &NetConf{}
	if len(stdin) > 0 {
		if err := json.Unmarshal(stdin, conf); err != nil {
			return nil, &cnitypes.Error{
				Code:    cnitypes.ErrDecodingFailure,
				Msg:     "could not parse the CNI network configuration",
				Details: err.Error(),
			}
		}
	}
	if conf.DaemonAddress == "" {
		conf.DaemonAddress = defaultDaemonAddress
	}
	return conf, nil
}

func buildRequest(args *skel.CmdArgs, command string) request {
	var conf any
	if len(args.StdinData) > 0 {
		_ = json.Unmarshal(args.StdinData, &conf)
	}
	return request{
		Command:     command,
		ContainerID: args.ContainerID,
		NetNS:       args.Netns,
		IfName:      args.IfName,
		Args:        args.Args,
		Path:        args.Path,
		Config:      conf,
	}
}

// call POSTs to the daemon and returns the body on the expected status.
func call(conf *NetConf, req request, wantStatus int) ([]byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, &cnitypes.Error{
			Code:    cnitypes.ErrInvalidNetworkConfig,
			Msg:     "could not encode the request for kuryr-daemon",
			Details: err.Error(),
		}
	}

	path := "addNetwork"
	if req.Command == "DEL" {
		path = "delNetwork"
	}
	url := "http://" + conf.DaemonAddress + "/" + path

	// The daemon holds ADD open until Neutron activates the port, so the
	// client must outlast [cni_daemon]vif_annotation_timeout. The runtime
	// enforces its own deadline above this.
	client := &http.Client{Timeout: 5 * time.Minute}

	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, &cnitypes.Error{
			Code: errDaemonUnreachable,
			Msg: fmt.Sprintf("cannot reach kuryr-daemon at %s; is the kuryr-cni pod running on this node",
				conf.DaemonAddress),
			Details: err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != wantStatus {
		// The daemon reports a structured error; pass its code straight
		// through so the runtime sees the same classification.
		var derr daemonError
		if json.Unmarshal(body, &derr) == nil && derr.Code != 0 {
			return nil, &cnitypes.Error{Code: derr.Code, Msg: derr.Msg, Details: derr.Details}
		}
		return nil, &cnitypes.Error{
			Code:    uint(resp.StatusCode),
			Msg:     fmt.Sprintf("kuryr-daemon returned %s", resp.Status),
			Details: truncate(body),
		}
	}
	return body, nil
}

// toResult renders a VIF as a CNI result.
func toResult(vif *kuryrv1.VIF, args *skel.CmdArgs) (*current.Result, error) {
	result := &current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		Interfaces: []*current.Interface{{
			Name:    args.IfName,
			Mac:     vif.Address,
			Sandbox: args.Netns,
		}},
	}

	var nameservers []string

	for _, sub := range vif.Network.Subnets {
		_, cidr, err := net.ParseCIDR(sub.CIDR)
		if err != nil {
			return nil, &cnitypes.Error{
				Code:    errBadAddress,
				Msg:     fmt.Sprintf("kuryr-daemon returned an unparseable CIDR %q", sub.CIDR),
				Details: err.Error(),
			}
		}
		ones, bits := cidr.Mask.Size()

		for _, raw := range sub.IPs {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, &cnitypes.Error{
					Code: errBadAddress,
					Msg:  fmt.Sprintf("kuryr-daemon returned an unparseable address %q", raw),
				}
			}
			ifaceIndex := 0
			entry := &current.IPConfig{
				Interface: &ifaceIndex,
				Address:   net.IPNet{IP: ip, Mask: net.CIDRMask(ones, bits)},
			}
			if sub.Gateway != "" {
				entry.Gateway = net.ParseIP(sub.Gateway)
			}
			result.IPs = append(result.IPs, entry)
		}

		for _, r := range sub.Routes {
			_, dst, err := net.ParseCIDR(r.Destination)
			if err != nil {
				return nil, &cnitypes.Error{
					Code: errBadAddress,
					Msg:  fmt.Sprintf("kuryr-daemon returned an unparseable route %q", r.Destination),
				}
			}
			gw := net.ParseIP(r.NextHop)
			if gw == nil {
				return nil, &cnitypes.Error{
					Code: errBadAddress,
					Msg:  fmt.Sprintf("kuryr-daemon returned an unparseable next hop %q", r.NextHop),
				}
			}
			result.Routes = append(result.Routes, &cnitypes.Route{Dst: *dst, GW: gw})
		}

		nameservers = append(nameservers, sub.DNS...)
	}

	if len(nameservers) > 0 {
		result.DNS.Nameservers = nameservers
	}
	return result, nil
}

func truncate(b []byte) string {
	const limit = 512
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "..."
}
