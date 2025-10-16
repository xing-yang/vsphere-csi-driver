# CBT Implementation Comparison: VDDK vs FCD APIs

## Executive Summary

This document compares two approaches for implementing Changed Block Tracking (CBT) in vsphere-csi-driver:

1. **VDDK** (Current): Using VMware Virtual Disk Development Kit
2. **FCD APIs** (Alternative): Using vSphere VSLM (Virtual Storage Lifecycle Management) APIs

## Quick Comparison

| Aspect | VDDK | FCD APIs |
|--------|------|----------|
| **Language** | C library + CGO | Pure Go |
| **Platform** | Linux only | Cross-platform |
| **Build** | Docker required | Standard Go build |
| **Dependencies** | VDDK library (80MB+) | govmomi only |
| **Connection** | Direct ESXi access | Through vCenter |
| **Performance** | Direct disk access | Network API calls |
| **Dev Experience** | Complex setup | Simple setup |
| **API Maturity** | Mature, stable | Official, supported |

## Detailed Comparison

### 1. API Structure

#### VDDK Approach

```go
// virtual-disks library wrapping VDDK
import "github.com/vmware/virtual-disks/pkg/disklib"

// Initialize VDDK
disklib.Init(7, 0, "/usr/local/lib/vmware-vix-disklib-distrib")

// Connect and open disk
connection, _ := disklib.Connect(connectParams)
diskHandle, _ := disklib.Open(connection, diskPath)

// Query allocated blocks
blocks, _ := disklib.QueryAllocatedBlocks(
    diskHandle,
    startSector,
    numSectors,
    chunkSize,
)
```

#### FCD API Approach

```go
// govmomi VSLM client
import "github.com/vmware/govmomi/vslm"

// Connect to VSLM endpoint
vslmClient, _ := vslm.NewClient(ctx, vimClient.Client)
globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)

// Query changed disk areas
diskChangeInfo, _ := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,
    snapshotID,
    startOffset,
    changeId,  // "*" for all allocated, or specific ID for delta
)
```

### 2. GetMetadataAllocated Implementation

#### VDDK

```go
// Query allocated blocks (sectors)
allocatedBlocks, err := disklib.QueryAllocatedBlocks(
    diskHandle,
    startSector,        // VixDiskLibSectorType (uint64)
    numSectors,         // VixDiskLibSectorType
    8,                  // Chunk size in sectors (8 = 4KB)
)

// Result: Array of blocks with sector offsets
for _, block := range allocatedBlocks {
    offset := uint64(block.Offset()) * 512  // Convert sectors to bytes
    length := uint64(block.Length()) * 512
}
```

**Characteristics:**
- Works with 512-byte sectors
- Requires sector/byte conversion
- Direct disk access
- Must maintain disk handle

#### FCD APIs

```go
// Query all allocated blocks (bytes)
diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,
    snapshotID,
    startOffset,        // int64 (bytes)
    "*",                // Special changeId for "all allocated"
)

// Result: Array of byte ranges
for _, area := range diskChangeInfo.ChangedArea {
    offset := uint64(area.Start)   // Already in bytes
    length := uint64(area.Length)
}
```

**Characteristics:**
- Works with byte offsets directly
- No conversion needed
- Network API call
- Stateless (no handles to manage)

### 3. GetMetadataDelta Implementation

#### VDDK

```go
// Note: QueryChangedAreas is NOT standard in VDDK
// Most implementations use QueryAllocatedBlocks on both snapshots
// and compute intersection

// Pseudo-code (not actual VDDK API):
baseBlocks, _ := disklib.QueryAllocatedBlocks(baseHandle, ...)
targetBlocks, _ := disklib.QueryAllocatedBlocks(targetHandle, ...)

// Manually compute delta
changedBlocks := computeDelta(baseBlocks, targetBlocks)
```

**Characteristics:**
- No native delta query (in standard VDDK)
- Requires querying both snapshots
- Manual delta computation needed
- More complex logic

#### FCD APIs

