/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2019 StackPath, LLC
 *
 */

// Adds support to configure SR-IOV interfaces within a VM through cloud-init
// network version 1 configuration. Other interface types such as bridge
// are configured within the VM by binding a DHCP server to the bridge
// source interface in the compute container. This is not possible for
// SR-IOV network interfaces as there is nothing in the compute container
// to bind a DHCP server to.

// Other network interface types can be added to this logic but are
// currently already handled with existing code.

package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"gopkg.in/yaml.v2"

	"kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/log"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/network"
)

// CloudInitSubnetRoute is a representation of cloud-init nework config v1
// Route objects
type CloudInitSubnetRoute struct {
	Network string `yaml:"network,omitempty"`
	Netmask string `yaml:"netmask,omitempty"`
	Gateway string `yaml:"gateway,omitempty"`
}

// CloudInitSubnet is a representation of a cloud-init nework config v1
// subnet object
type CloudInitSubnet struct {
	SubnetType string                 `yaml:"type,omitempty"`
	Address    string                 `yaml:"address,omitempty"`
	Gateway    string                 `yaml:"gateway,omitempty"`
	Routes     []CloudInitSubnetRoute `yaml:"routes,omitempty"`
}

// CloudInitNetworkInterface is a representation of a cloud-init nework config v1
// network interface
type CloudInitNetworkInterface struct {
	NetworkType string            `yaml:"type"`
	Name        string            `yaml:"name,omitempty"`
	MacAddress  string            `yaml:"mac_address,omitempty"`
	Mtu         uint16            `yaml:"mtu,omitempty"`
	Subnets     []CloudInitSubnet `yaml:"subnets,omitempty"`
	Address     []string          `yaml:"address,omitempty"`
	Search      []string          `yaml:"search,omitempty"`
	Destination string            `yaml:"destination,omitempty"`
	Gateway     string            `yaml:"gateway,omitempty"`
	Metric      int               `yaml:"metric,omitempty"`
}

// CloudInitNetConfig is a representation of a cloud-init nework config v1
type CloudInitNetConfig struct {
	Version int                         `yaml:"version"`
	Config  []CloudInitNetworkInterface `yaml:"config"`
}

// CloudInitManageResolv is a representation of a cloud-init
// manage_resolv_conf object
type CloudInitManageResolv struct {
	ManageResolv bool                `yaml:"manage_resolv_conf,omitempty"`
	ResolvConf   CloudInitResolvConf `yaml:"resolv_conf,omitempty"`
}

// CloudInitResolvConf is a representation of a cloud-init
// resolver configuration object
type CloudInitResolvConf struct {
	NameServers   []string `yaml:"nameservers,omitempty"`
	SearchDomains []string `yaml:"searchdomains,omitempty"`
	Domain        string   `yaml:"domain,omitempty"`
	// TODO Add options map when pkg/util/net/dns can parse them
}

var disableResolv bool
var getResolvConfDetailsFromPod = api.GetResolvConfDetailsFromPod

// Inspired by Convert_v1_VirtualMachine_To_api_Domain
func setNetworkInfo(vmi *v1.VirtualMachineInstance) (map[string]*v1.Network, map[string]int) {
	networks := map[string]*v1.Network{}
	cniNetworks := map[string]int{}
	multusNetworkIndex := 1

	for _, vmiNetwork := range vmi.Spec.Networks {
		if vmiNetwork.Multus != nil {
			if vmiNetwork.Multus.Default {
				// default network is eth0
				cniNetworks[vmiNetwork.Name] = 0
			} else {
				cniNetworks[vmiNetwork.Name] = multusNetworkIndex
				multusNetworkIndex++
			}
		}
		if vmiNetwork.Genie != nil {
			cniNetworks[vmiNetwork.Name] = len(cniNetworks)
		}
		networks[vmiNetwork.Name] = vmiNetwork.DeepCopy()
	}
	return networks, cniNetworks
}

func getSriovNetworkInfo(vmi *v1.VirtualMachineInstance) ([]network.VIF, error) {
	var sriovVifs []network.VIF

	networks, cniNetworks := setNetworkInfo(vmi)

	for _, iface := range vmi.Spec.Domain.Devices.Interfaces {
		net, isExist := networks[iface.Name]
		if !isExist {
			return sriovVifs, fmt.Errorf("failed to find network %s", iface.Name)
		}

		if iface.Bridge != nil || iface.Masquerade != nil {
			disableResolv = true
		}

		if value, ok := cniNetworks[iface.Name]; ok {
			prefix := ""
			// no error check, we assume that CNI type was set correctly
			if net.Multus != nil {
				if net.Multus.Default {
					// Default network is eth0
					prefix = "eth"
				} else {
					prefix = "net"
				}
			} else if net.Genie != nil {
				prefix = "eth"
			}
			if iface.SRIOV != nil {
				details, err := getNetworkDetails(fmt.Sprintf("%s%d", prefix, value))
				if err != nil {
					log.Log.Reason(err).Errorf("failed to get SR-IOV network details for %s", fmt.Sprintf("%s%d", prefix, value))
					return sriovVifs, err
				}
				sriovVifs = append(sriovVifs, details)
			}
		}
	}
	if len(sriovVifs) == 0 {
		err := fmt.Errorf("No SRIOV interfaces found")
		return sriovVifs, err
	}
	return sriovVifs, nil
}

