# Changed Block Tracking (CBT) APIs - Complete Overview

## Introduction

This document provides an overview of the Changed Block Tracking (CBT) implementation options for the vsphere-csi-driver project, comparing two distinct approaches: VDDK (Virtual Disk Development Kit) and FCD (First Class Disk) APIs.

## Quick Navigation

### For Developers

- **Want to understand the big picture?** → Read this document
- **Want to implement using VDDK?** → See [CBT-VDDK-IMPLEMENTATION.md](./CBT-VDDK-IMPLEMENTATION.md)
- **Want to implement using FCD APIs?** → See [FCD-CBT-APIs.md](./FCD-CBT-APIs.md)
- **Need a quick FCD API reference?** → See [FCD-API-QUICK-REFERENCE.md](./FCD-API-QUICK-REFERENCE.md)
- **Want to compare both approaches?** → See [CBT-IMPLEMENTATION-COMPARISON.md](./CBT-IMPLEMENTATION-COMPARISON.md)
- **Need VDDK setup instructions?** → See [VDDK-SETUP.md](./VDDK-SETUP.md)

## What is Changed Block Tracking (CBT)?

Changed Block Tracking is a vSphere feature that keeps track of which blocks on a virtual disk have changed. This is essential for:

- **Incremental Backups**: Only backup changed blocks
- **Replication**: Sync only modified data
- **Storage Efficiency**: Understand actual data usage
- **Performance**: Avoid scanning entire disks

## CSI APIs to Implement

The CSI (Container Storage Interface) specification defines two CBT-related APIs:

### 1. GetMetadataAllocated

Returns the **allocated blocks** (non-zero data blocks) in a snapshot.

**CSI Request:**
```protobuf
message GetMetadataAllocatedRequest {
  string snapshot_id = 1;      // Which snapshot to query
  uint64 starting_offset = 2;  // Where to start (pagination)
  uint32 max_results = 3;      // Max blocks to return
}
```

**CSI Response:**
```protobuf
message GetMetadataAllocatedResponse {
  repeated BlockMetadata block_metadata = 1;  // Array of allocated blocks
  uint64 volume_capacity_bytes = 2;           // Total volume size
}

message BlockMetadata {
  uint64 byte_offset = 1;  // Block start offset
  uint64 size_bytes = 2;   // Block size
}
```

**Use Case:** Initial full backup - know which blocks contain data

### 2. GetMetadataDelta

Returns the **changed blocks** between two snapshots.

**CSI Request:**
```protobuf
message GetMetadataDeltaRequest {
  string base_snapshot_id = 1;    // Starting point
  string target_snapshot_id = 2;  // End point
  uint64 starting_offset = 3;     // Where to start (pagination)
  uint32 max_results = 4;         // Max blocks to return
}
```

**CSI Response:**
```protobuf
message GetMetadataDeltaResponse {
  repeated BlockMetadata block_metadata = 1;  // Array of changed blocks
  uint64 volume_capacity_bytes = 2;           // Total volume size
}
```

**Use Case:** Incremental backup - know which blocks changed since last backup

## Two Implementation Approaches

### Approach 1: VDDK (Virtual Disk Development Kit)

**What it is:** VMware's C library for direct virtual disk operations

**Architecture:**
```
CSI Driver (Go + CGO)
    ↓
VDDK C Library (libvixDiskLib.so)
    ↓
ESXi Host (Direct disk access)
```

**Key APIs:**
```c
// C API (wrapped by virtual-disks Go library)
VixDiskLib_QueryAllocatedBlocks(disk, start, length, chunkSize, ...)
```

**Pros:**
- ✅ Direct disk access (fast)
- ✅ Low latency for bulk operations
- ✅ Doesn't depend on vCenter availability

**Cons:**
- ❌ Linux only (CGO + C libraries)
- ❌ Complex build process (Docker required)
- ❌ Large container images (~400MB)
- ❌ Difficult cross-platform development

**Status:** Currently implemented in `controller_cbt_linux.go`

### Approach 2: FCD APIs (First Class Disk / VSLM)

**What it is:** vSphere's SOAP/REST APIs for managing independent disks

**Architecture:**
```
CSI Driver (Pure Go)
    ↓
vCenter VSLM API (SOAP)
    ↓
vCenter → ESXi Host
```

**Key APIs:**
```go
// Pure Go API via govmomi
globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,      // FCD volume ID
    snapshotID,    // Snapshot ID
    startOffset,   // Byte offset
    changeId,      // "*" = all blocks, or specific ID = delta
)
```

**Pros:**
- ✅ Pure Go (cross-platform)
- ✅ Simple build process
- ✅ Small container images (~130MB)
- ✅ Easy development on any OS

