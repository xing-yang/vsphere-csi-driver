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

// Package hms is a minimal client for the vSphere Replication HMS VMODL API
// (hms.version.version29) covering CNS volume replication groups: configure,
// progress, on-demand sync, unconfigure, disaster recovery and reprotect.
// Only the calls that the CSI driver needs are implemented.
package hms

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Group status values reported by HMS (HmsGroupStatus).
const (
	StatusError           = "error"
	StatusMoving          = "moving"
	StatusConfiguring     = "configuring"
	StatusPaused          = "paused"
	StatusFullSync        = "fullSync"
	StatusInitialFullSync = "initialFullSync"
	StatusSync            = "sync"
	StatusNotActive       = "notActive"
	StatusOK              = "OK"
	StatusUnknown         = "unknown"
	StatusRecovering      = "recovering"
	StatusRecovered       = "recovered"
	StatusDiskResizing    = "diskResizing"
)

// Policy health values reported by the replicator-service (HmsPolicyProgress.health).
const (
	HealthProgressing = "progressing"
	HealthStalled     = "stalled"
	HealthIdle        = "idle"
)

// RecoveryType is HmsRecoveryManagerRecoveryType. testRecovery is not
// supported for CNS groups.
type RecoveryType string

const (
	RecoveryPlannedMigration RecoveryType = "plannedMigration"
	RecoveryDisaster         RecoveryType = "disasterRecovery"
)

// Errors returned by Client implementations. Faults from HMS match these
// with errors.Is.
var (
	// ErrNotFound means the group or another referenced object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNotSupported means HMS does not support the operation for CNS groups.
	ErrNotSupported = errors.New("not supported")
	// ErrAlreadyRecovered means the secondary group is already recovered.
	ErrAlreadyRecovered = errors.New("already recovered")
	// ErrSyncOngoing means a sync cycle is already running.
	ErrSyncOngoing = errors.New("sync ongoing")
	// ErrNotAuthenticated means the HMS session is not logged in or expired.
	ErrNotAuthenticated = errors.New("not authenticated")
	// ErrNoPeer means the target site is not paired with this HMS.
	ErrNoPeer = errors.New("site not paired")
	// ErrPeerUnreachable means the peer HMS could not be reached or logged in to.
	ErrPeerUnreachable = errors.New("peer unreachable")
	// ErrInvalidState means the group is in a state that does not allow the operation.
	ErrInvalidState = errors.New("invalid state")
)

// MoRef is a managed object reference.
type MoRef struct {
	Type  string
	Value string
}

// Group is an HMS replication group as listed by the replication managers.
type Group struct {
	Ref    MoRef
	Name   string
	Status string
	// RPO is the recovery point objective.
	RPO time.Duration
	// PeerHealthy reports whether the peer HMS is connected.
	PeerHealthy bool
	// Peer is the paired HMS server of the group, if HMS reported it.
	Peer *MoRef
}

// PolicyProgress is the replicator-service progress of a CNS group. It is
// only available on the primary site.
type PolicyProgress struct {
	// CurrentGeneration advances by one per completed sync cycle. Nil until the
	// first (full) sync completes.
	CurrentGeneration *int64
	// LastSyncDuration is how long the last completed sync cycle took.
	LastSyncDuration *time.Duration
	// LastSyncBytes is the number of bytes the last completed cycle transferred.
	LastSyncBytes *int64
	// LiveBytesTransferred and LiveBytesToTransfer describe the running cycle.
	LiveBytesTransferred *int64
	LiveBytesToTransfer  *int64
	Health               string
}

// CnsVolumeReplicationSpec configures replication of one CNS volume.
type CnsVolumeReplicationSpec struct {
	// Name is the group name. It must be unique on the HMS.
	Name string
	// TargetSite is the site name (or pairedVcSiteName) of the target HMS server.
	TargetSite string
	// VolumeID is the CNS volume (FCD) ID.
	VolumeID string
	// SourceDatastore is the source-site datastore holding the volume.
	SourceDatastore MoRef
	// TargetDatastore is the target-site datastore for the replica disk.
	TargetDatastore MoRef
	// TargetStorageProfileID is an optional target-site SPBM policy for the replica.
	TargetStorageProfileID string
	// RPO is rounded up to whole minutes.
	RPO time.Duration
}

// RecoverSpec recovers a secondary CNS group.
type RecoverSpec struct {
	// Folder and Pool are required by HMS even though no VM is registered.
	Folder MoRef
	Pool   MoRef
	Type   RecoveryType
}

// DatastorePath is HmsDatastorePath, a file on a datastore.
type DatastorePath struct {
	Datastore MoRef
	// Path is the folder relative to the datastore root.
	Path     string
	FileName string
}

// RelativePath returns the path of the file relative to the datastore root.
func (p DatastorePath) RelativePath() string {
	if p.FileName == "" {
		return p.Path
	}
	return p.Path + "/" + p.FileName
}

// ReprotectSpec is HmsCnsPeReprotectSpec. It reverses a recovered CNS group:
// the volume registered from the recovered disk becomes the source, and the
// original source volume on the peer site becomes the target.
type ReprotectSpec struct {
	// OperationID identifies the operation and is reused for retries. Use
	// ReprotectOperationID so that the checkpoint sealed at demote matches.
	OperationID string
	// VolumeID and Datastore are the CNS volume registered from the recovered
	// disk, on the site of the recovered group.
	VolumeID  string
	Datastore MoRef
	// ForceFullSync discards the sealed checkpoint and copies the whole disk.
	ForceFullSync bool
}