```go
// Step 1: Get changeId from base snapshot
baseSnapshot, _ := globalObjectManager.RetrieveSnapshotDetails(
    ctx, volumeID, baseSnapshotID,
)
baseChangeId := baseSnapshot.DiskInfo[0].ChangeId

// Step 2: Query changes since base
diskChangeInfo, _ := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,
    targetSnapshotID,
    startOffset,
    baseChangeId,  // Specific changeId for delta
)

// Result: Only changed blocks
for _, area := range diskChangeInfo.ChangedArea {
    offset := uint64(area.Start)
    length := uint64(area.Length)
}
```

**Characteristics:**
- Native delta query support
- Single API call (after getting changeId)
- vCenter computes delta
- Simpler, cleaner code

### 4. Build and Deployment

#### VDDK

**Build Process:**

```bash
# On macOS - must use Docker
make docker-build-csi

# Docker command with VDDK
docker run \
    --platform linux/amd64 \
    -e CGO_ENABLED=1 \
    -e CGO_CFLAGS="-I/usr/local/vmware-vix-disklib-distrib/include" \
    -e CGO_LDFLAGS="-L.../lib64 -lvixDiskLib -ldl" \
    -v $(VDDK_LIBS):/usr/local/vmware-vix-disklib-distrib:ro \
    golang:1.24.2 \
    go build ...
```

**Container Image:**

```dockerfile
# Must include VDDK libraries
FROM photon:5.0

# Copy VDDK (~80MB)
COPY vmware-vix-disklib-distrib/lib64 /usr/local/vmware-vix-disklib-distrib/lib64

# Set library path
ENV LD_LIBRARY_PATH=/usr/local/vmware-vix-disklib-distrib/lib64

# Binary is dynamically linked
COPY vsphere-csi /bin/vsphere-csi
```

**Image Size:** ~405MB (with VDDK)

**Characteristics:**
- ❌ Complex build process
- ❌ Requires VDDK installation
- ❌ Docker mandatory for macOS
- ❌ Large container images
- ❌ CGO compilation overhead
- ❌ Platform-specific builds

#### FCD APIs

**Build Process:**

```bash
# On any platform - standard Go build
make build-csi

# Or cross-compile
GOOS=linux GOARCH=amd64 go build ./cmd/vsphere-csi
```

**Container Image:**

```dockerfile
# Standard Go application
FROM photon:5.0

# Just copy the binary
COPY vsphere-csi /bin/vsphere-csi
```

**Image Size:** ~130MB (without VDDK)

**Characteristics:**
- ✅ Simple build process
- ✅ No external dependencies
- ✅ Standard Go toolchain
- ✅ Smaller container images
- ✅ Fast compilation
- ✅ True cross-compilation

### 5. Development Experience

#### VDDK

**Local Development (macOS):**

```bash
# Cannot build natively on macOS
❌ make build-csi
# Error: build constraints exclude all Go files

# Must use Docker
✅ make docker-build-csi
# Takes 2-3 minutes per build

# Cannot run locally
❌ ./vsphere-csi
# Binary is Linux ELF format

# Must deploy to test
kubectl apply -f deployment.yaml
```

**Iteration Cycle:**
1. Edit code (on macOS)
2. Build in Docker (2-3 min)
3. Create container image (1-2 min)
4. Push to registry (1-2 min)
5. Deploy to cluster (1 min)
6. Test

**Total:** ~7-10 minutes per iteration

#### FCD APIs

**Local Development (macOS):**

```bash
# Build natively
✅ make build-csi
# Takes 30 seconds

# Run locally (with stub data)
✅ ./vsphere-csi
# Can test most logic locally

# Or deploy to test
kubectl apply -f deployment.yaml
```

**Iteration Cycle:**
1. Edit code (on macOS)
2. Build natively (30 sec)
3. Test locally or deploy

**Total:** ~1-3 minutes per iteration

### 6. Connection and Architecture

#### VDDK

