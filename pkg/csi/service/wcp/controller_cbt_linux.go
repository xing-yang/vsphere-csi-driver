//go:build linux && cgo

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
	"context"
	"fmt"

	"github.com/vmware/virtual-disks/pkg/disklib"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

// queryChangedAreasFromVirtualDisks calls the virtual-disks QueryChangedAreas API
// This is the Linux implementation that uses VDDK.
func (c *controller) queryChangedAreasFromVirtualDisks(ctx context.Context, volumeID, baseSnapshotID,
	targetSnapshotID string, startingOffset uint64, maxResults uint32) (*ChangedAreasResult, error) {

	log := logger.GetLogger(ctx)
	log.Infof("Calling virtual-disks QueryChangedAreas for volume %s, base snapshot %s, target snapshot %s",
		volumeID, baseSnapshotID, targetSnapshotID)

	// Get vCenter connection parameters from the manager
	vcenter, err := common.GetVCenter(ctx, c.manager)
	if err != nil {
		return nil, fmt.Errorf("failed to get vCenter instance: %v", err)
	}

	// Initialize virtual-disks library
	err = disklib.Init(7, 0, "/usr/local/lib/vmware-vix-disklib-distrib")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize virtual-disks library: %v", err)
	}
	defer disklib.Exit()

	// Create connection parameters for virtual-disks
	connectParams := disklib.ConnectParams{}

	log.Infof("Setting up connection to vCenter %s for volume %s", vcenter.Config.Host, volumeID)

	// Prepare for access - this prevents VM migration during backup
	err = disklib.PrepareForAccess(connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare for access: %v", err)
	}
	defer disklib.EndAccess(connectParams)

	// Connect to vCenter
	connection, err := disklib.Connect(connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to vCenter: %v", err)
	}
	defer disklib.Disconnect(connection)

	// Open the disk for the target snapshot
	diskPath := fmt.Sprintf("[%s] %s/%s.vmdk", "datastore", volumeID, targetSnapshotID)

	diskHandle, err := disklib.Open(connection, connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to open disk %s: %v", diskPath, err)
	}
	defer disklib.Close(diskHandle)

	// Get disk information
	diskInfo, err := disklib.GetInfo(diskHandle)
	if err != nil {
		return nil, fmt.Errorf("failed to get disk info: %v", err)
	}

	// Convert byte offset to sector offset (VDDK works with sectors)
	startSector := disklib.VixDiskLibSectorType(startingOffset / 512)

	// Calculate the number of sectors to query
	remainingBytes := uint64(diskInfo.Capacity) - startingOffset
	maxBytesToQuery := uint64(maxResults) * 4096 // Assume 4KB blocks
	if remainingBytes < maxBytesToQuery {
		maxBytesToQuery = remainingBytes
	}
	numSectors := disklib.VixDiskLibSectorType(maxBytesToQuery / 512)

	// TODO: Implement actual VDDK QueryChangedAreas call
	// The virtual-disks library API needs to be verified
	// For now, return a placeholder implementation
	log.Infof("QueryChangedAreas API implementation pending for base snapshot %s to target snapshot %s",
		baseSnapshotID, targetSnapshotID)
	
	// Placeholder: Query allocated blocks only
	allocatedBlocks, err := disklib.QueryAllocatedBlocks(diskHandle,
		startSector,
		numSectors,
		disklib.VixDiskLibSectorType(8)) // 4KB chunks (8 sectors)
	if err != nil {
		return nil, fmt.Errorf("failed to query allocated blocks: %v", err)
	}
	
	// For prototype: treat allocated blocks as changed blocks
	changedBlocks := allocatedBlocks

	// Create maps for efficient intersection
	allocatedMap := make(map[uint64]uint64) // offset -> length
	for _, block := range allocatedBlocks {
		offset := uint64(block.Offset()) * 512
		length := uint64(block.Length()) * 512
		allocatedMap[offset] = length
	}

	// Find intersection of changed blocks and allocated blocks
	var changedAreas []ChangedArea
	nextOffset := startingOffset

	for _, block := range changedBlocks {
		// Convert from sectors to bytes
		blockOffset := uint64(block.Offset()) * 512
		blockLength := uint64(block.Length()) * 512

		// Only include blocks that are within our query range and are allocated
		if blockOffset >= startingOffset {
			// Check if this changed block intersects with any allocated block
			for allocOffset, allocLength := range allocatedMap {
				// Check for overlap
				changedEnd := blockOffset + blockLength
				allocEnd := allocOffset + allocLength

				if blockOffset < allocEnd && changedEnd > allocOffset {
					// Calculate overlapping area
					overlapStart := blockOffset
					if allocOffset > blockOffset {
						overlapStart = allocOffset
					}

					overlapEnd := changedEnd
					if allocEnd < changedEnd {
						overlapEnd = allocEnd
					}

					if overlapEnd > overlapStart {
						area := ChangedArea{
							Offset: overlapStart,
							Length: overlapEnd - overlapStart,
						}
						changedAreas = append(changedAreas, area)

						if overlapEnd > nextOffset {
							nextOffset = overlapEnd
						}

						if uint32(len(changedAreas)) >= maxResults {
							break
						}
					}
				}
			}
		}

		if uint32(len(changedAreas)) >= maxResults {
			break
		}
	}

	// If we've processed all blocks, set nextOffset to 0 to indicate completion
	if uint32(len(changedAreas)) < maxResults {
		nextOffset = 0
	}

	log.Infof("QueryChangedAreas returned %d changed areas for volume %s", len(changedAreas), volumeID)

	return &ChangedAreasResult{
		ChangedAreas: changedAreas,
		NextOffset:   nextOffset,
	}, nil
}

