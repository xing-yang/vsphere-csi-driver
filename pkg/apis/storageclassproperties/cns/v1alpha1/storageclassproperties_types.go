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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StorageClassPropertiesSpec defines the desired state of StorageClassProperties
type StorageClassPropertiesSpec struct {
	// StorageClassName is the name of the StorageClass or VolumeAttributesClass
	// this properties object describes
	StorageClassName string `json:"storageClassName"`

	// StoragePolicyID is the vSphere storage policy ID
	StoragePolicyID string `json:"storagePolicyID"`

	// Kind specifies whether this is for a StorageClass or VolumeAttributesClass
	// Valid values: "StorageClass", "VolumeAttributesClass"
	// +kubebuilder:validation:Enum=StorageClass;VolumeAttributesClass
	Kind string `json:"kind"`
}

// StorageClassPropertiesStatus defines the observed state of StorageClassProperties
type StorageClassPropertiesStatus struct {
	// StorageTypeInfo describes the underlying storage type
	// +optional
	StorageTypeInfo *StorageTypeInfo `json:"storageTypeInfo,omitempty"`

	// VolumeTypeInfo describes supported volume types and access modes
	// +optional
	VolumeTypeInfo *VolumeTypeInfo `json:"volumeTypeInfo,omitempty"`

	// TopologyInfo describes the topology characteristics of the storage
	// +optional
	TopologyInfo *TopologyInfo `json:"topologyInfo,omitempty"`

	// EncryptionInfo describes encryption capabilities
	// +optional
	EncryptionInfo *EncryptionInfo `json:"encryptionInfo,omitempty"`

	// PerformanceInfo describes performance characteristics
	// +optional
	PerformanceInfo *PerformanceInfo `json:"performanceInfo,omitempty"`
}

// StorageTypeInfo describes the underlying storage type
type StorageTypeInfo struct {
	// DatastoreType indicates the type of datastore
	// Valid values: "vsan", "vmfs", "nfs", "vvol"
	// +kubebuilder:validation:Enum=vsan;vmfs;nfs;vvol
	DatastoreType string `json:"datastoreType"`

	// VsanArchitecture indicates the vSAN architecture (only for vsan datastoreType)
	// Valid values: "esa", "osa"
	// +optional
	// +kubebuilder:validation:Enum=esa;osa
	VsanArchitecture string `json:"vsanArchitecture,omitempty"`

	// VsanVersion is the vSAN version (only for vsan datastoreType)
	// +optional
	VsanVersion string `json:"vsanVersion,omitempty"`

	// VmfsVersion is the VMFS version (only for vmfs datastoreType)
	// +optional
	VmfsVersion string `json:"vmfsVersion,omitempty"`

	// NfsVersion is the NFS version (only for nfs datastoreType)
	// +optional
	NfsVersion string `json:"nfsVersion,omitempty"`
}

// VolumeTypeInfo describes supported volume types and access modes
type VolumeTypeInfo struct {
	// SupportedAccessModes lists the access modes supported by this storage class
	// Valid values: "ReadWriteOnce", "ReadWriteMany", "ReadOnlyMany"
	// +optional
	SupportedAccessModes []string `json:"supportedAccessModes,omitempty"`

	// SupportedVolumeModes lists the volume modes supported
	// Valid values: "Filesystem", "Block"
	// +optional
	SupportedVolumeModes []string `json:"supportedVolumeModes,omitempty"`

	// FileSystemTypes lists supported file system types for filesystem volumes
	// +optional
	FileSystemTypes []string `json:"fileSystemTypes,omitempty"`

	// BlockSupport indicates whether raw block volumes are supported
	// +optional
	BlockSupport bool `json:"blockSupport,omitempty"`
}

// TopologyInfo describes the topology characteristics of the storage
type TopologyInfo struct {
	// Type indicates whether the storage is zonal or cross-zonal
	// Valid values: "zonal", "cross-zonal"
	// +kubebuilder:validation:Enum=zonal;cross-zonal
	Type string `json:"type"`

	// AccessibleZones lists the zones where this storage class can be accessed
	// +optional
	AccessibleZones []string `json:"accessibleZones,omitempty"`

	// BackingZones lists the zones where the physical storage resides
	// +optional
	BackingZones []string `json:"backingZones,omitempty"`
}

// EncryptionInfo describes encryption capabilities
type EncryptionInfo struct {
	// Supported indicates whether encryption is supported
	Supported bool `json:"supported"`

	// EncryptionType indicates the type of encryption
	// Valid values: "vm-crypt", "storage-level", "none"
	// +optional
	// +kubebuilder:validation:Enum=vm-crypt;storage-level;none
	EncryptionType string `json:"encryptionType,omitempty"`

	// CryptoKeyId is the identifier for the encryption key (if applicable)
	// +optional
	CryptoKeyId string `json:"cryptoKeyId,omitempty"`

	// KeyProviderId is the identifier for the key provider (if applicable)
	// +optional
	KeyProviderId string `json:"keyProviderId,omitempty"`
}

// PerformanceInfo describes performance characteristics
type PerformanceInfo struct {
	// IopsLimit is the IOPS limit for volumes created from this storage class
	// +optional
	IopsLimit *int64 `json:"iopsLimit,omitempty"`

	// StorageTier indicates the storage tier
	// Valid values: "all-flash", "hybrid"
	// +optional
	// +kubebuilder:validation:Enum=all-flash;hybrid
	StorageTier string `json:"storageTier,omitempty"`

	// DiskStripes is the number of disk stripes
	// +optional
	DiskStripes *int32 `json:"diskStripes,omitempty"`

	// RaidType indicates the RAID configuration
	// +optional
	RaidType string `json:"raidType,omitempty"`
}

// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StorageClassProperties is the Schema for the storageclassproperties API
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
type StorageClassProperties struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClassPropertiesSpec   `json:"spec,omitempty"`
	Status StorageClassPropertiesStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StorageClassPropertiesList contains a list of StorageClassProperties
type StorageClassPropertiesList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageClassProperties `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageClassProperties{}, &StorageClassPropertiesList{})
}
