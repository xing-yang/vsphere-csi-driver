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

// Package snapservice contains a client abstraction for the vSphere SnapService
// (vSAN Data Protection) protection group API, used to replicate CNS volumes.
//
// The types mirror the public SnapService REST API (vSphere API 9.1). FCD
// membership in protection groups is assumed to be added in 9.2 through the
// same TargetEntities structure that holds VMs today.
package snapservice

import (
	"errors"
	"time"
)

// ProtectionGroupState is the status of a protection group, mirroring
// Snapservice ProtectionGroupStatus.
type ProtectionGroupState string

const (
	// StateActive means local protection and outgoing replication are enabled.
	StateActive ProtectionGroupState = "ACTIVE"
	// StatePaused means snapshots are paused.
	StatePaused ProtectionGroupState = "PAUSED"
	// StateDemoted means there is no outgoing or incoming replication and local
	// snapshots are stopped.
	StateDemoted ProtectionGroupState = "DEMOTED"
	// StateDormant means local protection and outgoing replication are disabled
	// and the members act as replicas for incoming replication.
	StateDormant ProtectionGroupState = "DORMANT"
	// StateRecovery means site recovery operations (planned migration or
	// unplanned failover) can be initiated.
	StateRecovery ProtectionGroupState = "RECOVERY"
)

// TargetEntities is the membership of a protection group.
type TargetEntities struct {
	// VMs are VirtualMachine identifiers (public 9.1 API).
	VMs []string
	// FCDs are CNS volume / FCD identifiers.
	// TODO: The field name is assumed until the 9.2 API is published.
	FCDs []string
}

// ReplicationPolicy describes where and how often a protection group replicates.
type ReplicationPolicy struct {
	// TargetSite is the SnapService site the group replicates to.
	TargetSite string
	// TargetDatastore is the datastore on the target site for replicas.
	TargetDatastore string
	// TargetStoragePolicy is the storage policy applied to replicas.
	TargetStoragePolicy string
	// RPO is the recovery point objective.
	RPO time.Duration
}

// ProtectionGroupSpec is the input to create a protection group.
type ProtectionGroupSpec struct {
	Name           string
	TargetEntities TargetEntities
	Replication    *ReplicationPolicy
}

// ProtectionGroup is the observed state of a protection group on one site.
type ProtectionGroup struct {
	// ID is the site-local protection group identifier.
	ID string
	// GlobalID links the protection groups of a replication pair across sites.
	GlobalID       string
	Name           string
	State          ProtectionGroupState
	TargetEntities TargetEntities
	Replication    *ReplicationPolicy
	// ReplicationHealthy is false when replication is failing or the peer is
	// unreachable.
	ReplicationHealthy bool
	// InitialSyncDone is true once the first full replica is complete.
	InitialSyncDone bool
	// LastReplicaTime is the time of the latest replicated point in time.
	LastReplicaTime *time.Time
}

// RecoveredDisk describes an FCD recovered on the local site by a promote.
type RecoveredDisk struct {
	// SourceID is the FCD ID on the source site.
	SourceID string
	// RecoveredID is the FCD ID on the local site, if SnapService registered
	// the disk. It is empty if the caller must register it.
	RecoveredID string
	// DiskURLPath is the datastore URL path of the recovered VMDK.
	DiskURLPath string
}

// PromoteResult is the result of a promote operation.
type PromoteResult struct {
	RecoveredDisks []RecoveredDisk
}

var (
	// ErrNotFound is returned when a protection group does not exist.
	ErrNotFound = errors.New("protection group not found")
	// ErrAlreadyExists is returned when creating a protection group whose name is taken.
	ErrAlreadyExists = errors.New("protection group already exists")
	// ErrInvalidState is returned when an operation is not allowed in the
	// current protection group state.
	ErrInvalidState = errors.New("operation not allowed in current protection group state")
	// ErrPeerActive is returned by promote while the peer protection group is
	// reachable and still active, with or without force. Force only overrides
	// an unreachable peer.
	ErrPeerActive = errors.New("peer protection group is still active")
	// ErrPeerUnreachable is returned when the operation requires the peer site
	// and it cannot be reached.
	ErrPeerUnreachable = errors.New("peer site is unreachable")
	// ErrSiteUnavailable is returned when the local SnapService cannot be reached.
	ErrSiteUnavailable = errors.New("snapservice is unavailable")
	// ErrNoSitePair is returned when the target site is not paired.
	ErrNoSitePair = errors.New("target site is not paired")
)
