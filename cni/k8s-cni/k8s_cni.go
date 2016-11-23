package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	"git.code.oa.com/gaiastack/galaxy/pkg/api/k8s"
)

var (
	stateDir = "/var/lib/cni/galaxy"
)

type NetConf struct {
	types.NetConf
	NetworkType map[string]map[string]interface{} `json:"networkType,omitempty"`
	//ipam url, currently its the apiswitch
	URL        string `json:"url"`
	NetworkURI string `json:"network_uri"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(bytes []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, fmt.Errorf("failed to load hybrid netconf: %v", err)
	}
	return conf, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	kvMap, err := k8s.ParseK8SCNIArgs(args.Args)
	if err != nil {
		return err
	}
	for networkType, delegate := range conf.NetworkType {
		result, err := delegateCmd(kvMap[k8s.K8S_POD_INFRA_CONTAINER_ID], delegate, true)
		if err != nil {
			return fmt.Errorf("failed to delegate setup network %s: %v", networkType, err)
		}
		ports, err := k8s.ParsePorts(kvMap[k8s.K8S_PORTS])
		if err != nil {
			return err
		}
		if err := savePort(kvMap[k8s.K8S_POD_INFRA_CONTAINER_ID], kvMap[k8s.K8S_PORTS]); err != nil {
			return fmt.Errorf("failed to save ports %v", err)
		}
		// we have to fulfill ip field of the current pod
		if result.IP4 == nil {
			return fmt.Errorf("CNI plugin reported no IPv4 address")
		}
		ip4 := result.IP4.IP.IP.To4()
		if ip4 == nil {
			return fmt.Errorf("CNI plugin reported an invalid IPv4 address: %+v.", result.IP4)
		}
		for _, p := range ports {
			if p.PodName == kvMap[k8s.K8S_POD_NAME]+"_"+kvMap[k8s.K8S_POD_NAMESPACE] {
				p.PodIP = ip4.String()
			}
		}
		if len(ports) != 0 {
			if err := setupPortMapping("cni0", ports); err != nil {
				return fmt.Errorf("failed to setup port mapping %v: %v", ports, err)
			}
		}
		//TODO send http request for l5 setup
		return result.Print()
	}
	return fmt.Errorf("no network configured")
}

func cmdDel(args *skel.CmdArgs) error {
	if args.Netns == "" {
		// avoid k8s double delete error
		// see https://github.com/kubernetes/kubernetes/issues/20379#issuecomment-255272531
		return nil
	}
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	kvMap, err := k8s.ParseK8SCNIArgs(args.Args)
	if err != nil {
		return err
	}
	for networkType, delegate := range conf.NetworkType {
		_, err := delegateCmd(kvMap[k8s.K8S_POD_INFRA_CONTAINER_ID], delegate, false)
		if err != nil {
			return fmt.Errorf("failed to delegate delete network %s: %v", networkType, err)
		}
		ports, err := consumePort(kvMap[k8s.K8S_POD_INFRA_CONTAINER_ID])
		if err != nil {
			return fmt.Errorf("failed to read ports %v", err)
		}
		if len(ports) != 0 {
			if err := cleanPortMapping("cni0", ports); err != nil {
				return fmt.Errorf("failed to delete port mapping %v: %v", ports, err)
			}
		}
		//TODO send http request for l5 setup
		return nil
	}
	return nil
}

func delegateCmd(cid string, netconf map[string]interface{}, add bool) (*types.Result, error) {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return nil, fmt.Errorf("error serializing delegate netconf: %v", err)
	}

	if add {
		result, err := invoke.DelegateAdd(netconf["type"].(string), netconfBytes)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, invoke.DelegateDel(netconf["type"].(string), netconfBytes)
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.Legacy)
}

func savePort(containerID string, portStr string) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, containerID)
	return ioutil.WriteFile(path, []byte(portStr), 0600)
}

func consumePort(containerID string) ([]*k8s.Port, error) {
	path := filepath.Join(stateDir, containerID)
	defer os.Remove(path)

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var ports []*k8s.Port
	if err := json.Unmarshal(data, &ports); err != nil {
		return nil, err
	}
	return ports, nil
}
