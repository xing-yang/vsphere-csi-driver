# FCD API Quick Reference for CBT Implementation

## Summary

This document provides a quick reference for implementing Changed Block Tracking (CBT) using FCD (First Class Disk) APIs through vSphere's VSLM (Virtual Storage Lifecycle Management) interface.

## Key API: VslmQueryChangedDiskAreas

The single VSLM API that powers both GetMetadataAllocated and GetMetadataDelta:

```go
func QueryChangedDiskAreas(
    ctx context.Context,
    id vim.ID,              // Volume ID (FCD UUID)
    snapshotId vim.ID,      // Snapshot ID
    startOffset int64,      // Starting byte offset
    changeId string,        // Base change ID
) (*vim.DiskChangeInfo, error)
```

### Key Insight

The `changeId` parameter determines the operation:
- **`changeId = "*"`**: Returns ALL allocated blocks (GetMetadataAllocated)
- **`changeId = "<specific-id>"`**: Returns changed blocks since that ID (GetMetadataDelta)

## GetMetadataAllocated Implementation

### API Call

```go
// Use "*" as changeId to get all allocated blocks
diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    types.ID{Id: volumeID},
    types.ID{Id: snapshotID},
    int64(startingOffset),
    "*",  // <-- Key: "*" means "all allocated blocks"
)
```

### Flow

```
1. Connect to vCenter VSLM endpoint
2. Call QueryChangedDiskAreas with changeId="*"
3. Parse DiskChangeInfo.ChangedArea array
4. Convert to CSI response format
```

### Example

```go
// Get allocated blocks from offset 0, max 1000 results
result, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    types.ID{Id: "volume-uuid"},
    types.ID{Id: "snapshot-uuid"},
    0,     // Start at beginning
    "*",   // All allocated blocks
)

// Result contains:
// result.ChangedArea[0] = {Start: 0, Length: 4096}        // Block 0
// result.ChangedArea[1] = {Start: 8192, Length: 4096}     // Block 2
// etc.
```

## GetMetadataDelta Implementation

### API Call

```go
// Step 1: Get changeId from base snapshot
baseChangeId := getSnapshotChangeId(ctx, volumeID, baseSnapshotID)

// Step 2: Query changes since baseChangeId
diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    types.ID{Id: volumeID},
    types.ID{Id: targetSnapshotID},
    int64(startingOffset),
    baseChangeId,  // <-- Base changeId for delta
)
```

### Flow

```
1. Connect to vCenter VSLM endpoint
2. Retrieve base snapshot details to get its changeId
3. Call QueryChangedDiskAreas with base changeId
4. Parse DiskChangeInfo.ChangedArea array
5. Convert to CSI response format
```

### Getting ChangeId

```go
func getSnapshotChangeId(ctx context.Context, volumeID, snapshotID string) (string, error) {
    // Retrieve snapshot details
    snapshotDetails, err := globalObjectManager.RetrieveSnapshotDetails(
        ctx,
        types.ID{Id: volumeID},
        types.ID{Id: snapshotID},
    )
    if err != nil {
        return "", err
    }
    
    // Extract changeId
    return snapshotDetails.DiskInfo[0].ChangeId, nil
}
```

### Example

```go
// Get changeId from base snapshot
baseChangeId := "snapshot-changeId-123"

// Get changed blocks between base and target
result, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    types.ID{Id: "volume-uuid"},
    types.ID{Id: "target-snapshot-uuid"},
    0,
    baseChangeId,
)

// Result contains only changed blocks:
// result.ChangedArea[0] = {Start: 4096, Length: 4096}     // Block 1 changed
// result.ChangedArea[1] = {Start: 12288, Length: 8192}    // Blocks 3-4 changed
```

## Setup and Connection

### Import Packages

```go
import (
    "github.com/vmware/govmomi"
    "github.com/vmware/govmomi/vslm"
    "github.com/vmware/govmomi/vim25/types"
)
```

### Connect to VSLM

```go
// 1. Connect to vCenter
vimClient, err := govmomi.NewClient(ctx, vcURL, insecure)
if err != nil {
    return err
}

// 2. Create VSLM client
vslmClient, err := vslm.NewClient(ctx, vimClient.Client)
if err != nil {
    return err
}

// 3. Get global object manager
globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)

// 4. Now you can call QueryChangedDiskAreas
```

## Data Structure Conversion

