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

package wcpguest

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/unittestcommon"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common/commonco"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

const (
	substrateTestTKCUID      = "8d9e8c5a-1f4b-4c0e-9a7e-3b2d1c0f9e8d"
	substrateTestTKCName     = "tkc-substrate"
	substrateTestSVNamespace = "test-namespace-substrate"
	substrateTestVolumeName  = "substrate-0b6f1c7e-2c1d-4f7e-9a51-2f4f7f1d9c3a-data"
	substrateTestNode        = "tkc-substrate-worker-0"
)

// newSubstrateController builds a guest controller backed by fake supervisor and guest
// clients, with the agent-substrate-support FSS set as requested.
func newSubstrateController(t *testing.T, fssEnabled bool) *controller {
	t.Helper()
	co, err := unittestcommon.GetFakeContainerOrchestratorInterface(common.Kubernetes)
	if err != nil {
		t.Fatalf("failed to build fake CO: %v", err)
	}
	if fssEnabled {
		if err := co.EnableFSS(context.Background(), common.AgentSubstrateSupportFSS); err != nil {
			t.Fatalf("failed to enable %s: %v", common.AgentSubstrateSupportFSS, err)
		}
	}
	commonco.ContainerOrchestratorUtility = co
	return &controller{
		supervisorClient:           testclient.NewClientset(),
		guestClient:                testclient.NewClientset(),
		supervisorNamespace:        substrateTestSVNamespace,
		tanzukubernetesClusterUID:  substrateTestTKCUID,
		tanzukubernetesClusterName: substrateTestTKCName,
	}
}

// substrateCreateRequest returns a CreateVolumeRequest as Agent Substrate sends it: the raw
// StorageClass parameters, no PVC/PV metadata parameters, and a single-node-writer mount.
func substrateCreateRequest(params map[string]string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          substrateTestVolumeName,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 * common.GbInBytes},
		Parameters:    params,
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}},
	}
}

// createVolumeAndBind runs CreateVolume and marks the supervisor PVC svPVCName as Bound once
// it is created, so that the call returns. It returns the bound supervisor PVC, if any.
func createVolumeAndBind(t *testing.T, c *controller, req *csi.CreateVolumeRequest,
	svPVCName string) (*v1.PersistentVolumeClaim, *csi.CreateVolumeResponse, error) {
	t.Helper()
	ctx := context.Background()
	type result struct {
		resp *csi.CreateVolumeResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.CreateVolume(ctx, req)
		done <- result{resp, err}
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-done:
			pvc, _ := c.supervisorClient.CoreV1().PersistentVolumeClaims(c.supervisorNamespace).Get(
				ctx, svPVCName, metav1.GetOptions{})
			return pvc, r.resp, r.err
		case <-deadline:
			t.Fatalf("CreateVolume did not return within timeout")
		case <-time.After(50 * time.Millisecond):
		}
		pvc, err := c.supervisorClient.CoreV1().PersistentVolumeClaims(c.supervisorNamespace).Get(
			ctx, svPVCName, metav1.GetOptions{})
		if err != nil || pvc.Status.Phase == v1.ClaimBound {
			continue
		}
		pvc.Status.Phase = v1.ClaimBound
		if _, err := c.supervisorClient.CoreV1().PersistentVolumeClaims(c.supervisorNamespace).Update(
			ctx, pvc, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to mark supervisor PVC bound: %v", err)
		}
	}
}

func TestSupervisorPVCNameFor(t *testing.T) {
	tests := []struct {
		name       string
		volumeName string
		substrate  bool
		expect     string
	}{
		{
			name:       "guest PV name",
			volumeName: "pvc-0b6f1c7e",
			expect:     substrateTestTKCUID + "-0b6f1c7e",
		},
		{
			name:       "substrate volume name",
			volumeName: substrateTestVolumeName,
			substrate:  true,
			expect:     substrateTestTKCUID + "-" + substrateTestVolumeName,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supervisorPVCNameFor(substrateTestTKCUID, tt.volumeName, tt.substrate); got != tt.expect {
				t.Errorf("supervisorPVCNameFor() = %q, want %q", got, tt.expect)
			}
		})
	}
}

