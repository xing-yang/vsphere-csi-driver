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

package replication

import (
	"context"
	"fmt"

	cnstypes "github.com/vmware/govmomi/cns/types"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common/commonco"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

// VolumeInfo is the CNS view of a volume needed for replication.
type VolumeInfo struct {
	ID              string
	VolumeType      string
	DatastoreURL    string
	StoragePolicyID string
}

// VolumeBackend is the CNS and Kubernetes side of replication.
type VolumeBackend interface {
	// GetVolume returns the CNS volume, or nil if it does not exist.
	GetVolume(ctx context.Context, volumeID string) (*VolumeInfo, error)
	// IsAttached returns whether the volume is attached to any node or VM.
	IsAttached(ctx context.Context, volumeID string) (bool, error)
	// RegisterVolume registers an existing disk with CNS under the given volume ID.
	RegisterVolume(ctx context.Context, volumeID, diskURLPath, storagePolicyID string) error
}

// cnsVolumeBackend implements VolumeBackend with CNS and the Kubernetes API.
type cnsVolumeBackend struct {
	// manager returns the current manager; it is replaced on config reload.
	manager   func() *common.Manager
	k8sClient kubernetes.Interface
}

// NewCNSVolumeBackend returns a VolumeBackend backed by CNS.
func NewCNSVolumeBackend(manager func() *common.Manager, k8sClient kubernetes.Interface) VolumeBackend {
	return &cnsVolumeBackend{
		manager:   manager,
		k8sClient: k8sClient,
	}
}

func (b *cnsVolumeBackend) GetVolume(ctx context.Context, volumeID string) (*VolumeInfo, error) {
	res, err := b.manager().VolumeManager.QueryVolume(ctx, cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{{Id: volumeID}},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Volumes) == 0 {
		return nil, nil
	}
	v := res.Volumes[0]
	return &VolumeInfo{
		ID:              v.VolumeId.Id,
		VolumeType:      v.VolumeType,
		DatastoreURL:    v.DatastoreUrl,
		StoragePolicyID: v.StoragePolicyId,
	}, nil
}

// IsAttached checks VolumeAttachments for the volume's PV.
// TODO: VM Service VMs attach through CnsNodeVmAttachment without a
// VolumeAttachment; use the VSLM disk associations instead.
func (b *cnsVolumeBackend) IsAttached(ctx context.Context, volumeID string) (bool, error) {
	pvName, ok := commonco.ContainerOrchestratorUtility.GetPVNameFromCSIVolumeID(volumeID)
	if !ok {
		return false, nil
	}
	vas, err := b.k8sClient.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, va := range vas.Items {
		if va.Spec.Attacher != csitypes.Name || va.Spec.Source.PersistentVolumeName == nil {
			continue
		}
		if *va.Spec.Source.PersistentVolumeName == pvName && va.Status.Attached {
			return true, nil
		}
	}
	return false, nil
}

// RegisterVolume uses the CNS same-ID registration that the
// VSphereDPLPModernApp capability enables: CNS locates the VMDK by URL and
// stamps the given volume ID onto it.
func (b *cnsVolumeBackend) RegisterVolume(ctx context.Context, volumeID, diskURLPath,
	storagePolicyID string) error {
	if !commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.VSphereDPLPModernApp) {
		return fmt.Errorf("registering volume %s with its source ID requires the %s capability",
			volumeID, common.VSphereDPLPModernApp)
	}
	m := b.manager()
	clusterID := m.CnsConfig.Global.ClusterID
	if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.TKGsHA) {
		clusterID = m.CnsConfig.Global.SupervisorID
	}
	spec := &cnstypes.CnsVolumeCreateSpec{
		Name:       "replica-" + volumeID,
		VolumeType: common.BlockVolumeType,
		VolumeId:   &cnstypes.CnsVolumeId{Id: volumeID},
		Metadata: cnstypes.CnsVolumeMetadata{
			ContainerCluster: cnsvsphere.GetContainerCluster(clusterID, m.VcenterConfig.Username,
				cnstypes.CnsClusterFlavorWorkload, m.CnsConfig.Global.ClusterDistribution),
		},
		BackingObjectDetails: &cnstypes.CnsBlockBackingDetails{
			BackingDiskUrlPath: diskURLPath,
		},
	}
	if storagePolicyID != "" {
		spec.Profile = []vimtypes.BaseVirtualMachineProfileSpec{
			&vimtypes.VirtualMachineDefinedProfileSpec{ProfileId: storagePolicyID},
		}
	}
	info, _, err := m.VolumeManager.CreateVolume(ctx, spec, nil)
	if err != nil {
		return err
	}
	if info.VolumeID.Id != volumeID {
		return fmt.Errorf("CNS registered disk %s as volume %s, expected %s",
			diskURLPath, info.VolumeID.Id, volumeID)
	}
	return nil
}
