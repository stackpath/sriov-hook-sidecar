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

// Inspired by cmd/example-hook-sidecar

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"

	v1 "kubevirt.io/kubevirt/pkg/api/v1"
	hooks "kubevirt.io/kubevirt/pkg/hooks"
	hooksInfo "kubevirt.io/kubevirt/pkg/hooks/info"
	hooksV1alpha2 "kubevirt.io/kubevirt/pkg/hooks/v1alpha2"
	"kubevirt.io/kubevirt/pkg/log"
)

var HostsIpAddress string

type infoServer struct{}

func (s infoServer) Info(ctx context.Context, params *hooksInfo.InfoParams) (*hooksInfo.InfoResult, error) {
	log.Log.Info("Hook's Info method has been called")

	return &hooksInfo.InfoResult{
		Name: "cloudinit",
		Versions: []string{
			hooksV1alpha2.Version,
		},
		HookPoints: []*hooksInfo.HookPoint{
			{
				Name:     hooksInfo.PreCloudInitIsoHookPointName,
				Priority: 0,
			},
		},
	}, nil
}

type v1alpha2Server struct{}

func getCloudInitData(params *hooksV1alpha2.PreCloudInitIsoParams) (*v1.CloudInitNoCloudSource, *v1.VirtualMachineInstance) {
	vmiJSON := params.GetVmi()
	vmi := v1.VirtualMachineInstance{}
	err := json.Unmarshal(vmiJSON, &vmi)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to unmarshal given VMI spec: %s", vmiJSON)
		panic(err)
	}

	cloudInitDataJSON := params.GetCloudInitData()
	cloudInitData := v1.CloudInitNoCloudSource{}
	err = json.Unmarshal(cloudInitDataJSON, &cloudInitData)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to unmarshal given CloudInitNoCloudSource: %s", cloudInitDataJSON)
		panic(err)
	}
	return &cloudInitData, &vmi
}

func setUserData(cloudInitData *v1.CloudInitNoCloudSource) ([]byte, error) {
	var userData []byte
	if cloudInitData.UserData != "" {
		log.Log.V(2).Info("Found UserData")
		userData = []byte(cloudInitData.UserData)
	} else if cloudInitData.UserDataBase64 != "" {
		log.Log.V(2).Info("Found UserDataBase64")
		userData, err := base64.StdEncoding.DecodeString(cloudInitData.UserDataBase64)
		if err != nil {
			return userData, err
		}
	}
	if len(userData) == 0 {
		log.Log.V(2).Info("no userData found: adding #cloud-config header")
		userData = []byte("#cloud-config\n")
		return userData, nil
	}
	return userData, nil
}

func setAdditionalData(hostname string, resolvData, userData []byte) []byte {
	if len(resolvData) > 0 {
		log.Log.V(2).Info("attempting to append resolvData to userData")
		if strings.HasPrefix(string(userData), "#cloud-config") {
			// Check if it already contains manage_resolv_conf
			if bytes.Contains(userData, []byte("manage_resolv_conf:")) {
				log.Log.V(2).Info("skipping append: manage_resolv_conf found in userData")
			} else if len(resolvData) > 0 {
				log.Log.V(2).Info("appending resolv configuration to userData")
				userData = append(userData, []byte("\n")...)
				userData = append(userData, resolvData...)
			}
		} else {
			log.Log.V(2).Info("skipping append for resolvData: #cloud-config header not in userData ")
		}
	}

	if len(HostsIpAddress) > 0 {
		log.Log.V(2).Info("Attemping to append bootcmd for /etc/hosts to userData")
		if strings.HasPrefix(string(userData), "#cloud-config") {
			log.Log.V(2).Info("Appending bootcmd for /etc/hosts to userData")
			bootStr := "bootcmd:\n  - cloud-init-per instance etcHosts sh -c \"echo " + HostsIpAddress + " " + hostname + " >> /etc/hosts\"\n"
			userData = append(userData, []byte(bootStr)...)
		} else {
			log.Log.V(2).Info("skipping append for bootcmd: #cloud-config header not in userData ")
		}
	}

	return userData
}

func (s v1alpha2Server) PreCloudInitIso(ctx context.Context, params *hooksV1alpha2.PreCloudInitIsoParams) (*hooksV1alpha2.PreCloudInitIsoResult, error) {
	log.Log.Info("Hook's PreCloudInitIso callback method has been called")

	var cloudInitData *v1.CloudInitNoCloudSource
	var vmi *v1.VirtualMachineInstance
	cloudInitData, vmi = getCloudInitData(params)

	if cloudInitData.NetworkData != "" || cloudInitData.NetworkDataBase64 != "" || cloudInitData.NetworkDataSecretRef != nil {
		log.Log.Warning("Skipping SR-IOV network discovery: cloud-init networkData is already defined")
		return &hooksV1alpha2.PreCloudInitIsoResult{
			CloudInitData: params.GetCloudInitData(),
		}, nil
	}

	networkData, resolvData, err := cloudInitDiscoverNetworkData(vmi)
	if err != nil {
		log.Log.Reason(err).Errorf("cloudInitDiscoverNetworkData Failed")
		panic(err)
	}

	userData, err := setUserData(cloudInitData)
	if err != nil {
		return &hooksV1alpha2.PreCloudInitIsoResult{
			CloudInitData: params.GetCloudInitData(),
		}, err
	}

	userData = setAdditionalData(vmi.Spec.Hostname, resolvData, userData)

	cloudInitData.UserDataBase64 = base64.StdEncoding.EncodeToString([]byte(userData))
	cloudInitData.NetworkDataBase64 = base64.StdEncoding.EncodeToString([]byte(networkData))
	cloudInitData.UserData = ""

	response, err := json.Marshal(cloudInitData)
	if err != nil {
		return &hooksV1alpha2.PreCloudInitIsoResult{
			CloudInitData: params.GetCloudInitData(),
		}, fmt.Errorf("Failed to marshal CloudInitNoCloudSource: %v", cloudInitData)

	}

	return &hooksV1alpha2.PreCloudInitIsoResult{
		CloudInitData: response,
	}, nil
}

func (s v1alpha2Server) OnDefineDomain(ctx context.Context, params *hooksV1alpha2.OnDefineDomainParams) (*hooksV1alpha2.OnDefineDomainResult, error) {
	log.Log.Warning("Hook's OnDefineDomain callback method has been called which should never happen")
	return &hooksV1alpha2.OnDefineDomainResult{
		DomainXML: params.GetDomainXML(),
	}, nil
}

func main() {
	log.InitializeLogging("cloudinit-hook-sidecar")

	socketPath := hooks.HookSocketsSharedDirectory + "/sriov-discovery.sock"
	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to initialized socket on path: %s", socket)
		log.Log.Error("Check whether given directory exists and socket name is not already taken by other file")
		panic(err)
	}
	defer os.Remove(socketPath)

	server := grpc.NewServer([]grpc.ServerOption{}...)
	hooksInfo.RegisterInfoServer(server, infoServer{})
	hooksV1alpha2.RegisterCallbacksServer(server, v1alpha2Server{})
	log.Log.Infof("Starting hook server exposing 'info' and 'v1alpha2' services on socket %s", socketPath)
	server.Serve(socket)
}