**Cons:**
- ❌ Requires vCenter availability
- ❌ Network latency (SOAP overhead)
- ❌ May impact vCenter performance at scale

**Status:** Not yet implemented, but fully supported by govmomi

## The Key Insight: One API for Both Operations

The beauty of the FCD approach is that **one API handles both operations**:

### GetMetadataAllocated with FCD

```go
// Use "*" as changeId to get ALL allocated blocks
diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,
    snapshotID,
    0,       // Start from beginning
    "*",     // 🔑 Special value: "*" means "all allocated blocks"
)
```

### GetMetadataDelta with FCD

```go
// Step 1: Get the changeId from base snapshot
baseChangeId := getSnapshotChangeId(volumeID, baseSnapshotID)

// Step 2: Query changes since that changeId
diskChangeInfo, err := globalObjectManager.QueryChangedDiskAreas(
    ctx,
    volumeID,
    targetSnapshotID,
    0,
    baseChangeId,  // 🔑 Specific changeId: returns only changes since then
)
```

**The changeId parameter determines the behavior!**

## Current Implementation Status

### What's Implemented (VDDK)

**Files:**
- `pkg/csi/service/wcp/controller.go` - Main CSI controller
- `pkg/csi/service/wcp/controller_cbt_linux.go` - VDDK implementation
- `pkg/csi/service/wcp/controller_cbt_stub.go` - Stub for non-Linux
- `pkg/csi/service/wcp/controller_cbt_test.go` - Unit tests

**Build System:**
- `Makefile` - Docker-based build targets
- `images/driver/Dockerfile.vddk` - Multi-stage container build
- `.libs/vmware-vix-disklib-distrib/` - VDDK library location

**APIs Implemented:**
- ✅ `GetMetadataAllocated` via `QueryAllocatedBlocks`
- ⚠️ `GetMetadataDelta` - Using allocated blocks on both snapshots (not true delta)

### What's Not Implemented (FCD)

**Would Need:**
- `pkg/csi/service/wcp/controller_cbt_fcd.go` - FCD implementation
- Build flag to choose between VDDK and FCD
- Makefile targets for FCD-only builds
- Updated tests

**APIs Would Implement:**
- ✅ `GetMetadataAllocated` via `QueryChangedDiskAreas(changeId="*")`
- ✅ `GetMetadataDelta` via `QueryChangedDiskAreas(changeId=baseId)` with true delta

## Comparison Summary

| Aspect | VDDK | FCD APIs |
|--------|------|----------|
| **Implementation** | Complex (CGO + C) | Simple (Pure Go) |
| **Build** | Docker required | Standard Go build |
| **Dev Platform** | Linux only | macOS/Linux/Windows |
| **Container Size** | ~400MB | ~130MB |
| **Performance** | Fast (direct access) | Good (network API) |
| **vCenter Dependency** | Optional | Required |
| **GetMetadataAllocated** | `QueryAllocatedBlocks` | `QueryChangedDiskAreas("*")` |
| **GetMetadataDelta** | Manual computation | Native API support |
| **Maintenance** | Complex dependencies | Simple dependencies |

## Architecture Diagrams

### Current Architecture (VDDK)

```
┌─────────────────────────────────────────────────┐
│           vsphere-csi-driver (Linux)            │
│                                                 │
│  ┌──────────────────────────────────────────┐  │
│  │  controller.go (CSI Implementation)      │  │
│  │   ├─ GetMetadataAllocated()              │  │
│  │   └─ GetMetadataDelta()                  │  │
│  └────────────────┬─────────────────────────┘  │
│                   │                             │
│  ┌────────────────▼─────────────────────────┐  │
│  │  controller_cbt_linux.go                 │  │
│  │   ├─ queryAllocatedBlocksFromVDDK()      │  │
│  │   └─ queryChangedAreasFromVDDK()         │  │
│  └────────────────┬─────────────────────────┘  │
│                   │ CGO                         │
│  ┌────────────────▼─────────────────────────┐  │
│  │  virtual-disks (Go wrapper)              │  │
│  │   github.com/vmware/virtual-disks        │  │
│  └────────────────┬─────────────────────────┘  │
│                   │ CGO                         │
└───────────────────┼─────────────────────────────┘
                    │
    ┌───────────────▼──────────────┐
    │  VDDK C Library              │
    │  libvixDiskLib.so            │
    └───────────────┬──────────────┘
                    │ Direct Access
    ┌───────────────▼──────────────┐
    │  ESXi Host                   │
    │  Datastore / VMDK Files      │
    └──────────────────────────────┘
```

### Proposed Architecture (FCD APIs)