```
┌─────────────┐
│  CSI Driver │
│   (Linux)   │
└─────┬───────┘
      │ CGO
      ▼
┌─────────────┐
│    VDDK     │
│  C Library  │
└─────┬───────┘
      │ Direct Access
      ▼
┌─────────────┐
│ ESXi Host   │
│  Datastore  │
└─────────────┘
```

**Characteristics:**
- Direct disk access
- Low latency (after connection)
- Can work without vCenter
- Requires network access to ESXi
- Stateful connections (handles)

#### FCD APIs

```
┌─────────────┐
│  CSI Driver │
│  (Any OS)   │
└─────┬───────┘
      │ SOAP/REST
      ▼
┌─────────────┐
│  vCenter    │
│    VSLM     │
└─────┬───────┘
      │
      ▼
┌─────────────┐
│ ESXi Host   │
│  Datastore  │
└─────────────┘
```

**Characteristics:**
- Through vCenter (single endpoint)
- Network latency per request
- Requires vCenter availability
- Stateless API calls
- Better for distributed systems

### 7. Performance Analysis

#### VDDK Performance

**Advantages:**
- ✅ Direct disk access (no vCenter overhead)
- ✅ Persistent connections (amortized setup cost)
- ✅ Lower latency for bulk operations
- ✅ Can read actual data if needed

**Disadvantages:**
- ❌ Connection setup overhead
- ❌ Must manage disk handles
- ❌ Network directly to ESXi hosts
- ❌ More complex error handling

**Typical Query:**
```
Connection: 100-200ms (one-time)
Query:      10-50ms per call
Total:      ~150-250ms for first query
           ~10-50ms for subsequent queries
```

#### FCD API Performance

**Advantages:**
- ✅ Stateless (no connection management)
- ✅ Single network endpoint (vCenter)
- ✅ vCenter handles ESXi communication
- ✅ Can leverage vCenter caching

**Disadvantages:**
- ❌ Network round-trip per call
- ❌ SOAP protocol overhead
- ❌ vCenter processing time
- ❌ Dependent on vCenter performance

**Typical Query:**
```
API Call:   50-150ms per call
            (includes vCenter processing)
Total:      ~50-150ms per query
```

**Conclusion:** VDDK is faster for bulk operations, FCD APIs have more consistent latency.

### 8. Code Complexity

#### VDDK Implementation

```go
// Complex setup with resource management
func queryAllocatedBlocks(ctx, volumeID, snapshotID string, ...) {
    // 1. Get vCenter connection
    vcenter, err := common.GetVCenter(ctx, c.manager)
    
    // 2. Initialize VDDK library
    err = disklib.Init(7, 0, "/usr/local/lib/vmware-vix-disklib-distrib")
    defer disklib.Exit()
    
    // 3. Prepare for access
    err = disklib.PrepareForAccess(connectParams)
    defer disklib.EndAccess(connectParams)
    
    // 4. Connect to vCenter
    connection, err := disklib.Connect(connectParams)
    defer disklib.Disconnect(connection)
    
    // 5. Open the disk
    diskHandle, err := disklib.Open(connection, diskPath)
    defer disklib.Close(diskHandle)
    
    // 6. Get disk info
    diskInfo, err := disklib.GetInfo(diskHandle)
    
    // 7. Convert bytes to sectors
    startSector := disklib.VixDiskLibSectorType(startingOffset / 512)
    numSectors := disklib.VixDiskLibSectorType(maxBytesToQuery / 512)
    
    // 8. Query allocated blocks
    allocatedBlocks, err := disklib.QueryAllocatedBlocks(
        diskHandle, startSector, numSectors, 8,
    )
    
    // 9. Convert sectors back to bytes
    for _, block := range allocatedBlocks {
        offset := uint64(block.Offset()) * 512
        length := uint64(block.Length()) * 512
        // ...
    }
}
```

**Lines of Code:** ~150-200 lines
**Error Handling Points:** 8-10
**Resource Management:** 5 defers
**Conversions:** Bytes ↔ Sectors

#### FCD API Implementation

