# StorageClassProperties CRD

This directory contains the `StorageClassProperties` Custom Resource Definition (CRD) for the vSphere CSI Driver.

## Overview

The `StorageClassProperties` CRD provides a unified way to expose comprehensive storage characteristics for StorageClasses and VolumeAttributesClasses. This enables DevOps teams and internal services to make informed decisions about storage selection based on:

- **Storage Type Information**: Underlying datastore type (vSAN ESA/OSA, VMFS, NFS, vVol) and versions
- **Volume Type Information**: Supported access modes (RWO, RWX, ROX) and volume modes (Filesystem, Block)
- **Topology Information**: Zonal vs cross-zonal storage, accessible zones, and backing zones
- **Encryption Information**: Encryption support and types (VM crypt, storage-level encryption)
- **Performance Information**: IOPS limits, throughput characteristics, storage tier, and RAID configuration

## Directory Structure

```
storageclassproperties/
├── README.md                    # This file
├── apis.go                      # API group registration
├── cns/
│   └── v1alpha1/
│       ├── doc.go                           # Package documentation
│       ├── register.go                      # Scheme registration
│       ├── storageclassproperties_types.go  # CRD type definitions
│       ├── zz_generated.deepcopy.go         # Generated deepcopy methods
│       └── example-storageclassproperties.yaml  # Example instances
└── config/
    ├── config.go                            # Embedded CRD file
    └── cns.vmware.com_storageclassproperties.yaml  # Generated CRD manifest
```

## API Version

- **Group**: `cns.vmware.com`
- **Version**: `v1alpha1`
- **Kind**: `StorageClassProperties`
- **Scope**: Cluster

## Usage

### Applying the CRD

```bash
kubectl apply -f pkg/apis/storageclassproperties/config/cns.vmware.com_storageclassproperties.yaml
```

### Creating a StorageClassProperties Instance

See `cns/v1alpha1/example-storageclassproperties.yaml` for complete examples.

```yaml
apiVersion: cns.vmware.com/v1alpha1
kind: StorageClassProperties
metadata:
  name: premium-vsan-esa-properties
spec:
  storageClassName: premium-vsan-esa
  driver: csi.vsphere.vmware.com
  kind: StorageClass
status:
  storageTypeInfo:
    datastoreType: vsan
    vsanArchitecture: esa
    vsanVersion: "8.0"
  volumeTypeInfo:
    supportedAccessModes:
      - ReadWriteOnce
      - ReadWriteMany
    supportedVolumeModes:
      - Filesystem
      - Block
    fileSystemTypes:
      - ext4
      - xfs
    blockSupport: true
  # ... additional status fields
```

### Querying StorageClassProperties

```bash
# List all StorageClassProperties
kubectl get storageclassproperties

# Get details for a specific StorageClass
kubectl get storageclassproperties premium-vsan-esa-properties -o yaml

# Find all vSAN ESA storage classes
kubectl get storageclassproperties -o json | \
  jq '.items[] | select(.status.storageTypeInfo.vsanArchitecture == "esa") | .spec.storageClassName'

# Find storage classes that support RWX
kubectl get storageclassproperties -o json | \
  jq '.items[] | select(.status.volumeTypeInfo.supportedAccessModes[]? == "ReadWriteMany") | .spec.storageClassName'
```

## Code Generation

This CRD uses Kubernetes code generators. To regenerate the CRD manifest and deepcopy methods:

```bash
# Generate CRD manifest
controller-gen crd paths=./pkg/apis/storageclassproperties/... output:crd:dir=pkg/apis/storageclassproperties/config

# Generate deepcopy methods
controller-gen object:headerFile=/dev/null paths=./pkg/apis/storageclassproperties/cns/v1alpha1
```

## Integration

The vSphere CSI Driver controller should create and maintain `StorageClassProperties` instances for each StorageClass and VolumeAttributesClass. The controller:

1. Queries vCenter APIs (SPBM, CNS, PBM) to gather storage characteristics
2. Creates/updates `StorageClassProperties` instances with the gathered information
3. Keeps the information synchronized with changes in vCenter

## Use Cases

### For DevOps Teams

- **Storage Selection**: Choose appropriate storage classes based on performance, encryption, and topology requirements
- **Capacity Planning**: Understand storage characteristics for workload placement
- **Compliance**: Verify encryption and data locality requirements

### For Internal Services

- **DSM (Data Services Manager)**: Validate vSAN ESA for DB Fast Clone feature
- **Backup Services**: Identify storage classes with snapshot capabilities
- **Security Services**: Enforce encryption policies

### Example Queries

**Find all-flash storage classes:**
```bash
kubectl get storageclassproperties -o json | \
  jq -r '.items[] | select(.status.performanceInfo.storageTier == "all-flash") | .spec.storageClassName'
```

**Find encrypted storage classes:**
```bash
kubectl get storageclassproperties -o json | \
  jq -r '.items[] | select(.status.encryptionInfo.supported == true) | .spec.storageClassName'
```

**Find cross-zonal storage classes:**
```bash
kubectl get storageclassproperties -o json | \
  jq -r '.items[] | select(.status.topologyInfo.type == "cross-zonal") | .spec.storageClassName'
```

## Documentation

For comprehensive documentation, see:
- [StorageClassProperties README](https://github.com/kubernetes-sigs/vsphere-csi-driver/blob/master/docs/STORAGECLASSPROPERTIES-README.md)
- [CRD Summary](https://github.com/kubernetes-sigs/vsphere-csi-driver/blob/master/docs/STORAGECLASSPROPERTIES-CRD-SUMMARY.md)

## Contributing

When modifying the CRD:

1. Update type definitions in `storageclassproperties_types.go`
2. Regenerate the CRD manifest and deepcopy methods
3. Update examples in `example-storageclassproperties.yaml`
4. Update documentation
5. Test with `kubectl apply` and verify the schema

## License

Copyright 2026 The Kubernetes Authors. Licensed under the Apache License, Version 2.0.