// Scavenged from various parts of podnetwork and BridgePodInterface
func getNetworkDetails(intName string) (network.VIF, error) {
	log.Log.V(2).Infof("starting discovery for: %s", intName)
	if network.Handler == nil {
		network.Handler = &network.NetworkUtilsHandler{}
	}

	var vif network.VIF

	vif.Name = intName

	link, err := network.Handler.LinkByName(vif.Name)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", vif.Name)
		return vif, err
	}

	addrList, err := network.Handler.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get an ip address for %s", vif.Name)
		return vif, err
	}

	if len(addrList) > 0 {
		vif.IP = addrList[0]
	}

	if len(vif.MAC) == 0 {
		mac, err := network.Handler.GetMacDetails(vif.Name)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to get MAC for %s", vif.Name)
			return vif, err
		}
		vif.MAC = mac
	}

	routes, err := network.Handler.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get routes for %s", vif.Name)
		return vif, err
	}
	vif.Routes = &routes

	vif.Mtu = uint16(link.Attrs().MTU)

	return vif, nil
}

func getCloudInitManageResolv() (CloudInitManageResolv, error) {
	var cloudInitManageResolv CloudInitManageResolv
	var cloudInitResolvConf CloudInitResolvConf

	// Skip discovering resolv data if dhcp interface type is present
	if disableResolv {
		log.Log.V(2).Info("found dhcp interface: skipping resolv discovery")
		return cloudInitManageResolv, nil
	}

	nameServers, searchDomains, err := getResolvConfDetailsFromPod()
	if err != nil {
		log.Log.Errorf("Failed to get DNS servers from resolv.conf: %v", err)
		return cloudInitManageResolv, err
	}

	cloudInitManageResolv.ManageResolv = true

	for _, nameServer := range nameServers {
		cloudInitResolvConf.NameServers = append(cloudInitResolvConf.NameServers, net.IP(nameServer).String())
	}

	for _, searchDomain := range searchDomains {
		cloudInitResolvConf.SearchDomains = append(cloudInitResolvConf.SearchDomains, searchDomain)
	}

	cloudInitManageResolv.ResolvConf = cloudInitResolvConf

	return cloudInitManageResolv, nil
}

func convertCloudInitNetworksToCloudInitNetConfig(cloudInitNetworks *[]network.VIF, config *CloudInitNetConfig) {
	for _, vif := range *cloudInitNetworks {
		var nif CloudInitNetworkInterface
		var nifSubnet CloudInitSubnet
		var nifRoutes []CloudInitSubnetRoute

		nif.Name = vif.Name
		nif.NetworkType = "physical"
		nif.MacAddress = vif.MAC.String()
		nif.Mtu = vif.Mtu

		if vif.IP.String() == "<nil>" {
			nifSubnet.SubnetType = "manual"
			nif.Subnets = append(nif.Subnets, nifSubnet)
		} else {
			nifSubnet.SubnetType = "static"
			nifSubnet.Address = strings.Split(vif.IP.String(), " ")[0]
			for _, route := range *vif.Routes {
				if route.Dst == nil && route.Src.Equal(nil) && route.Gw.Equal(nil) {
					continue
				}

				if route.Src != nil && route.Src.Equal(vif.IP.IP) {
					continue
				}

				var subnetRoute CloudInitSubnetRoute

				if route.Dst == nil {
					nifSubnet.Gateway = route.Gw.String()
					continue
				} else {
					subnetRoute.Network = route.Dst.IP.String()
				}

				subnetRoute.Network = route.Dst.IP.String()
				subnetRoute.Netmask = net.IP(route.Dst.Mask).String()
				if route.Gw != nil {
					subnetRoute.Gateway = route.Gw.String()
				}
				nifRoutes = append(nifRoutes, subnetRoute)
			}
			nifSubnet.Routes = nifRoutes
			nif.Subnets = append(nif.Subnets, nifSubnet)
		}
		config.Config = append(config.Config, nif)
	}
}

func cloudInitDiscoverNetworkData(vmi *v1.VirtualMachineInstance) ([]byte, []byte, error) {
	var networkFile []byte
	var resolvFile []byte
	var cloudInitNetworks []network.VIF

	cloudInitNetworks, err := getSriovNetworkInfo(vmi)
	if err != nil {
		return networkFile, resolvFile, err
	}

	if len(cloudInitNetworks) == 0 {
		return networkFile, resolvFile, err
	}

	var config = CloudInitNetConfig{
		Version: 1,
	}

	convertCloudInitNetworksToCloudInitNetConfig(&cloudInitNetworks, &config)

	networkFile, err = yaml.Marshal(config)
	if err != nil {
		return networkFile, resolvFile, err
	}

	cloudInitManageResolv, err := getCloudInitManageResolv()
	if err != nil {
		return networkFile, resolvFile, err
	}

	if cloudInitManageResolv.ManageResolv {
		resolvFile, err = yaml.Marshal(cloudInitManageResolv)
		if err != nil {
			return networkFile, resolvFile, err
		}
	}

	return networkFile, resolvFile, err
}