```go
// Simple API call
func queryAllocatedBlocks(ctx, volumeID, snapshotID string, ...) {
    // 1. Get vCenter connection
    vcenter, err := common.GetVCenter(ctx, c.manager)
    
    // 2. Create VSLM client
    vslmClient, err := vslm.NewClient(ctx, vcenter.Client.Client)
    globalObjectManager := vslm.NewGlobalObjectManager(vslmClient)
    
    // 3. Query allocated blocks
    diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
        ctx,
        types.ID{Id: volumeID},
        types.ID{Id: snapshotID},
        int64(startingOffset),
        "*",
    )
    
    // 4. Convert to result
    for _, area := range diskChangeInfo.ChangedArea {
        offset := uint64(area.Start)
        length := uint64(area.Length)
        // ...
    }
}
```

**Lines of Code:** ~50-80 lines
**Error Handling Points:** 2-3
**Resource Management:** None (stateless)
**Conversions:** None (bytes throughout)

### 9. Error Handling

#### VDDK Errors

```go
// Multiple failure points
if err := disklib.Init(...); err != nil {
    return fmt.Errorf("failed to initialize VDDK: %v", err)
}

if err := disklib.PrepareForAccess(...); err != nil {
    return fmt.Errorf("failed to prepare for access: %v", err)
}

if err := disklib.Connect(...); err != nil {
    return fmt.Errorf("failed to connect: %v", err)
}

// VDDK-specific errors
// - VIX_E_DISK_LOCKED
// - VIX_E_INVALID_HANDLE
// - VIX_E_DISK_INVAL
```

#### FCD API Errors

```go
// Simple SOAP fault handling
if err := globalObjectManager.QueryChangedDiskAreas(...); err != nil {
    if soap.IsSoapFault(err) {
        fault := soap.ToSoapFault(err)
        switch fault.VimFault().(type) {
        case *types.NotFound:
            return fmt.Errorf("snapshot not found")
        case *types.InvalidArgument:
            return fmt.Errorf("invalid changeId")
        }
    }
    return fmt.Errorf("query failed: %v", err)
}
```

### 10. Testing

#### VDDK Testing

**Unit Tests:**
```go
// Difficult - requires VDDK library
// Must run on Linux
// Need mock C library or integration test
```

**Integration Tests:**
```go
// Requires:
// - Linux environment
// - VDDK installation
// - ESXi host access
// - Test volumes with CBT
```

**CI/CD:**
```yaml
# Complex CI setup
- Install VDDK
- Use Docker for builds
- Deploy to test cluster
- Run integration tests
```

#### FCD API Testing

**Unit Tests:**
```go
// Easy - pure Go mocking
type mockGlobalObjectManager struct {
    queryFunc func(...) (*types.DiskChangeInfo, error)
}

func TestQueryAllocatedBlocks(t *testing.T) {
    mock := &mockGlobalObjectManager{
        queryFunc: func(...) (*types.DiskChangeInfo, error) {
            return &types.DiskChangeInfo{...}, nil
        },
    }
    // Test with mock
}
```

**Integration Tests:**
```go
// Requires:
// - vCenter access
// - Test volumes with CBT
// - Can run from any platform
```

**CI/CD:**
```yaml
# Simple CI setup
- Standard Go build
- Run unit tests
- Deploy to test cluster
- Run integration tests
```

### 11. Maintenance and Support

#### VDDK

**Updates:**
- Tied to VDDK releases
- Must update VDDK libraries
- Rebuild containers with new VDDK
- Test compatibility

**Support:**
- VMware VDDK documentation
- C API documentation
- CGO debugging required
- Platform-specific issues

**Dependencies:**
```
vsphere-csi-driver
  ├── virtual-disks (wrapper)
  │   └── VDDK C library
  │       └── System libraries (Linux)
  └── govmomi (for other operations)
```

#### FCD APIs

**Updates:**
- Tied to govmomi releases
- Pure Go dependency updates
- Standard Go module management
- Automatic compatibility

**Support:**
- govmomi documentation
- vSphere API documentation
- Standard Go debugging
- Cross-platform