// TestCreateVolume_Substrate verifies that an Agent Substrate request creates a labeled
// supervisor PVC named after the whole Substrate volume name, and that reserved StorageClass
// parameters forwarded by Substrate do not fail validation.
func TestCreateVolume_Substrate(t *testing.T) {
	c := newSubstrateController(t, true)
	req := substrateCreateRequest(map[string]string{
		common.AttributeSupervisorStorageClass: testStorageClass,
		"csi.storage.k8s.io/fstype":            "ext4",
	})
	wantSVPVCName := substrateTestTKCUID + "-" + substrateTestVolumeName

	pvc, resp, err := createVolumeAndBind(t, c, req, wantSVPVCName)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if resp.GetVolume().GetVolumeId() != wantSVPVCName {
		t.Errorf("VolumeId = %q, want %q", resp.GetVolume().GetVolumeId(), wantSVPVCName)
	}
	if pvc == nil {
		t.Fatalf("supervisor PVC %q was not created", wantSVPVCName)
	}
	if pvc.Labels[common.SubstrateVolumeLabel] != "true" {
		t.Errorf("supervisor PVC labels = %v, want %s=true", pvc.Labels, common.SubstrateVolumeLabel)
	}
	if got := *pvc.Spec.StorageClassName; got != testStorageClass {
		t.Errorf("supervisor PVC storage class = %q, want %q", got, testStorageClass)
	}
	if _, ok := pvc.Annotations[common.AnnKeyGuestClusterPvc]; ok {
		t.Errorf("supervisor PVC has guest PVC annotation %s, but there is no guest PVC",
			common.AnnKeyGuestClusterPvc)
	}
	if !c.isSubstrateVolume(context.Background(), resp.GetVolume().GetVolumeId()) {
		t.Errorf("isSubstrateVolume(%q) = false, want true", resp.GetVolume().GetVolumeId())
	}
}

// TestCreateVolume_Substrate_UnsupportedFsType verifies that a filesystem type that Agent
// Substrate cannot pass to the node is rejected instead of being silently ignored.
func TestCreateVolume_Substrate_UnsupportedFsType(t *testing.T) {
	c := newSubstrateController(t, true)
	req := substrateCreateRequest(map[string]string{
		common.AttributeSupervisorStorageClass: testStorageClass,
		"csi.storage.k8s.io/fstype":            "xfs",
	})
	_, err := c.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume error = %v, want code %s", err, codes.InvalidArgument)
	}
}

// TestCreateVolume_Substrate_FSSDisabled verifies that requests are handled as before when
// the agent-substrate-support FSS is disabled.
func TestCreateVolume_Substrate_FSSDisabled(t *testing.T) {
	c := newSubstrateController(t, false)
	req := substrateCreateRequest(map[string]string{
		common.AttributeSupervisorStorageClass: testStorageClass,
		"csi.storage.k8s.io/fstype":            "ext4",
	})
	_, err := c.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume error = %v, want code %s", err, codes.InvalidArgument)
	}
}

// TestIsAttachStillRequested_Substrate verifies that Agent Substrate volumes, which never
// have a VolumeAttachment, are attached, while other volumes still require one.
func TestIsAttachStillRequested_Substrate(t *testing.T) {
	ctx := context.Background()
	substrateHandle := substrateTestTKCUID + "-" + substrateTestVolumeName
	guestHandle := substrateTestTKCUID + "-0b6f1c7e-2c1d-4f7e-9a51-2f4f7f1d9c3a"
	otherClusterHandle := "4c3b2a19-0000-0000-0000-000000000000-" + substrateTestVolumeName

	tests := []struct {
		name       string
		fssEnabled bool
		handle     string
		seedVA     bool
		expect     bool
	}{
		{name: "substrate volume", fssEnabled: true, handle: substrateHandle, expect: true},
		{name: "substrate volume with FSS disabled", handle: substrateHandle, expect: false},
		{name: "substrate volume of another cluster", fssEnabled: true, handle: otherClusterHandle, expect: false},
		{name: "guest volume without VolumeAttachment", fssEnabled: true, handle: guestHandle, expect: false},
		{name: "guest volume with VolumeAttachment", fssEnabled: true, handle: guestHandle, seedVA: true,
			expect: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSubstrateController(t, tt.fssEnabled)
			if tt.seedVA {
				va := &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{
					Name: volumeAttachmentName(tt.handle, csitypes.Name, substrateTestNode),
				}}
				if _, err := c.guestClient.StorageV1().VolumeAttachments().Create(
					ctx, va, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to seed VolumeAttachment: %v", err)
				}
			}
			if got := c.isAttachStillRequested(ctx, tt.handle, substrateTestNode); got != tt.expect {
				t.Errorf("isAttachStillRequested(%q) = %v, want %v", tt.handle, got, tt.expect)
			}
		})
	}
}