### Input: CSI Request

```go
// GetMetadataAllocated
req := &csi.GetMetadataAllocatedRequest{
    SnapshotId:     "volume-id+snapshot-id",  // Combined ID
    StartingOffset: 0,
    MaxResults:     1000,
}

// GetMetadataDelta
req := &csi.GetMetadataDeltaRequest{
    BaseSnapshotId:   "volume-id+base-snapshot-id",
    TargetSnapshotId: "volume-id+target-snapshot-id",
    StartingOffset:   0,
    MaxResults:       1000,
}
```

### Output: VSLM Response

```go
// DiskChangeInfo structure
type DiskChangeInfo struct {
    StartOffset int64                        // Query starting point
    Length      int64                        // Total length queried
    ChangedArea []DiskChangeInfo_ChangedArea // Array of changed/allocated areas
}

type DiskChangeInfo_ChangedArea struct {
    Start  int64  // Byte offset where area starts
    Length int64  // Length of area in bytes
}
```

### Conversion Logic

```go
// Convert VSLM response to CSI format
var areas []AllocatedArea  // or ChangedArea
nextOffset := startingOffset

for _, area := range diskChangeInfo.ChangedArea {
    if len(areas) >= maxResults {
        break
    }
    
    areas = append(areas, AllocatedArea{
        Offset: uint64(area.Start),
        Length: uint64(area.Length),
    })
    
    // Track the end of this area
    areaEnd := uint64(area.Start) + uint64(area.Length)
    if areaEnd > nextOffset {
        nextOffset = areaEnd
    }
}

// If we got fewer results than requested, we're done
if len(areas) < maxResults {
    nextOffset = 0  // Signals completion
}
```

## Complete Minimal Example

```go
package main

import (
    "context"
    "fmt"
    "net/url"

    "github.com/vmware/govmomi"
    "github.com/vmware/govmomi/vslm"
    "github.com/vmware/govmomi/vim25/types"
)

func main() {
    ctx := context.Background()
    
    // Connect to vCenter
    vcURL, _ := url.Parse("https://user:pass@vcenter.example.com/sdk")
    vimClient, _ := govmomi.NewClient(ctx, vcURL, true)
    
    // Create VSLM client
    vslmClient, _ := vslm.NewClient(ctx, vimClient.Client)
    globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)
    
    // Example 1: Get all allocated blocks (GetMetadataAllocated)
    fmt.Println("Getting allocated blocks...")
    allocated, _ := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        types.ID{Id: "volume-uuid"},
        types.ID{Id: "snapshot-uuid"},
        0,    // Start at beginning
        "*",  // All allocated blocks
    )
    fmt.Printf("Found %d allocated areas\n", len(allocated.ChangedArea))
    
    // Example 2: Get changed blocks (GetMetadataDelta)
    fmt.Println("\nGetting changed blocks...")
    
    // Get base snapshot changeId
    baseSnapshot, _ := globalObjectManager.RetrieveSnapshotDetails(
        ctx,
        types.ID{Id: "volume-uuid"},
        types.ID{Id: "base-snapshot-uuid"},
    )
    baseChangeId := baseSnapshot.DiskInfo[0].ChangeId
    
    // Query changes
    changed, _ := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        types.ID{Id: "volume-uuid"},
        types.ID{Id: "target-snapshot-uuid"},
        0,
        baseChangeId,
    )
    fmt.Printf("Found %d changed areas\n", len(changed.ChangedArea))
}
```

## Comparison Table

| Feature | VDDK | FCD API |
|---------|------|---------|
| **GetMetadataAllocated** | `disklib.QueryAllocatedBlocks()` | `QueryChangedDiskAreas(changeId="*")` |
| **GetMetadataDelta** | `disklib.QueryChangedAreas()` | `QueryChangedDiskAreas(changeId=base)` |
| **Platform** | Linux only | Cross-platform |
| **Language** | C + CGO | Pure Go |
| **Dependency** | VDDK library | govmomi library |
| **Connection** | ESXi host direct | vCenter VSLM endpoint |
| **Granularity** | 512-byte sectors | Byte ranges (typically 4KB) |
| **Performance** | Direct disk access | Network API calls |
| **Build** | Docker + CGO | Standard Go build |

## Key Differences from VM APIs

### For VMs (with govmomi/object)

