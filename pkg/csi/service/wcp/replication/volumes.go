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
	"net/url"
	pathpkg "path"
	"strings"

	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
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
	// DatastoreMoID returns the managed object ID of the datastore with the given URL.
	DatastoreMoID(ctx context.Context, datastoreURL string) (string, error)
	// FindDatastoreWithFile returns the managed object ID of an accessible
	// datastore that has a file at path, relative to the datastore root.
	FindDatastoreWithFile(ctx context.Context, path string) (string, error)
	// DiskURLPath returns the CNS disk URL of a file on a datastore, in the
	// form https://<vc>/folder/<path>?dcPath=<datacenter>&dsName=<datastore>.
	DiskURLPath(ctx context.Context, datastoreMoID, path string) (string, error)
	// RecoveryPlacement returns the VM folder and resource pool that HMS
	// requires for a recovery, as managed object IDs.
	RecoveryPlacement(ctx context.Context) (folder, pool string, err error)
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
	containerCluster := cnsvsphere.GetContainerCluster(clusterID, m.VcenterConfig.Username,
		cnstypes.CnsClusterFlavorWorkload, m.CnsConfig.Global.ClusterDistribution)
	spec := &cnstypes.CnsVolumeCreateSpec{
		Name:       "replica-" + volumeID,
		VolumeType: common.BlockVolumeType,
		VolumeId:   &cnstypes.CnsVolumeId{Id: volumeID},
		Metadata: cnstypes.CnsVolumeMetadata{
			ContainerCluster: containerCluster,
			// The volume manager reads ContainerClusterArray[0] on WCP.
			ContainerClusterArray: []cnstypes.CnsContainerCluster{containerCluster},
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

func (b *cnsVolumeBackend) vcClient(ctx context.Context) (*vim25.Client, *cnsvsphere.VirtualCenter, error) {
	vc, err := common.GetVCenter(ctx, b.manager())
	if err != nil {
		return nil, nil, err
	}
	return vc.Client.Client, vc, nil
}

func (b *cnsVolumeBackend) DatastoreMoID(ctx context.Context, datastoreURL string) (string, error) {
	c, _, err := b.vcClient(ctx)
	if err != nil {
		return "", err
	}
	v, err := view.NewManager(c).CreateContainerView(ctx, c.ServiceContent.RootFolder,
		[]string{"Datastore"}, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = v.Destroy(ctx) }()
	var dss []mo.Datastore
	if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"summary.url"}, &dss); err != nil {
		return "", err
	}
	for _, ds := range dss {
		if ds.Summary.Url == datastoreURL {
			return ds.Reference().Value, nil
		}
	}
	return "", fmt.Errorf("datastore with URL %q not found", datastoreURL)
}

func (b *cnsVolumeBackend) FindDatastoreWithFile(ctx context.Context, path string) (string, error) {
	c, _, err := b.vcClient(ctx)
	if err != nil {
		return "", err
	}
	v, err := view.NewManager(c).CreateContainerView(ctx, c.ServiceContent.RootFolder,
		[]string{"Datastore"}, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = v.Destroy(ctx) }()
	var dss []mo.Datastore
	if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"name", "browser", "summary.accessible"},
		&dss); err != nil {
		return "", err
	}
	dir, file := pathpkg.Split(path)
	var denied []string
	for _, ds := range dss {
		if !ds.Summary.Accessible {
			continue
		}
		browser := object.NewHostDatastoreBrowser(c, ds.Browser)
		task, err := browser.SearchDatastore(ctx, fmt.Sprintf("[%s] %s", ds.Name, dir),
			&vimtypes.HostDatastoreBrowserSearchSpec{MatchPattern: []string{file}})
		if err != nil {
			// The CSI user may not have Datastore.Browse on every datastore.
			denied = append(denied, ds.Name)
			continue
		}
		info, err := task.WaitForResult(ctx, nil)
		if err != nil {
			// FileNotFound when the directory does not exist on this datastore.
			continue
		}
		if res, ok := info.Result.(vimtypes.HostDatastoreBrowserSearchResults); ok && len(res.File) > 0 {
			return ds.Reference().Value, nil
		}
	}
	return "", fmt.Errorf("no accessible datastore has %s (could not search %v)", path, denied)
}

// datacenterOf walks up the inventory from ref to its datacenter.
func datacenterOf(ctx context.Context, c *vim25.Client, ref vimtypes.ManagedObjectReference) (
	*mo.Datacenter, error) {
	pc := property.DefaultCollector(c)
	for ref.Type != "Datacenter" {
		var e mo.ManagedEntity
		if err := pc.RetrieveOne(ctx, ref, []string{"parent"}, &e); err != nil {
			return nil, err
		}
		if e.Parent == nil {
			return nil, fmt.Errorf("%s is not in a datacenter", ref)
		}
		ref = *e.Parent
	}
	var dc mo.Datacenter
	if err := pc.RetrieveOne(ctx, ref, []string{"name", "vmFolder"}, &dc); err != nil {
		return nil, err
	}
	return &dc, nil
}

func (b *cnsVolumeBackend) DiskURLPath(ctx context.Context, datastoreMoID, path string) (string, error) {
	c, vc, err := b.vcClient(ctx)
	if err != nil {
		return "", err
	}
	ref := vimtypes.ManagedObjectReference{Type: "Datastore", Value: datastoreMoID}
	var ds mo.Datastore
	if err := property.DefaultCollector(c).RetrieveOne(ctx, ref, []string{"name"}, &ds); err != nil {
		return "", fmt.Errorf("datastore %s: %w", datastoreMoID, err)
	}
	dc, err := datacenterOf(ctx, c, ref)
	if err != nil {
		return "", err
	}
	q := url.Values{"dcPath": {dc.Name}, "dsName": {ds.Name}}
	u := url.URL{Scheme: "https", Host: vc.Config.Host.String(), Path: "/folder/" + path, RawQuery: q.Encode()}
	return u.String(), nil
}

// RecoveryPlacement uses the first vSphere cluster of the Supervisor.
func (b *cnsVolumeBackend) RecoveryPlacement(ctx context.Context) (string, string, error) {
	c, _, err := b.vcClient(ctx)
	if err != nil {
		return "", "", err
	}
	clusters, _, err := common.GetClusterComputeResourceMoIds(ctx)
	if err != nil || len(clusters) == 0 {
		// Supervisors without zones identify their cluster in the config.
		id, _, _ := strings.Cut(b.manager().CnsConfig.Global.ClusterID, ":")
		if id == "" {
			return "", "", fmt.Errorf("no vSphere cluster found for the Supervisor: %v", err)
		}
		clusters = []string{id}
	}
	ref := vimtypes.ManagedObjectReference{Type: "ClusterComputeResource", Value: clusters[0]}
	var cluster mo.ClusterComputeResource
	if err := property.DefaultCollector(c).RetrieveOne(ctx, ref, []string{"resourcePool"}, &cluster); err != nil {
		return "", "", fmt.Errorf("cluster %s: %w", clusters[0], err)
	}
	if cluster.ResourcePool == nil {
		return "", "", fmt.Errorf("cluster %s has no resource pool", clusters[0])
	}
	dc, err := datacenterOf(ctx, c, ref)
	if err != nil {
		return "", "", err
	}
	return dc.VmFolder.Value, cluster.ResourcePool.Value, nil
}