```
┌─────────────────────────────────────────────────┐
│       vsphere-csi-driver (Any Platform)         │
│                                                 │
│  ┌──────────────────────────────────────────┐  │
│  │  controller.go (CSI Implementation)      │  │
│  │   ├─ GetMetadataAllocated()              │  │
│  │   └─ GetMetadataDelta()                  │  │
│  └────────────────┬─────────────────────────┘  │
│                   │                             │
│  ┌────────────────▼─────────────────────────┐  │
│  │  controller_cbt_fcd.go                   │  │
│  │   ├─ queryAllocatedBlocksFromFCD()       │  │
│  │   └─ queryChangedAreasFromFCD()          │  │
│  └────────────────┬─────────────────────────┘  │
│                   │ Pure Go                     │
│  ┌────────────────▼─────────────────────────┐  │
│  │  govmomi/vslm                            │  │
│  │   GlobalObjectManager.QueryChangedDisk.. │  │
│  └────────────────┬─────────────────────────┘  │
│                   │ SOAP/REST                   │
└───────────────────┼─────────────────────────────┘
                    │
    ┌───────────────▼──────────────┐
    │  vCenter Server              │
    │  VSLM Service                │
    └───────────────┬──────────────┘
                    │
    ┌───────────────▼──────────────┐
    │  ESXi Host                   │
    │  Datastore / FCD Objects     │
    └──────────────────────────────┘
```

### Hybrid Architecture (Recommended)

```
┌──────────────────────────────────────────────────┐
│        vsphere-csi-driver (Any Platform)         │
│                                                  │
│  ┌───────────────────────────────────────────┐  │
│  │  controller.go (CSI Implementation)       │  │
│  │   ├─ GetMetadataAllocated()               │  │
│  │   └─ GetMetadataDelta()                   │  │
│  └─────────────┬─────────────────────────────┘  │
│                │                                 │
│      ┌─────────▼──────────┐                     │
│      │   Build Flag       │                     │
│      │   Decision         │                     │
│      └──┬──────────────┬──┘                     │
│         │              │                         │
│  ┌──────▼────┐   ┌─────▼─────┐                 │
│  │  VDDK     │   │  FCD APIs │                 │
│  │  (Linux)  │   │  (Any OS) │                 │
│  └───────────┘   └───────────┘                 │
└──────────────────────────────────────────────────┘

Build Options:
  go build                    → FCD APIs (default)
  go build -tags vddk        → VDDK
```

## Decision Framework

### Choose VDDK If:

```
✓ Need maximum performance
✓ Performing frequent/bulk queries
✓ Already have VDDK infrastructure
✓ Linux-only deployment acceptable
✓ vCenter availability uncertain
✓ Direct disk features needed
```

### Choose FCD APIs If:

```
✓ Want simple implementation
✓ Cross-platform development needed
✓ vCenter always available
✓ Container size matters
✓ Fast iteration/development desired
✓ Performance adequate (most cases)
```

### Use Both (Hybrid) If:

```
✓ Want flexibility
✓ Different requirements per deployment
✓ Testing and comparison needed
✓ Migration path desired
```

## Implementation Roadmap

### Phase 1: FCD Implementation (Recommended First)

1. **Week 1-2: Basic Implementation**
   - Create `controller_cbt_fcd.go`
   - Implement GetMetadataAllocated with FCD
   - Implement GetMetadataDelta with FCD
   - Basic unit tests

2. **Week 3: Integration**
   - Build flag system
   - Makefile updates
   - Integration tests
   - Documentation

3. **Week 4: Testing & Validation**
   - Performance testing
   - Comparison with VDDK
   - Bug fixes and optimization

### Phase 2: VDDK Optimization (If Needed)

1. **Improve VDDK Implementation**
   - True delta query (if available)
   - Performance optimizations
   - Better error handling

2. **Build System Refinement**
   - Streamline Docker builds
   - Automated testing
   - CI/CD integration

### Phase 3: Production Deployment

1. **Documentation**
   - Deployment guides
   - Performance tuning
   - Troubleshooting

2. **Monitoring**
   - Metrics and logging
   - Performance monitoring
   - Error tracking

## Getting Started

### For FCD Implementation

```bash
# 1. Read the documentation
cat docs/FCD-CBT-APIs.md

# 2. Check the quick reference
cat docs/FCD-API-QUICK-REFERENCE.md

# 3. Implement the APIs
vim pkg/csi/service/wcp/controller_cbt_fcd.go

# 4. Test
go test ./pkg/csi/service/wcp/...

# 5. Build
make build-csi
```

### For VDDK Implementation

```bash
# 1. Read the documentation
cat docs/CBT-VDDK-IMPLEMENTATION.md

# 2. Setup VDDK
cat docs/VDDK-SETUP.md
make check-vddk

# 3. Build with Docker
make docker-build-csi

# 4. Create container image
make docker-images-vddk
```

