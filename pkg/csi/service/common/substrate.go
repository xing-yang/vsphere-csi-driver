/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

// Agent Substrate (github.com/agent-substrate/substrate) provisions a dedicated volume per
// actor by calling the CSI controller and node services directly. It creates no PVC, PV or
// VolumeAttachment objects and runs none of the CSI sidecars, so:
//   - CreateVolume receives the raw StorageClass parameters, including keys reserved for
//     Kubernetes (csi.storage.k8s.io/*) that external-provisioner would normally strip, and
//     none of the PVC/PV metadata parameters.
//   - ControllerPublishVolume/ControllerUnpublishVolume are driven by Substrate on every
//     actor resume/suspend, with the Kubernetes node name as NodeId.

import (
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	// SubstrateVolumeNamePrefix is the prefix of every CreateVolume name Agent Substrate sends:
	// "substrate-<actorUID>-<volumeName>".
	SubstrateVolumeNamePrefix = "substrate-"

	// SubstrateVolumeLabel marks Supervisor PVCs created for Agent Substrate volumes.
	SubstrateVolumeLabel = "cns.vmware.com/agent-substrate"

	// kubernetesReservedParamPrefix is the StorageClass parameter prefix reserved for
	// Kubernetes. external-provisioner removes these keys before calling CreateVolume.
	kubernetesReservedParamPrefix = "csi.storage.k8s.io/"

	// kubernetesFsTypeParam is the reserved StorageClass parameter for the filesystem type.
	kubernetesFsTypeParam = kubernetesReservedParamPrefix + "fstype"
)

// IsSubstrateCreateRequest reports whether req comes from Agent Substrate instead of
// external-provisioner. The volume name must carry the Substrate prefix and none of the
// PVC/PV metadata parameters may be present, so a PVC that happens to be named
// "substrate-..." still takes the normal path.
func IsSubstrateCreateRequest(req *csi.CreateVolumeRequest) bool {
	if req == nil || !strings.HasPrefix(req.GetName(), SubstrateVolumeNamePrefix) {
		return false
	}
	for param := range req.GetParameters() {
		switch strings.ToLower(param) {
		case AttributePvcName, AttributePvcNamespace, AttributePvName:
			return false
		}
	}
	return true
}

// NormalizeSubstrateParams returns a copy of params without the keys reserved for
// Kubernetes (csi.storage.k8s.io/*), matching what external-provisioner does before calling
// CreateVolume.
//
// Substrate passes the filesystem type only to CreateVolume, through the StorageClass, and
// never to the node: its node calls always carry an empty fsType, so the node formats volumes
// with the default ext4. Rather than silently provision a different filesystem than the
// StorageClass asks for, any fstype other than ext4 is rejected.
func NormalizeSubstrateParams(params map[string]string) (map[string]string, error) {
	normalized := make(map[string]string, len(params))
	for param, value := range params {
		lower := strings.ToLower(param)
		if !strings.HasPrefix(lower, kubernetesReservedParamPrefix) {
			normalized[param] = value
			continue
		}
		if lower == kubernetesFsTypeParam && value != "" && !strings.EqualFold(value, Ext4FsType) {
			return nil, fmt.Errorf("StorageClass parameter %s=%q is not supported for Agent Substrate "+
				"volumes: Agent Substrate does not pass the filesystem type to the node, so only %q is supported",
				param, value, Ext4FsType)
		}
	}
	return normalized, nil
}
