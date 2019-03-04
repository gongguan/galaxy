package cniutil

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.code.oa.com/gaiastack/galaxy/pkg/api/galaxy/constant"
	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	t020 "github.com/containernetworking/cni/pkg/types/020"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/golang/glog"
	"github.com/vishvananda/netlink"
)

const (
	// CNI_ARGS="IP=192.168.33.3"
	// CNI_COMMAND="ADD"
	// CNI_CONTAINERID=ctn1
	// CNI_NETNS=/var/run/netns/ctn
	// CNI_IFNAME=eth0
	// CNI_PATH=$CNI_PATH
	CNI_ARGS        = "CNI_ARGS"
	CNI_COMMAND     = "CNI_COMMAND"
	CNI_CONTAINERID = "CNI_CONTAINERID"
	CNI_NETNS       = "CNI_NETNS"
	CNI_IFNAME      = "CNI_IFNAME"
	CNI_PATH        = "CNI_PATH"

	COMMAND_ADD = "ADD"
	COMMAND_DEL = "DEL"
)

type Uint16 uint16

func (n *Uint16) UnmarshalText(data []byte) error {
	u, err := strconv.ParseUint(string(data), 10, 16)
	if err != nil {
		return fmt.Errorf("failed to parse uint16 %s", string(data))
	}
	*n = Uint16(uint16(u))
	return nil
}

func BuildCNIArgs(args map[string]string) string {
	var entries []string
	for k, v := range args {
		entries = append(entries, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(entries, ";")
}

func ParseCNIArgs(args string) (map[string]string, error) {
	kvMap := make(map[string]string)
	kvs := strings.Split(args, ";")
	if len(kvs) == 0 {
		return kvMap, fmt.Errorf("invalid args %s", args)
	}
	for _, kv := range kvs {
		part := strings.SplitN(kv, "=", 2)
		if len(part) != 2 {
			continue
		}
		kvMap[strings.TrimSpace(part[0])] = strings.TrimSpace(part[1])
	}
	return kvMap, nil
}

func DelegateAdd(netconf map[string]interface{}, args *skel.CmdArgs, ifName string) (types.Result, error) {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return nil, fmt.Errorf("error serializing delegate netconf: %v", err)
	}
	pluginPath, err := invoke.FindInPath(netconf["type"].(string), strings.Split(args.Path, ":"))
	if err != nil {
		return nil, err
	}
	glog.Infof("delegate add %s args %s conf %s", args.ContainerID, args.Args, string(netconfBytes))
	return invoke.ExecPluginWithResult(pluginPath, netconfBytes, &invoke.Args{
		Command:       "ADD",
		ContainerID:   args.ContainerID,
		NetNS:         args.Netns,
		PluginArgsStr: args.Args,
		IfName:        ifName,
		Path:          args.Path,
	})
}

func DelegateDel(netconf map[string]interface{}, args *skel.CmdArgs, ifName string) error {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return fmt.Errorf("error serializing delegate netconf: %v", err)
	}
	pluginPath, err := invoke.FindInPath(netconf["type"].(string), strings.Split(args.Path, ":"))
	if err != nil {
		return err
	}
	glog.Infof("delegate del %s args %s conf %s", args.ContainerID, args.Args, string(netconfBytes))
	return invoke.ExecPluginWithoutResult(pluginPath, netconfBytes, &invoke.Args{
		Command:       "DEL",
		ContainerID:   args.ContainerID,
		NetNS:         args.Netns,
		PluginArgsStr: args.Args,
		IfName:        ifName,
		Path:          args.Path,
	})
}

func CmdAdd(cmdArgs *skel.CmdArgs, netConf map[string]map[string]interface{}, networkInfos []*NetworkInfo) (types.Result, error) {
	if len(networkInfos) == 0 {
		return nil, fmt.Errorf("No network info returned")
	}
	var (
		err    error
		result types.Result
	)
	for idx, networkInfo := range networkInfos {
		for t, v := range *networkInfo {
			conf, ok := netConf[t]
			if !ok {
				return nil, fmt.Errorf("network %s not configured", t)
			}
			//append additional args from network info
			cmdArgs.Args = fmt.Sprintf("%s;%s", cmdArgs.Args, BuildCNIArgs(v))
			var ifName string
			ifName = v["IfName"]
			result, err = DelegateAdd(conf, cmdArgs, ifName)
			if err != nil {
				//fail to add cni, then delete all established CNIs recursively
				glog.Errorf("fail to add network %s: %v, then rollback and delete it", v, err)
				delErr := CmdDel(cmdArgs, netConf, networkInfos, idx)
				glog.Warningf("fail to delete cni in rollback %v", delErr)
				return nil, fmt.Errorf("fail to establish network %s:%v", v, err)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

type NetworkInfo map[string]map[string]string

func CmdDel(cmdArgs *skel.CmdArgs, netConf map[string]map[string]interface{}, networkInfos []*NetworkInfo, lastIdx int) error {
	var errorSet []string
	for idx := lastIdx; idx >= 0; idx-- {
		networkInfo := networkInfos[idx]
		for t, v := range *networkInfo {
			conf, ok := netConf[t]
			if !ok {
				return fmt.Errorf("network %s not configured", t)
			}
			//append additional args from network info
			cmdArgs.Args = fmt.Sprintf("%s;%s", cmdArgs.Args, BuildCNIArgs(v))
			var ifName string
			ifName = v["IfName"]
			err := DelegateDel(conf, cmdArgs, ifName)
			if err != nil {
				errorSet = append(errorSet, err.Error())
				glog.Errorf("failed to delete network %v: %v", v, err)
			}
		}
	}
	if len(errorSet) > 0 {
		return fmt.Errorf(strings.Join(errorSet, " / "))
	}
	return nil
}

const (
	stateDir = "/var/lib/cni/galaxy"
)

func SaveNetworkInfo(containerID string, info NetworkInfo) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, containerID)
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, data, 0600)
}

func ConsumeNetworkInfo(containerID string) (NetworkInfo, error) {
	m := make(map[string]map[string]string)
	path := filepath.Join(stateDir, containerID)
	defer os.Remove(path) // nolint: errcheck

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

func IPInfoToResult(ipInfo *constant.IPInfo) *t020.Result {
	return &t020.Result{
		IP4: &t020.IPConfig{
			IP:      net.IPNet(*ipInfo.IP),
			Gateway: ipInfo.Gateway,
			Routes: []types.Route{{
				Dst: net.IPNet{
					IP:   net.IPv4(0, 0, 0, 0),
					Mask: net.IPv4Mask(0, 0, 0, 0),
				},
			}},
		},
	}
}

// ConfigureIface takes the result of IPAM plugin and
// applies to the ifName interface
func ConfigureIface(ifName string, res *t020.Result) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", ifName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set %q UP: %v", ifName, err)
	}

	// TODO(eyakubovich): IPv6
	addr := &netlink.Addr{IPNet: &res.IP4.IP, Label: ""}
	if err = netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add IP addr to %q: %v", ifName, err)
	}

	for _, r := range res.IP4.Routes {
		gw := r.GW
		if gw == nil {
			gw = res.IP4.Gateway
		}
		if err = ip.AddRoute(&r.Dst, gw, link); err != nil {
			// we skip over duplicate routes as we assume the first one wins
			if !os.IsExist(err) {
				return fmt.Errorf("failed to add route '%v via %v dev %v': %v", r.Dst, gw, ifName, err)
			}
		}
	}

	return nil
}
