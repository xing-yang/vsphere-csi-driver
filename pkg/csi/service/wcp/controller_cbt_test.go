/*
Copyright 2025 The Kubernetes Authors.

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

package wcp

import (
	"os"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"github.com/vmware/govmomi/pbm"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common/commonco"
)

// TestWCPGetMetadataAllocated tests GetMetadataAllocated functionality.
func TestWCPGetMetadataAllocated(t *testing.T) {
	ct := getControllerTest(t)

	// Enable Changed Block Tracking FSS
	err := commonco.ContainerOrchestratorUtility.EnableFSS(ctx, common.ChangedBlockTracking)
	if err != nil {
		t.Fatal("failed to enable ChangedBlockTracking FSS")
	}
	defer func() {
		err := commonco.ContainerOrchestratorUtility.DisableFSS(ctx, common.ChangedBlockTracking)
		if err != nil {
			t.Fatal("failed to disable ChangedBlockTracking FSS")
		}
	}()

	// Create volume
	params := make(map[string]string)
	profileID := os.Getenv("VSPHERE_STORAGE_POLICY_ID")
	if profileID == "" {
		storagePolicyName := os.Getenv("VSPHERE_STORAGE_POLICY_NAME")
		if storagePolicyName == "" {
			storagePolicyName = "vSAN Default Storage Policy"
		}

		pc, err := pbm.NewClient(ctx, ct.vcenter.Client.Client)
		if err != nil {
			t.Fatal(err)
		}

		profileID, err = pc.ProfileIDByName(ctx, storagePolicyName)
		if err != nil {
			t.Fatal(err)
		}
	}
	params[common.AttributeStoragePolicyID] = profileID

	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{},
			Preferred: []*csi.Topology{},
		},
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify volume was created
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		t.Fatal(err)
	}
	if len(queryResult.Volumes) != 1 || queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify volume was deleted
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, &queryFilter)
		if err != nil {
			t.Fatal(err)
		}
		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	// Create snapshot
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId
	t.Logf("Created snapshot with ID: %s", snapID)

	defer func() {
		// Delete snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}
		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Test GetMetadataAllocated
	t.Run("ValidRequest", func(t *testing.T) {
		reqGetMetadata := &csi.GetMetadataAllocatedRequest{
			SnapshotId:     snapID,
			StartingOffset: 0,
			MaxResults:     100,
		}

		respGetMetadata, err := ct.controller.GetMetadataAllocated(ctx, reqGetMetadata)
		if err != nil {
			t.Fatalf("GetMetadataAllocated failed: %v", err)
		}

		t.Logf("GetMetadataAllocated returned %d allocated blocks", len(respGetMetadata.BlockMetadata))
		t.Logf("Volume capacity: %d bytes", respGetMetadata.VolumeCapacityBytes)

		// Verify response structure
		if respGetMetadata.VolumeCapacityBytes != 1*common.GbInBytes {
			t.Fatalf("expected volume capacity %d, got %d",
				1*common.GbInBytes, respGetMetadata.VolumeCapacityBytes)
		}

		// Log allocated blocks details
		for i, block := range respGetMetadata.BlockMetadata {
			t.Logf("Block %d: Offset=%d, Size=%d", i, block.ByteOffset, block.SizeBytes)
		}

		if len(respGetMetadata.BlockMetadata) > 0 {
			if respGetMetadata.BlockMetadataType != csi.BlockMetadataType_FIXED_LENGTH {
				t.Fatalf("expected BlockMetadataType FIXED_LENGTH, got %v",
					respGetMetadata.BlockMetadataType)
			}
		}
	})

	// Test with invalid snapshot ID
	t.Run("InvalidSnapshotID", func(t *testing.T) {
		reqGetMetadata := &csi.GetMetadataAllocatedRequest{
			SnapshotId:     "invalid-snapshot-id",
			StartingOffset: 0,
			MaxResults:     100,
		}

		_, err := ct.controller.GetMetadataAllocated(ctx, reqGetMetadata)
		if err == nil {
			t.Fatal("expected error for invalid snapshot ID, got nil")
		}

		statusErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert error to grpc status: %v", err)
		}

		if statusErr.Code() != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument error code, got %s", statusErr.Code())
		}
		t.Logf("Got expected error for invalid snapshot ID: %v", err)
	})

	// Test with empty snapshot ID
	t.Run("EmptySnapshotID", func(t *testing.T) {
		reqGetMetadata := &csi.GetMetadataAllocatedRequest{
			SnapshotId:     "",
			StartingOffset: 0,
			MaxResults:     100,
		}

		_, err := ct.controller.GetMetadataAllocated(ctx, reqGetMetadata)
		if err == nil {
			t.Fatal("expected error for empty snapshot ID, got nil")
		}

		statusErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert error to grpc status: %v", err)
		}

		if statusErr.Code() != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument error code, got %s", statusErr.Code())
		}
		t.Logf("Got expected error for empty snapshot ID: %v", err)
	})

	// Test with nil request
	t.Run("NilRequest", func(t *testing.T) {
		_, err := ct.controller.GetMetadataAllocated(ctx, nil)
		if err == nil {
			t.Fatal("expected error for nil request, got nil")
		}

		statusErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert error to grpc status: %v", err)
		}

		if statusErr.Code() != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument error code, got %s", statusErr.Code())
		}
		t.Logf("Got expected error for nil request: %v", err)
	})

	// Test with pagination
	t.Run("WithPagination", func(t *testing.T) {
		var allBlocks []*csi.BlockMetadata
		startingOffset := int64(0)

		// Fetch allocated blocks in multiple requests
		for i := 0; i < 3; i++ {
			reqGetMetadata := &csi.GetMetadataAllocatedRequest{
				SnapshotId:     snapID,
				StartingOffset: startingOffset,
				MaxResults:     10,
			}

			respGetMetadata, err := ct.controller.GetMetadataAllocated(ctx, reqGetMetadata)
			if err != nil {
				t.Fatalf("GetMetadataAllocated failed on iteration %d: %v", i, err)
			}

			t.Logf("Iteration %d: returned %d blocks", i, len(respGetMetadata.BlockMetadata))

			if len(respGetMetadata.BlockMetadata) == 0 {
				t.Logf("No more allocated blocks to fetch")
				break
			}

			allBlocks = append(allBlocks, respGetMetadata.BlockMetadata...)

			// Calculate next offset
			lastBlock := respGetMetadata.BlockMetadata[len(respGetMetadata.BlockMetadata)-1]
			startingOffset = lastBlock.ByteOffset + lastBlock.SizeBytes
		}

		t.Logf("Total allocated blocks fetched: %d", len(allBlocks))
	})
}

// TestWCPGetMetadataAllocatedWithoutFSS tests GetMetadataAllocated when FSS is disabled.
func TestWCPGetMetadataAllocatedWithoutFSS(t *testing.T) {
	ct := getControllerTest(t)

	// Ensure Changed Block Tracking FSS is disabled
	err := commonco.ContainerOrchestratorUtility.DisableFSS(ctx, common.ChangedBlockTracking)
	if err != nil {
		t.Logf("Warning: failed to disable ChangedBlockTracking FSS: %v", err)
	}

	// Create volume
	params := make(map[string]string)
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	defer func() {
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		ct.controller.DeleteVolume(ctx, reqDelete)
	}()

	// Create snapshot
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}
		ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
	}()

	// Try GetMetadataAllocated - should fail with Unimplemented
	reqGetMetadata := &csi.GetMetadataAllocatedRequest{
		SnapshotId:     snapID,
		StartingOffset: 0,
		MaxResults:     100,
	}

	_, err = ct.controller.GetMetadataAllocated(ctx, reqGetMetadata)
	if err == nil {
		t.Fatal("expected error when FSS is disabled, got nil")
	}

	statusErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("unable to convert error to grpc status: %v", err)
	}

	if statusErr.Code() != codes.Unimplemented {
		t.Fatalf("expected Unimplemented error code, got %s", statusErr.Code())
	}

	t.Logf("Got expected Unimplemented error: %v", err)
}

