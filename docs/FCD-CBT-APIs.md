# Implementing GetMetadataAllocated and GetMetadataDelta with FCD APIs

## Overview

This document explains how to implement the Changed Block Tracking (CBT) APIs - `GetMetadataAllocated` and `GetMetadataDelta` - using **FCD (First Class Disk) APIs** directly through the vSphere VSLM (Virtual Storage Lifecycle Management) interface, as an alternative to using the VDDK library.

## Table of Contents

- [Background](#background)
- [Current Implementation (VDDK)](#current-implementation-vddk)
- [FCD API Implementation](#fcd-api-implementation)
- [API Comparison](#api-comparison)
- [Implementation Guide](#implementation-guide)
- [Code Examples](#code-examples)
- [Advantages and Disadvantages](#advantages-and-disadvantages)

## Background

### What is FCD?

First Class Disk (FCD), also called Improved Virtual Disk (IVD), is a vSphere feature that allows managing virtual disks independently of virtual machines. The Virtual Storage Lifecycle Management (VSLM) API provides full lifecycle management of these disks.

### CBT APIs We Need to Implement

1. **GetMetadataAllocated**: Returns the allocated blocks (non-zero blocks) for a snapshot
2. **GetMetadataDelta**: Returns the changed blocks between two snapshots

### Two Implementation Approaches

1. **VDDK (Current)**: Uses the Virtual Disk Development Kit library
   - Requires C library (CGO)
   - Linux-only
   - Direct access to VMDK files
   - Uses `QueryAllocatedBlocks` and `QueryChangedAreas`

2. **FCD APIs (Alternative)**: Uses vSphere VSLM web service APIs
   - Pure Go implementation
   - Cross-platform
   - Works through vCenter
   - Uses `VslmQueryChangedDiskAreas`

## Current Implementation (VDDK)

### Current Architecture

```
CSI Driver (controller_cbt_linux.go)
    ↓
virtual-disks library (disklib package)
    ↓
VDDK C Library (libvixDiskLib.so)
    ↓
VMware vSphere (ESXi host direct access)
```

### Current APIs Used

```go
// From virtual-disks library
disklib.Init(7, 0, "/usr/local/lib/vmware-vix-disklib-distrib")
connection, _ := disklib.Connect(connectParams)
diskHandle, _ := disklib.Open(connection, diskPath)

// Query allocated blocks
allocatedBlocks, _ := disklib.QueryAllocatedBlocks(
    diskHandle,
    startSector,
    numSectors,
    chunkSize,
)

// Query changed areas (hypothetical - not currently implemented in VDDK)
changedBlocks, _ := disklib.QueryChangedAreas(
    diskHandle,
    baseChangeId,
    startSector,
    numSectors,
)
```

### Limitations

- **Platform**: Linux-only (CGO + C libraries)
- **Deployment**: Requires VDDK installation in containers
- **Build**: Complex Docker-based build process
- **Development**: Cannot develop on macOS without Docker

## FCD API Implementation

### FCD Architecture

```
CSI Driver (controller_cbt_fcd.go)
    ↓
govmomi library (vslm package)
    ↓
vSphere VSLM SOAP API
    ↓
VMware vCenter (manages FCDs)
```

### Key FCD APIs

The VSLM API provides:

1. **VslmQueryChangedDiskAreas**: Get changed disk areas between snapshots
   - Equivalent to both GetMetadataAllocated and GetMetadataDelta
   - Works with FCD objects (volumes)
   - Returns list of changed disk areas

### govmomi Implementation

The `govmomi` library already includes VSLM support:

```go
// From govmomi/vslm/global_object_manager.go
func (this *GlobalObjectManager) QueryChangedDiskAreas(
    ctx context.Context,
    id vim.ID,              // Volume ID
    snapshotId vim.ID,      // Snapshot ID
    startOffset int64,      // Starting byte offset
    changeId string,        // Base change ID (* for all blocks)
) (*vim.DiskChangeInfo, error)
```

### Response Structure

```go
// vim25/types/types.go
type DiskChangeInfo struct {
    StartOffset int64           `xml:"startOffset" json:"startOffset"`
    Length      int64           `xml:"length" json:"length"`
    ChangedArea []DiskChangeInfo_ChangedArea `xml:"changedArea,omitempty" json:"changedArea,omitempty"`
}

type DiskChangeInfo_ChangedArea struct {
    Start  int64 `xml:"start" json:"start"`   // Byte offset
    Length int64 `xml:"length" json:"length"` // Length in bytes
}
```

## API Comparison

### GetMetadataAllocated

| Aspect | VDDK | FCD API |
|--------|------|---------|
| **API** | `QueryAllocatedBlocks` | `VslmQueryChangedDiskAreas` with `changeId="*"` |
| **Input** | Disk handle, sector range | Volume ID, Snapshot ID, byte range |
| **Output** | List of allocated sectors | List of allocated byte ranges |
| **Granularity** | 512-byte sectors | Arbitrary byte ranges (typically 4KB) |
| **Platform** | Linux only | Any platform |

### GetMetadataDelta

| Aspect | VDDK | FCD API |
|--------|------|---------|
| **API** | `QueryChangedAreas` (not standard VDDK) | `VslmQueryChangedDiskAreas` with base `changeId` |
| **Input** | Base/target handles, sector range | Volume ID, Snapshot ID, base changeId |
| **Output** | List of changed sectors | List of changed byte ranges |
| **Change Tracking** | Requires CBT enabled | Requires CBT enabled |
| **Platform** | Linux only | Any platform |

## Implementation Guide

### Prerequisites

1. **vSphere Configuration**:
   - vCenter 6.7 or later
   - Changed Block Tracking (CBT) enabled on volumes
   - Snapshots created with CBT enabled

2. **Go Dependencies**:
   ```go
   import (
       "github.com/vmware/govmomi"
       "github.com/vmware/govmomi/vslm"
       "github.com/vmware/govmomi/vim25/types"
   )
   ```

3. **Authentication**:
   - vCenter credentials
   - Authenticated vCenter session

### Step-by-Step Implementation

#### Step 1: Connect to vCenter VSLM Endpoint

```go
// Connect to vCenter
vimClient, err := govmomi.NewClient(ctx, vcURL, true)
if err != nil {
    return err
}

// Connect to VSLM endpoint
vslmClient, err := vslm.NewClient(ctx, vimClient.Client)
if err != nil {
    return err
}

// Get global object manager
globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)
```

#### Step 2: Implement GetMetadataAllocated

```go
func (c *controller) queryAllocatedBlocksFromFCD(
    ctx context.Context,
    volumeID string,      // FCD volume ID
    snapshotID string,    // Snapshot ID
    startingOffset uint64,
    maxResults uint32,
) (*AllocatedAreasResult, error) {
    
    log := logger.GetLogger(ctx)
    
    // Convert IDs to VSLM format
    vslmVolumeID := types.ID{Id: volumeID}
    vslmSnapshotID := types.ID{Id: snapshotID}
    
    // Use "*" as changeId to get all allocated blocks
    changeId := "*"
    
    // Query changed disk areas (all blocks when changeId="*")
    diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        vslmVolumeID,
        vslmSnapshotID,
        int64(startingOffset),
        changeId,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query allocated blocks: %v", err)
    }
    
    // Convert response to our format
    var allocatedAreas []AllocatedArea
    nextOffset := startingOffset
    
    for _, area := range diskChangeInfo.ChangedArea {
        if uint32(len(allocatedAreas)) >= maxResults {
            break
        }
        
        allocatedArea := AllocatedArea{
            Offset: uint64(area.Start),
            Length: uint64(area.Length),
        }
        allocatedAreas = append(allocatedAreas, allocatedArea)
        
        areaEnd := uint64(area.Start) + uint64(area.Length)
        if areaEnd > nextOffset {
            nextOffset = areaEnd
        }
    }
    
    // If we got fewer results than requested, we're done
    if uint32(len(allocatedAreas)) < maxResults {
        nextOffset = 0
    }
    
    return &AllocatedAreasResult{
        AllocatedAreas: allocatedAreas,
        NextOffset:     nextOffset,
    }, nil
}
```

#### Step 3: Implement GetMetadataDelta

```go
func (c *controller) queryChangedAreasFromFCD(
    ctx context.Context,
    volumeID string,
    baseSnapshotID string,
    targetSnapshotID string,
    startingOffset uint64,
    maxResults uint32,
) (*ChangedAreasResult, error) {
    
    log := logger.GetLogger(ctx)
    
    // Step 1: Get the changeId from the base snapshot
    baseChangeId, err := c.getSnapshotChangeId(ctx, volumeID, baseSnapshotID)
    if err != nil {
        return nil, fmt.Errorf("failed to get base snapshot changeId: %v", err)
    }
    
    // Step 2: Query changed areas between base and target
    vslmVolumeID := types.ID{Id: volumeID}
    vslmTargetSnapshotID := types.ID{Id: targetSnapshotID}
    
    diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        vslmVolumeID,
        vslmTargetSnapshotID,
        int64(startingOffset),
        baseChangeId,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query changed areas: %v", err)
    }
    
    // Step 3: Convert response to our format
    var changedAreas []ChangedArea
    nextOffset := startingOffset
    
    for _, area := range diskChangeInfo.ChangedArea {
        if uint32(len(changedAreas)) >= maxResults {
            break
        }
        
        changedArea := ChangedArea{
            Offset: uint64(area.Start),
            Length: uint64(area.Length),
        }
        changedAreas = append(changedAreas, changedArea)
        
        areaEnd := uint64(area.Start) + uint64(area.Length)
        if areaEnd > nextOffset {
            nextOffset = areaEnd
        }
    }
    
    if uint32(len(changedAreas)) < maxResults {
        nextOffset = 0
    }
    
    return &ChangedAreasResult{
        ChangedAreas: changedAreas,
        NextOffset:   nextOffset,
    }, nil
}
```

#### Step 4: Get Snapshot ChangeId

To query changed areas, you need the changeId from the base snapshot:

```go
func (c *controller) getSnapshotChangeId(
    ctx context.Context,
    volumeID string,
    snapshotID string,
) (string, error) {
    
    vslmVolumeID := types.ID{Id: volumeID}
    vslmSnapshotID := types.ID{Id: snapshotID}
    
    // Retrieve snapshot details
    snapshotDetails, err := globalObjectManager.RetrieveSnapshotDetails(
        ctx,
        vslmVolumeID,
        vslmSnapshotID,
    )
    if err != nil {
        return "", fmt.Errorf("failed to retrieve snapshot details: %v", err)
    }
    
    // Extract changeId from snapshot
    if snapshotDetails.DiskInfo != nil && len(snapshotDetails.DiskInfo) > 0 {
        return snapshotDetails.DiskInfo[0].ChangeId, nil
    }
    
    return "", fmt.Errorf("changeId not found in snapshot")
}
```

## Code Examples

### Complete Implementation File

Create a new file: `pkg/csi/service/wcp/controller_cbt_fcd.go`

```go
//go:build !vddk
// +build !vddk

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

    "github.com/vmware/govmomi"
    "github.com/vmware/govmomi/vslm"
    "github.com/vmware/govmomi/vim25/types"
    "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
    "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

// queryChangedAreasFromFCD uses FCD VSLM APIs to query changed disk areas
// This is a pure Go implementation that works on all platforms
func (c *controller) queryChangedAreasFromFCD(ctx context.Context, volumeID, baseSnapshotID,
    targetSnapshotID string, startingOffset uint64, maxResults uint32) (*ChangedAreasResult, error) {

    log := logger.GetLogger(ctx)
    log.Infof("Calling FCD QueryChangedDiskAreas for volume %s, base snapshot %s, target snapshot %s",
        volumeID, baseSnapshotID, targetSnapshotID)

    // Get vCenter connection
    vcenter, err := common.GetVCenter(ctx, c.manager)
    if err != nil {
        return nil, fmt.Errorf("failed to get vCenter instance: %v", err)
    }

    // Connect to VSLM endpoint
    vslmClient, err := vslm.NewClient(ctx, vcenter.Client.Client)
    if err != nil {
        return nil, fmt.Errorf("failed to create VSLM client: %v", err)
    }

    globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)

    // Get the changeId from the base snapshot
    baseChangeId, err := c.getSnapshotChangeIdFromFCD(ctx, globalObjectManager, volumeID, baseSnapshotID)
    if err != nil {
        return nil, fmt.Errorf("failed to get base snapshot changeId: %v", err)
    }

    // Convert IDs to VSLM format
    vslmVolumeID := types.ID{Id: volumeID}
    vslmTargetSnapshotID := types.ID{Id: targetSnapshotID}

    // Query changed disk areas
    diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        vslmVolumeID,
        vslmTargetSnapshotID,
        int64(startingOffset),
        baseChangeId,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query changed disk areas: %v", err)
    }

    // Convert to our result format
    var changedAreas []ChangedArea
    nextOffset := startingOffset

    for _, area := range diskChangeInfo.ChangedArea {
        if uint32(len(changedAreas)) >= maxResults {
            break
        }

        changedArea := ChangedArea{
            Offset: uint64(area.Start),
            Length: uint64(area.Length),
        }
        changedAreas = append(changedAreas, changedArea)

        areaEnd := uint64(area.Start) + uint64(area.Length)
        if areaEnd > nextOffset {
            nextOffset = areaEnd
        }
    }

    // If we got fewer results than requested, we're done
    if uint32(len(changedAreas)) < maxResults {
        nextOffset = 0
    }

    log.Infof("FCD QueryChangedDiskAreas returned %d changed areas for volume %s", len(changedAreas), volumeID)

    return &ChangedAreasResult{
        ChangedAreas: changedAreas,
        NextOffset:   nextOffset,
    }, nil
}

// queryAllocatedBlocksFromFCD uses FCD VSLM APIs to query allocated blocks
// This is a pure Go implementation that works on all platforms
func (c *controller) queryAllocatedBlocksFromFCD(ctx context.Context, volumeID, snapshotID string,
    startingOffset uint64, maxResults uint32) (*AllocatedAreasResult, error) {

    log := logger.GetLogger(ctx)
    log.Infof("Calling FCD QueryChangedDiskAreas (allocated blocks) for volume %s, snapshot %s",
        volumeID, snapshotID)

    // Get vCenter connection
    vcenter, err := common.GetVCenter(ctx, c.manager)
    if err != nil {
        return nil, fmt.Errorf("failed to get vCenter instance: %v", err)
    }

    // Connect to VSLM endpoint
    vslmClient, err := vslm.NewClient(ctx, vcenter.Client.Client)
    if err != nil {
        return nil, fmt.Errorf("failed to create VSLM client: %v", err)
    }

    globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)

    // Convert IDs to VSLM format
    vslmVolumeID := types.ID{Id: volumeID}
    vslmSnapshotID := types.ID{Id: snapshotID}

    // Use "*" as changeId to get all allocated blocks
    changeId := "*"

    // Query changed disk areas (all allocated blocks when changeId="*")
    diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        vslmVolumeID,
        vslmSnapshotID,
        int64(startingOffset),
        changeId,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query allocated blocks: %v", err)
    }

    // Convert to our result format
    var allocatedAreas []AllocatedArea
    nextOffset := startingOffset

    for _, area := range diskChangeInfo.ChangedArea {
        if uint32(len(allocatedAreas)) >= maxResults {
            break
        }

        allocatedArea := AllocatedArea{
            Offset: uint64(area.Start),
            Length: uint64(area.Length),
        }
        allocatedAreas = append(allocatedAreas, allocatedArea)

        areaEnd := uint64(area.Start) + uint64(area.Length)
        if areaEnd > nextOffset {
            nextOffset = areaEnd
        }
    }

    if uint32(len(allocatedAreas)) < maxResults {
        nextOffset = 0
    }

    log.Infof("FCD QueryChangedDiskAreas returned %d allocated areas for volume %s, snapshot %s",
        len(allocatedAreas), volumeID, snapshotID)

    return &AllocatedAreasResult{
        AllocatedAreas: allocatedAreas,
        NextOffset:     nextOffset,
    }, nil
}

// getSnapshotChangeIdFromFCD retrieves the changeId from a snapshot
func (c *controller) getSnapshotChangeIdFromFCD(ctx context.Context,
    globalObjectManager *vslm.GlobalObjectManager, volumeID, snapshotID string) (string, error) {

    log := logger.GetLogger(ctx)

    vslmVolumeID := types.ID{Id: volumeID}
    vslmSnapshotID := types.ID{Id: snapshotID}

    // Retrieve snapshot details
    snapshotDetails, err := globalObjectManager.RetrieveSnapshotDetails(
        ctx,
        vslmVolumeID,
        vslmSnapshotID,
    )
    if err != nil {
        return "", fmt.Errorf("failed to retrieve snapshot details: %v", err)
    }

    // Extract changeId from snapshot
    if snapshotDetails.DiskInfo != nil && len(snapshotDetails.DiskInfo) > 0 {
        changeId := snapshotDetails.DiskInfo[0].ChangeId
        log.Infof("Retrieved changeId %s for snapshot %s", changeId, snapshotID)
        return changeId, nil
    }

    return "", fmt.Errorf("changeId not found in snapshot %s", snapshotID)
}
```

### Integration with Main Controller

Modify `controller.go` to use the FCD implementation:

```go
func (c *controller) GetMetadataAllocated(ctx context.Context, req *csi.GetMetadataAllocatedRequest) (
    *csi.GetMetadataAllocatedResponse, error) {
    
    // ... validation code ...
    
    // Use FCD APIs instead of VDDK
    result, err := c.queryAllocatedBlocksFromFCD(ctx, volumeID, cnsSnapshotID, startingOffset, maxResults)
    if err != nil {
        return nil, logger.LogNewErrorCodef(log, codes.Internal,
            "failed to query allocated blocks: %v", err)
    }
    
    // ... convert result to response ...
}

func (c *controller) GetMetadataDelta(ctx context.Context, req *csi.GetMetadataDeltaRequest) (
    *csi.GetMetadataDeltaResponse, error) {
    
    // ... validation code ...
    
    // Use FCD APIs instead of VDDK
    result, err := c.queryChangedAreasFromFCD(ctx, volumeID, baseSnapshotID, targetSnapshotID,
        startingOffset, maxResults)
    if err != nil {
        return nil, logger.LogNewErrorCodef(log, codes.Internal,
            "failed to query changed areas: %v", err)
    }
    
    // ... convert result to response ...
}
```

## Advantages and Disadvantages

### FCD API Advantages

✅ **Cross-Platform**
- Pure Go implementation
- Works on macOS, Linux, Windows
- No CGO required

✅ **Simpler Build Process**
- No VDDK installation needed
- Standard Go cross-compilation works
- Smaller container images

✅ **Better Developer Experience**
- Develop and test on any platform
- Faster build times
- Easier CI/CD integration

✅ **Official vSphere APIs**
- Supported by VMware
- Well-documented
- Version compatibility guaranteed

### FCD API Disadvantages

❌ **Network Dependency**
- Requires vCenter connectivity
- Network latency affects performance
- Cannot work directly with ESXi hosts

❌ **vCenter Load**
- All queries go through vCenter
- May impact vCenter performance at scale
- Requires vCenter to be available

❌ **Limited Control**
- Cannot access VMDK files directly
- Depends on vCenter's CBT implementation
- Less flexibility than VDDK

❌ **Potential Performance**
- SOAP API overhead
- May be slower than direct VDDK access
- Network round-trips for each query

### VDDK Advantages

✅ **Performance**
- Direct access to VMDK files
- No network overhead after initial connection
- Lower latency

✅ **Independence**
- Can work directly with ESXi hosts
- Less dependency on vCenter
- More control over disk operations

### VDDK Disadvantages

❌ **Platform Limitations**
- Linux-only
- Requires CGO
- Complex build process

❌ **Deployment Complexity**
- Must bundle VDDK libraries
- Larger container images
- License considerations

❌ **Development Friction**
- Cannot develop on macOS natively
- Requires Docker for builds
- Slower iteration cycles

## Recommendations

### When to Use FCD APIs

Use FCD APIs when:
- Cross-platform support is important
- Simplicity and maintainability are priorities
- vCenter is always available
- Network latency is acceptable
- Development velocity matters

### When to Use VDDK

Use VDDK when:
- Maximum performance is critical
- Direct disk access is required
- vCenter availability is a concern
- You need features not available in FCD APIs
- Platform limitations are acceptable

### Hybrid Approach

Consider implementing both:

```go
// Build flag to choose implementation
//go:build vddk
// +build vddk

// Use VDDK implementation
func (c *controller) queryAllocatedBlocks(...) {
    return c.queryAllocatedBlocksFromVDDK(...)
}
```

```go
//go:build !vddk
// +build !vddk

// Use FCD implementation
func (c *controller) queryAllocatedBlocks(...) {
    return c.queryAllocatedBlocksFromFCD(...)
}
```

This allows:
- Development with FCD APIs (fast, cross-platform)
- Production with VDDK (performance-optimized)
- Choice based on deployment requirements

## Testing

### Unit Tests

```go
func TestQueryAllocatedBlocksFromFCD(t *testing.T) {
    // Create mock vCenter and VSLM client
    // Test FCD API calls
    // Verify response conversion
}

func TestQueryChangedAreasFromFCD(t *testing.T) {
    // Create mock vCenter and VSLM client
    // Test change tracking
    // Verify changeId handling
}
```

### Integration Tests

1. **Setup**:
   - Create FCD volume
   - Enable CBT
   - Create snapshots
   - Write test data

2. **Test GetMetadataAllocated**:
   - Call with various offsets
   - Verify allocated blocks
   - Test pagination

3. **Test GetMetadataDelta**:
   - Modify volume
   - Create new snapshot
   - Query changes
   - Verify change detection

## Conclusion

The FCD API implementation provides a **pure Go, cross-platform alternative** to VDDK for implementing Changed Block Tracking features. While VDDK may offer better performance for certain use cases, FCD APIs offer significant advantages in terms of simplicity, portability, and developer experience.

The choice between the two approaches should be based on your specific requirements, deployment environment, and priorities.

## References

- [vSphere VSLM API Documentation](https://developer.broadcom.com/xapis/virtual-infrastructure-json-api/latest/storage-lifecycle-management/)
- [govmomi vslm package](https://pkg.go.dev/github.com/vmware/govmomi/vslm)
- [govmomi object.VirtualMachine.QueryChangedDiskAreas](https://pkg.go.dev/github.com/vmware/govmomi/object#VirtualMachine.QueryChangedDiskAreas)
- [VSLM QueryChangedDiskAreas API](https://developer.broadcom.com/xapis/virtual-infrastructure-json-api/latest/storage-lifecycle-management/vslm-vstorage-object-manager/)
- [Current VDDK Implementation](./CBT-VDDK-IMPLEMENTATION.md)

---

**Document Version**: 1.0  
**Last Updated**: November 21, 2025  
**Status**: Reference Documentation