```go
vm := object.NewVirtualMachine(client, vmRef)

// VM method requires disk device key
result, err := vm.QueryChangedDiskAreas(
    ctx,
    baseSnapshot,
    curSnapshot,
    disk,         // *types.VirtualDisk with Key
    offset,
)
```

### For FCDs (with govmomi/vslm)

```go
globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)

// FCD method uses volume and snapshot IDs directly
result, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,      // types.ID
    snapshotID,    // types.ID
    offset,
    changeId,      // "*" or specific changeId
)
```

## Prerequisites

### vSphere Configuration

1. **vCenter 6.7+**: VSLM APIs available
2. **CBT Enabled**: On the volume/FCD
3. **Snapshots**: Created with CBT enabled

### Go Module Setup

```go
// go.mod
require (
    github.com/vmware/govmomi v0.53.0-alpha.0  // Or later
)
```

### Authentication

```go
// vCenter credentials required
vcURL := url.URL{
    Scheme: "https",
    User:   url.UserPassword("username", "password"),
    Host:   "vcenter.example.com",
    Path:   "/sdk",
}
```

## Error Handling

### Common Errors

```go
// 1. CBT not enabled
// Error: "Change tracking not supported"
// Solution: Enable CBT on the volume

// 2. Invalid changeId
// Error: "Invalid change ID"
// Solution: Verify changeId from base snapshot

// 3. Snapshot not found
// Error: "Snapshot does not exist"
// Solution: Verify snapshot ID is correct

// 4. vCenter not reachable
// Error: "Connection refused"
// Solution: Check vCenter connectivity
```

### Error Handling Pattern

```go
result, err := globalObjectManager.QueryChangedDiskAreas(...)
if err != nil {
    // Check for specific errors
    if soap.IsSoapFault(err) {
        fault := soap.ToSoapFault(err)
        switch fault.VimFault().(type) {
        case *types.NotFound:
            return fmt.Errorf("snapshot not found: %v", err)
        case *types.InvalidArgument:
            return fmt.Errorf("invalid changeId: %v", err)
        default:
            return fmt.Errorf("VSLM API error: %v", err)
        }
    }
    return fmt.Errorf("query failed: %v", err)
}
```

## Performance Considerations

### Pagination

```go
// Query in chunks to avoid large responses
const chunkSize = 1000  // blocks per query

offset := uint64(0)
for offset < diskSize {
    result, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        volumeID,
        snapshotID,
        int64(offset),
        changeId,
    )
    
    // Process result.ChangedArea
    
    // Get next offset
    if len(result.ChangedArea) < chunkSize {
        break  // Done
    }
    
    lastArea := result.ChangedArea[len(result.ChangedArea)-1]
    offset = uint64(lastArea.Start + lastArea.Length)
}
```

### Caching

```go
// Cache changeIds to avoid repeated lookups
type changeIdCache struct {
    sync.RWMutex
    cache map[string]string  // snapshotID -> changeId
}

func (c *changeIdCache) Get(snapshotID string) (string, bool) {
    c.RLock()
    defer c.RUnlock()
    changeId, ok := c.cache[snapshotID]
    return changeId, ok
}
```

## Testing Tips

### Mock VSLM Client

```go
type mockGlobalObjectManager struct {
    queryFunc func(ctx context.Context, id, snapshotId types.ID, 
                   offset int64, changeId string) (*types.DiskChangeInfo, error)
}

func (m *mockGlobalObjectManager) QueryChangedDiskAreas(...) (*types.DiskChangeInfo, error) {
    return m.queryFunc(...)
}
```

### Test Data

```go
// Sample response for testing
testResponse := &types.DiskChangeInfo{
    StartOffset: 0,
    Length:      1048576,  // 1MB
    ChangedArea: []types.DiskChangeInfo_ChangedArea{
        {Start: 0, Length: 4096},      // Block 0
        {Start: 8192, Length: 4096},   // Block 2
        {Start: 16384, Length: 8192},  // Blocks 4-5
    },
}
```

## Resources

- **govmomi vslm package**: https://pkg.go.dev/github.com/vmware/govmomi/vslm
- **VSLM API Reference**: https://developer.broadcom.com/xapis/virtual-infrastructure-json-api/latest/storage-lifecycle-management/
- **Complete Implementation Guide**: [FCD-CBT-APIs.md](./FCD-CBT-APIs.md)

---

**Quick Reference Version**: 1.0  
**Last Updated**: November 21, 2025