## Testing Strategy

### Unit Tests

```go
// Mock VSLM client for FCD
type mockGlobalObjectManager struct {}

// Test both implementations
func TestGetMetadataAllocated_VDDK(t *testing.T) { ... }
func TestGetMetadataAllocated_FCD(t *testing.T) { ... }
```

### Integration Tests

```go
// Test against real vSphere
func TestE2E_GetMetadataAllocated(t *testing.T) {
    // Create volume
    // Create snapshot
    // Call GetMetadataAllocated
    // Verify results
}

func TestE2E_GetMetadataDelta(t *testing.T) {
    // Create volume
    // Create snapshot 1
    // Modify data
    // Create snapshot 2
    // Call GetMetadataDelta
    // Verify only changes reported
}
```

### Performance Tests

```go
// Compare performance
func BenchmarkGetMetadataAllocated_VDDK(b *testing.B) { ... }
func BenchmarkGetMetadataAllocated_FCD(b *testing.B) { ... }

// Measure:
// - Latency
// - Throughput
// - Memory usage
// - Network overhead
```

## Monitoring and Observability

### Metrics to Track

```go
// Request metrics
cbt_requests_total{operation="get_metadata_allocated",status="success"}
cbt_requests_duration_seconds{operation="get_metadata_allocated"}

// Implementation metrics
cbt_implementation{type="vddk"}
cbt_implementation{type="fcd"}

// Error metrics
cbt_errors_total{operation="get_metadata_allocated",error_type="..."}
```

### Logging

```go
log.Infof("GetMetadataAllocated: volume=%s snapshot=%s implementation=%s",
    volumeID, snapshotID, implementation)

log.Infof("Query returned %d blocks, nextOffset=%d, duration=%v",
    len(blocks), nextOffset, duration)
```

## FAQ

### Q: Can I use FCD APIs with VDDK-style volumes?

**A:** Yes! FCD APIs work with any First Class Disk, including those created by CSI. The volumeID is the FCD UUID.

### Q: Do I need CBT enabled for these APIs to work?

**A:** Yes, Changed Block Tracking must be enabled on the volume. This is typically done when creating the volume or snapshot.

### Q: What's the performance difference?

**A:** VDDK is typically 2-3x faster for large queries due to direct access. For small queries or infrequent operations, the difference is negligible.

### Q: Can I switch between implementations?

**A:** Yes, with the hybrid approach using build flags. The CSI API remains the same, only the backend implementation changes.

### Q: Which should I implement first?

**A:** Start with FCD APIs for faster development and easier testing. Add VDDK later if performance requirements demand it.

### Q: Do both implementations produce the same results?

**A:** They should produce identical results for GetMetadataAllocated. For GetMetadataDelta, FCD APIs may be more accurate as they use native CBT change tracking.

## Conclusion

Both VDDK and FCD APIs can successfully implement the CSI Changed Block Tracking operations. The choice depends on your priorities:

- **FCD APIs**: Simplicity, developer experience, portability
- **VDDK**: Performance, direct access, independence from vCenter
- **Hybrid**: Flexibility, best of both worlds

For most use cases, **starting with FCD APIs** provides the fastest path to a working implementation, with the option to add VDDK optimization later if needed.

## Next Steps

1. **Review the detailed guides**:
   - [FCD Implementation Guide](./FCD-CBT-APIs.md)
   - [VDDK Implementation Guide](./CBT-VDDK-IMPLEMENTATION.md)
   - [Comparison Document](./CBT-IMPLEMENTATION-COMPARISON.md)

2. **Choose your approach** based on requirements

3. **Implement and test** following the guides

4. **Deploy and monitor** in your environment

5. **Iterate** based on performance and feedback

## Resources

### Documentation
- [FCD API Implementation Guide](./FCD-CBT-APIs.md)
- [FCD API Quick Reference](./FCD-API-QUICK-REFERENCE.md)
- [VDDK Implementation Guide](./CBT-VDDK-IMPLEMENTATION.md)
- [Implementation Comparison](./CBT-IMPLEMENTATION-COMPARISON.md)
- [VDDK Setup Guide](./VDDK-SETUP.md)
- [Docker Build Guide](./DOCKER-BUILD.md)

### External Resources
- [govmomi Documentation](https://pkg.go.dev/github.com/vmware/govmomi)
- [VSLM API Reference](https://developer.broadcom.com/xapis/virtual-infrastructure-json-api/latest/storage-lifecycle-management/)
- [VDDK Documentation](https://developer.vmware.com/web/sdk/8.0/vddk)
- [CSI Specification](https://github.com/container-storage-interface/spec)

---

**Document Version**: 1.0  
**Last Updated**: November 21, 2025  
**Status**: Overview and Guide