// Reprotect phases reported by HMS (PeReprotectJournal).
const (
	ReprotectPhaseComplete = "complete"
)

// ReprotectOperation is HmsPeReprotectOperation, the durable progress of a
// reprotect on the site of the recovered group.
type ReprotectOperation struct {
	OperationID     string
	OriginalGroupID string
	Phase           string
	ReverseGroup    *MoRef
	// SynchronizationMode is pending, optimized, fullSync or fileSync.
	SynchronizationMode string
	FallbackReason      string
	// Error is set when the last attempt failed. Resume the operation to retry.
	Error string
}

// reprotectNamespace makes reprotect operation IDs stable across sites.
var reprotectNamespace = uuid.MustParse("6f1e3d0a-8c1b-4c55-9a43-3b7c0e6d2f10")

// ReprotectOperationID returns the reprotect operation ID for a group. The
// group ID is the same on both sites, so the source site can seal a recovery
// checkpoint under the ID that the reprotect later uses on the target site.
// A reprotect creates a group with a new ID, so every failover round gets
// its own operation.
func ReprotectOperationID(group *Group) string {
	return uuid.NewSHA1(reprotectNamespace, []byte(group.Ref.Value)).String()
}

// Client is the subset of HMS that the CSI driver uses. All operations act on
// the local HMS; Configure and Unconfigure also reach the peer HMS through the
// local one. Peer returns a client for the peer HMS itself, which reprotect
// needs because it runs on the site of the recovered group.
//
// The recovery checkpoint and reprotect calls need an HMS with CNS reprotect
// support; older HMS builds return ErrNotSupported.
type Client interface {
	// LocalSite returns a name for the local site, for messages.
	LocalSite() string
	// FindOutgoingGroup returns the primary group with the given name, or ErrNotFound.
	FindOutgoingGroup(ctx context.Context, name string) (*Group, error)
	// FindIncomingGroup returns the secondary group with the given name, or ErrNotFound.
	FindIncomingGroup(ctx context.Context, name string) (*Group, error)
	// ConfigureCnsVolumeReplication creates a primary group and its secondary
	// group on the target site, and waits for the configure task.
	ConfigureCnsVolumeReplication(ctx context.Context, spec CnsVolumeReplicationSpec) (*Group, error)
	// GetPolicyProgress returns the progress of a primary group, or nil if
	// HMS reports none.
	GetPolicyProgress(ctx context.Context, group *Group) (*PolicyProgress, error)
	// OnlineSync runs a sync cycle now and waits for it.
	OnlineSync(ctx context.Context, group *Group) error
	// Unconfigure removes a primary group and its secondary group.
	Unconfigure(ctx context.Context, group *Group, preserveReplicaDisks bool) error
	// RecoverGroup recovers a secondary group from its latest replica and
	// waits for the task. The replica disk is left unregistered.
	RecoverGroup(ctx context.Context, group *Group, spec RecoverSpec) error
	// GroupDatastore returns the target datastore of a CNS group from its
	// current spec, which is the datastore holding the replica.
	GroupDatastore(ctx context.Context, group *Group) (MoRef, error)

	// PrepareRecovery runs the final sync of a primary group and records it
	// as the baseline of an optimized reprotect (HmsPrepareCnsRecovery). The
	// writers must be stopped.
	PrepareRecovery(ctx context.Context, group *Group, operationID string) error
	// SealRecovery checks that the volume did not change since
	// PrepareRecovery and hands the baseline to the secondary group
	// (HmsSealCnsRecovery). The volume must be detached.
	SealRecovery(ctx context.Context, group *Group, operationID string) error
	// RecoveredDisk returns the committed disk of a recovered secondary group.
	RecoveredDisk(ctx context.Context, group *Group) (*DatastorePath, error)
	// BindRecoveredVolume records the CNS volume registered from the disk of
	// a recovered secondary group (HmsBindRecoveredCnsVolume). The volume
	// must be detached.
	BindRecoveredVolume(ctx context.Context, group *Group, volumeID string, datastore MoRef) error
	// StartReprotect starts reversing a recovered secondary group and returns
	// without waiting (HmsReverseConfigurePeReplication). Follow it with
	// ReprotectOperation.
	StartReprotect(ctx context.Context, group *Group, spec ReprotectSpec) error
	// ReprotectOperation returns a reprotect operation, or ErrNotFound.
	ReprotectOperation(ctx context.Context, operationID string) (*ReprotectOperation, error)
	// ResumeReprotect retries a failed reprotect operation without waiting.
	// group is the recovered secondary group, or nil if it is gone.
	ResumeReprotect(ctx context.Context, group *Group, operationID string) error
	// Peer returns a client for the paired HMS, logged in with the peer
	// credentials. It fails with ErrPeerUnreachable if the peer HMS endpoint
	// is not configured.
	Peer(ctx context.Context) (Client, error)
}

// ReplicaDiskPath returns the path of the replica disk of a secondary CNS
// group, relative to the datastore root
// (ConfigureReplicationSecondaryCnsWorkflow, PeTargetStorageHelper). It does
// not apply to reverse groups, which replicate into the original volume; use
// RecoveredDisk where HMS supports it.
func ReplicaDiskPath(group *Group) string {
	return "hbr-pe-gid-" + group.Ref.Value + "/cnsVolume.vmdk"
}