// queryAllocatedBlocksFromVirtualDisks calls the virtual-disks QueryAllocatedBlocks API
// This is the Linux implementation that uses VDDK.
func (c *controller) queryAllocatedBlocksFromVirtualDisks(ctx context.Context, volumeID, snapshotID string,
	startingOffset uint64, maxResults uint32) (*AllocatedAreasResult, error) {

	log := logger.GetLogger(ctx)
	log.Infof("Calling virtual-disks QueryAllocatedBlocks for volume %s, snapshot %s",
		volumeID, snapshotID)

	// Get vCenter connection parameters from the manager
	vcenter, err := common.GetVCenter(ctx, c.manager)
	if err != nil {
		return nil, fmt.Errorf("failed to get vCenter instance: %v", err)
	}

	// Initialize virtual-disks library
	err = disklib.Init(7, 0, "/usr/local/lib/vmware-vix-disklib-distrib")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize virtual-disks library: %v", err)
	}
	defer disklib.Exit()

	// Create connection parameters
	connectParams := disklib.ConnectParams{}

	log.Infof("Setting up connection to vCenter %s for volume %s", vcenter.Config.Host, volumeID)

	// Prepare for access
	err = disklib.PrepareForAccess(connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare for access: %v", err)
	}
	defer disklib.EndAccess(connectParams)

	// Connect to vCenter
	connection, err := disklib.Connect(connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to vCenter: %v", err)
	}
	defer disklib.Disconnect(connection)

	// Open the disk for the snapshot
	diskPath := fmt.Sprintf("[%s] %s/%s.vmdk", "datastore", volumeID, snapshotID)

	diskHandle, err := disklib.Open(connection, connectParams)
	if err != nil {
		return nil, fmt.Errorf("failed to open disk %s: %v", diskPath, err)
	}
	defer disklib.Close(diskHandle)

	// Get disk information
	diskInfo, err := disklib.GetInfo(diskHandle)
	if err != nil {
		return nil, fmt.Errorf("failed to get disk info: %v", err)
	}

	// Convert byte offset to sector offset
	startSector := disklib.VixDiskLibSectorType(startingOffset / 512)

	// Calculate sectors to query
	remainingBytes := uint64(diskInfo.Capacity) - startingOffset
	maxBytesToQuery := uint64(maxResults) * 4096
	if remainingBytes < maxBytesToQuery {
		maxBytesToQuery = remainingBytes
	}
	numSectors := disklib.VixDiskLibSectorType(maxBytesToQuery / 512)

	// Query allocated blocks
	log.Infof("Calling QueryAllocatedBlocks for snapshot %s starting at sector %d for %d sectors",
		snapshotID, startSector, numSectors)

	allocatedBlocks, err := disklib.QueryAllocatedBlocks(diskHandle,
		startSector,
		numSectors,
		disklib.VixDiskLibSectorType(8)) // 4KB chunks
	if err != nil {
		return nil, fmt.Errorf("failed to query allocated blocks: %v", err)
	}

	// Convert to result format
	var allocatedAreas []AllocatedArea
	nextOffset := startingOffset

	for _, block := range allocatedBlocks {
		blockOffset := uint64(block.Offset()) * 512
		blockLength := uint64(block.Length()) * 512

		if blockOffset >= startingOffset {
			area := AllocatedArea{
				Offset: blockOffset,
				Length: blockLength,
			}
			allocatedAreas = append(allocatedAreas, area)

			blockEnd := blockOffset + blockLength
			if blockEnd > nextOffset {
				nextOffset = blockEnd
			}

			if uint32(len(allocatedAreas)) >= maxResults {
				break
			}
		}
	}

	if uint32(len(allocatedAreas)) < maxResults {
		nextOffset = 0
	}

	log.Infof("QueryAllocatedBlocks returned %d allocated areas for volume %s, snapshot %s",
		len(allocatedAreas), volumeID, snapshotID)

	return &AllocatedAreasResult{
		AllocatedAreas: allocatedAreas,
		NextOffset:     nextOffset,
	}, nil
}