**Dependencies:**
```
vsphere-csi-driver
  └── govmomi
      └── Standard library
```

### 12. Use Case Recommendations

#### Use VDDK When:

1. **Performance is Critical**
   - High-frequency queries
   - Large-scale backup operations
   - Low-latency requirements

2. **Direct Access Needed**
   - Reading actual disk data
   - Advanced VMDK operations
   - Need disk-level features

3. **vCenter Limitations**
   - vCenter not always available
   - Need to work with ESXi directly
   - vCenter performance concerns

4. **Existing Infrastructure**
   - Already using VDDK elsewhere
   - Established build pipelines
   - Linux-only deployment acceptable

#### Use FCD APIs When:

1. **Simplicity Preferred**
   - Easier development and maintenance
   - Standard Go toolchain
   - Cross-platform development

2. **Quick Development**
   - Rapid prototyping
   - Frequent iterations
   - Multiple developers (cross-platform)

3. **Container Size Matters**
   - Minimal images preferred
   - Bandwidth constraints
   - Storage optimization

4. **Cloud Native**
   - Kubernetes-first approach
   - Stateless architecture
   - Microservices patterns

5. **vCenter Available**
   - vCenter always accessible
   - Centralized management
   - vCenter HA setup

## Hybrid Approach

### Recommendation

Implement **both** approaches with build flags:

```go
// Build with VDDK (performance)
// go build -tags vddk

// Build with FCD APIs (simplicity)
// go build

// Makefile
.PHONY: build-csi-vddk
build-csi-vddk:
	go build -tags vddk -o vsphere-csi ./cmd/vsphere-csi

.PHONY: build-csi-fcd
build-csi-fcd:
	go build -o vsphere-csi ./cmd/vsphere-csi
```

### Benefits

1. **Development**: Use FCD APIs (fast, cross-platform)
2. **Production**: Choose based on requirements
3. **Flexibility**: Switch between implementations
4. **Testing**: Test both approaches

## Conclusion

### Summary Table

| Criteria | Winner | Reason |
|----------|--------|--------|
| **Performance** | VDDK | Direct disk access |
| **Simplicity** | FCD APIs | Pure Go, no CGO |
| **Dev Experience** | FCD APIs | Cross-platform, fast builds |
| **Build Process** | FCD APIs | Standard Go toolchain |
| **Container Size** | FCD APIs | No VDDK libraries |
| **Portability** | FCD APIs | Works on any platform |
| **Feature Completeness** | VDDK | More disk operations available |
| **Maintenance** | FCD APIs | Simpler dependencies |
| **Testing** | FCD APIs | Easy mocking, cross-platform |
| **API Support** | Tie | Both officially supported |

### Final Recommendation

**For vsphere-csi-driver specifically:**

1. **Start with FCD APIs**
   - Easier to implement and test
   - Better developer experience
   - Sufficient performance for most use cases
   - Can always add VDDK later if needed

2. **Add VDDK as optional**
   - Build flag for VDDK support
   - Use for performance-critical deployments
   - Keep both implementations maintained

3. **Let users choose**
   - Provide both container images
   - Document trade-offs
   - Allow deployment-time choice

```
vsphere-csi-driver:latest        (FCD APIs - default)
vsphere-csi-driver:vddk         (VDDK - performance)
```

This approach provides maximum flexibility while maintaining the best developer experience.

## References

- [VDDK Implementation Guide](./CBT-VDDK-IMPLEMENTATION.md)
- [FCD API Implementation Guide](./FCD-CBT-APIs.md)
- [FCD API Quick Reference](./FCD-API-QUICK-REFERENCE.md)
- [VDDK Setup Guide](./VDDK-SETUP.md)
- [govmomi Documentation](https://pkg.go.dev/github.com/vmware/govmomi)
- [VSLM API Reference](https://developer.broadcom.com/xapis/virtual-infrastructure-json-api/latest/storage-lifecycle-management/)

---

**Document Version**: 1.0  
**Last Updated**: November 21, 2025  
**Status**: Comparison and Recommendation

