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

// Package replication implements the csi-addons Replication service for the
// Supervisor (WCP) flavor, backed by vSphere Replication (HMS) CNS volume
// groups. Each replicated volume has its own HMS group named "cns-<volumeID>":
// a primary group on the source site and a secondary group with the same ID
// on the target site. Every driver talks only to the HMS of its own site, and
// a recovered volume is registered on the target with its source volume ID.
//
// HMS does not support reprotect or failback for CNS groups, so after a
// failover replication stops until it is configured again from the new
// primary site.
package replication

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/csi-addons/spec/lib/go/replication"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/node"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/hms"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

const (
	groupPrefix = "cns-"
	// defaultRPO is used when the VolumeReplicationClass has no
	// schedulingInterval.
	defaultRPO = 15 * time.Minute
)

// ClientFactory returns an HMS client for the local site. secrets are the
// contents of the VolumeReplicationClass secret, if one is configured.
type ClientFactory func(ctx context.Context, secrets map[string]string) (hms.Client, error)

// syncObservation records when the driver saw a group's generation advance.
// HMS reports how long the last cycle took but not when it completed.
type syncObservation struct {
	generation int64
	at         time.Time
}

// Server implements the csi-addons Replication controller service.
type Server struct {
	replication.UnimplementedControllerServer
	newClient        ClientFactory
	volumes          VolumeBackend
	locks            *node.VolumeLocks
	skipRegistration bool
	now              func() time.Time

	mu sync.Mutex
	// syncs is keyed by group ID.
	syncs map[string]syncObservation
	// demoted holds the IDs of primary groups that got their final sync.
	demoted map[string]bool
}

var _ replication.ControllerServer = &Server{}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithSkipVolumeRegistration makes PromoteVolume skip registering recovered
// disks with CNS. Only for testing with the fake HMS, whose replica disks do
// not exist.
func WithSkipVolumeRegistration(skip bool) ServerOption {
	return func(s *Server) { s.skipRegistration = skip }
}

// NewServer returns a Replication server.
func NewServer(newClient ClientFactory, volumes VolumeBackend, opts ...ServerOption) *Server {
	s := &Server{
		newClient: newClient,
		volumes:   volumes,
		locks:     node.NewVolumeLocks(),
		now:       time.Now,
		syncs:     map[string]syncObservation{},
		demoted:   map[string]bool{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// replicationRequest is implemented by every csi-addons replication request.
type replicationRequest interface {
	GetVolumeId() string
	GetReplicationSource() *replication.ReplicationSource
}

func groupName(volumeID string) string {
	return groupPrefix + volumeID
}

// volumeIDFromRequest returns the volume ID. Volume groups are not supported yet.
func volumeIDFromRequest(req replicationRequest) (string, error) {
	if src := req.GetReplicationSource(); src != nil {
		if src.GetVolumegroup() != nil {
			return "", status.Error(codes.Unimplemented, "volume group replication is not supported")
		}
		if id := src.GetVolume().GetVolumeId(); id != "" {
			return id, nil
		}
	}
	//nolint:staticcheck // VolumeId is deprecated but still set by older controllers.
	if id := req.GetVolumeId(); id != "" {
		return id, nil
	}
	return "", status.Error(codes.InvalidArgument, "volume ID is required")
}

// toStatus maps HMS errors to gRPC codes. FailedPrecondition makes the
// csi-addons controller retry a promote with force, which recovers with the
// disasterRecovery type, so it is used when the peer site cannot be reached.
func toStatus(log *zap.SugaredLogger, op string, err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, hms.ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, hms.ErrNoPeer), errors.Is(err, hms.ErrPeerUnreachable),
		errors.Is(err, hms.ErrInvalidState):
		code = codes.FailedPrecondition
	case errors.Is(err, hms.ErrNotSupported):
		code = codes.Unimplemented
	case errors.Is(err, hms.ErrSyncOngoing):
		code = codes.Aborted
	case errors.Is(err, hms.ErrNotAuthenticated):
		code = codes.Unauthenticated
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	}
	log.Errorf("%s failed: %v", op, err)
	return status.Errorf(code, "%s failed: %v", op, err)
}

// begin validates the request, takes the per-volume lock and creates a client.
// The caller must call the returned release function.
func (s *Server) begin(ctx context.Context, req replicationRequest,
	secrets map[string]string) (string, hms.Client, func(), error) {
	volumeID, err := volumeIDFromRequest(req)
	if err != nil {
		return "", nil, nil, err
	}
	if !s.locks.TryAcquire(volumeID) {
		return "", nil, nil, status.Errorf(codes.Aborted, node.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	release := func() { s.locks.Release(volumeID) }
	client, err := s.newClient(ctx, secrets)
	if err != nil {
		release()
		return "", nil, nil, status.Errorf(codes.Unavailable, "failed to create HMS client: %v", err)
	}
	return volumeID, client, release, nil
}

// groups returns the primary and the secondary group of the volume on the
// local site. Either may be nil.
func groups(ctx context.Context, client hms.Client, volumeID string) (primary, secondary *hms.Group, err error) {
	name := groupName(volumeID)
	primary, err = client.FindOutgoingGroup(ctx, name)
	if errors.Is(err, hms.ErrNotFound) {
		primary, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	secondary, err = client.FindIncomingGroup(ctx, name)
	if errors.Is(err, hms.ErrNotFound) {
		secondary, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return primary, secondary, nil
}

// replicationStopped reports whether a primary group stopped replicating
// because its secondary was recovered. notActive is not a stop: HMS derives it
// from the passive HBR state that CNS groups are in between sync cycles.
func replicationStopped(g *hms.Group) bool {
	return g.Status == hms.StatusRecovered
}

// EnableVolumeReplication configures replication of the volume to the target
// site. The csi-addons controller calls it before every promote, also on the
// target site after a failover, so an existing primary or secondary group is
// accepted and left to PromoteVolume.
func (s *Server) EnableVolumeReplication(ctx context.Context,
	req *replication.EnableVolumeReplicationRequest) (*replication.EnableVolumeReplicationResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("EnableVolumeReplication: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	params, err := parseClassParams(req.GetParameters(), false)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition, "invalid parameters: %v", err)
	}
	name := groupName(volumeID)
	primary, secondary, err := groups(ctx, client, volumeID)
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	if primary != nil {
		log.Infof("EnableVolumeReplication: group %q (%s) already exists with status %s",
			name, primary.Ref.Value, primary.Status)
		return &replication.EnableVolumeReplicationResponse{}, nil
	}
	if secondary != nil {
		log.Infof("EnableVolumeReplication: volume %s is a replica on this site (group %q, status %s)",
			volumeID, name, secondary.Status)
		return &replication.EnableVolumeReplicationResponse{}, nil
	}

	vol, err := s.volumes.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal, "failed to query volume %s: %v", volumeID, err)
	}
	if vol == nil {
		return nil, logger.LogNewErrorCodef(log, codes.NotFound, "volume %s not found", volumeID)
	}
	if vol.VolumeType != common.BlockVolumeType {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"replication is only supported for block volumes, volume %s is %s", volumeID, vol.VolumeType)
	}
	if params.targetSite == "" || params.targetDatastore == "" {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"parameters %q and %q are required", ParamTargetSite, ParamTargetDatastore)
	}
	dsMoID, err := s.volumes.DatastoreMoID(ctx, vol.DatastoreURL)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to find the datastore of volume %s: %v", volumeID, err)
	}
	rpo := params.rpo
	if rpo == 0 {
		rpo = defaultRPO
	}
	g, err := client.ConfigureCnsVolumeReplication(ctx, hms.CnsVolumeReplicationSpec{
		Name:                   name,
		TargetSite:             params.targetSite,
		VolumeID:               volumeID,
		SourceDatastore:        hms.MoRef{Type: "Datastore", Value: dsMoID},
		TargetDatastore:        hms.MoRef{Type: "Datastore", Value: params.targetDatastore},
		TargetStorageProfileID: params.targetStoragePolicy,
		RPO:                    rpo,
	})
	if err != nil {
		return nil, toStatus(log, "configure replication", err)
	}
	log.Infof("EnableVolumeReplication: configured group %q (%s) replicating from %s to %s on %q every %s",
		name, g.Ref.Value, dsMoID, params.targetDatastore, params.targetSite, g.RPO)
	return &replication.EnableVolumeReplicationResponse{}, nil
}

// DisableVolumeReplication removes the primary group and its secondary group.
// On the target site the replica is left for the source site to remove.
func (s *Server) DisableVolumeReplication(ctx context.Context,
	req *replication.DisableVolumeReplicationRequest) (*replication.DisableVolumeReplicationResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DisableVolumeReplication: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	if _, err := parseClassParams(req.GetParameters(), false); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.InvalidArgument, "invalid parameters: %v", err)
	}
	name := groupName(volumeID)
	primary, err := client.FindOutgoingGroup(ctx, name)
	if errors.Is(err, hms.ErrNotFound) {
		log.Infof("DisableVolumeReplication: no primary group %q on this site", name)
		return &replication.DisableVolumeReplicationResponse{}, nil
	}
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	if err := client.Unconfigure(ctx, primary, false); err != nil && !errors.Is(err, hms.ErrNotFound) {
		return nil, toStatus(log, "unconfigure replication", err)
	}
	s.forget(primary)
	log.Infof("DisableVolumeReplication: unconfigured group %q (%s)", name, primary.Ref.Value)
	return &replication.DisableVolumeReplicationResponse{}, nil
}

func (s *Server) forget(g *hms.Group) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.syncs, g.Ref.Value)
	delete(s.demoted, g.Ref.Value)
}

// PromoteVolume makes the volume primary on this site. On the source site
// that is a no-op. On the target site the secondary group is recovered from
// its latest replica and the replica disk is registered with CNS under the
// source volume ID. A forced promote uses the disasterRecovery type.
func (s *Server) PromoteVolume(ctx context.Context,
	req *replication.PromoteVolumeRequest) (*replication.PromoteVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("PromoteVolume: called with volume %q, force %t",
		req.GetReplicationSource().GetVolume().GetVolumeId(), req.GetForce())

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	params, err := parseClassParams(req.GetParameters(), false)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.InvalidArgument, "invalid parameters: %v", err)
	}
	name := groupName(volumeID)
	primary, secondary, err := groups(ctx, client, volumeID)
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	if secondary == nil {
		if primary == nil {
			return nil, logger.LogNewErrorCodef(log, codes.NotFound,
				"no replication group %q on this site", name)
		}
		if replicationStopped(primary) {
			return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
				"volume %s was failed over to the peer site; HMS does not support failback of CNS groups",
				volumeID)
		}
		// Already the source site. Cancel a pending demote.
		s.mu.Lock()
		delete(s.demoted, primary.Ref.Value)
		s.mu.Unlock()
		log.Infof("PromoteVolume: volume %s is already primary (group %q, status %s)",
			volumeID, name, primary.Status)
		return &replication.PromoteVolumeResponse{}, nil
	}

	if secondary.Status != hms.StatusRecovered {
		recoveryType := hms.RecoveryPlannedMigration
		if req.GetForce() {
			recoveryType = hms.RecoveryDisaster
		}
		folder, pool, err := s.volumes.RecoveryPlacement(ctx)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to find a folder and resource pool for recovery: %v", err)
		}
		log.Infof("PromoteVolume: recovering group %q (%s, status %s) with %s into folder %s, pool %s",
			name, secondary.Ref.Value, secondary.Status, recoveryType, folder, pool)
		err = client.RecoverGroup(ctx, secondary, hms.RecoverSpec{
			Folder: hms.MoRef{Type: "Folder", Value: folder},
			Pool:   hms.MoRef{Type: "ResourcePool", Value: pool},
			Type:   recoveryType,
		})
		if err != nil && !errors.Is(err, hms.ErrAlreadyRecovered) {
			return nil, toStatus(log, "recover group", err)
		}
	}
	if err := s.ensureVolume(ctx, client, volumeID, secondary, params); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"group %q is recovered but volume %s is not available: %v", name, volumeID, err)
	}
	log.Infof("PromoteVolume: volume %s is primary on this site", volumeID)
	return &replication.PromoteVolumeResponse{}, nil
}

// ensureVolume registers the recovered replica disk with CNS under the
// source volume ID, unless the volume already exists.
func (s *Server) ensureVolume(ctx context.Context, client hms.Client, volumeID string,
	group *hms.Group, params *classParams) error {
	log := logger.GetLogger(ctx)
	vol, err := s.volumes.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if vol != nil {
		return nil
	}
	// params.targetDatastore is the target of this site's outgoing
	// replication, not where the incoming replica is.
	replica := hms.ReplicaDiskPath(group)
	dsMoID := params.replicaDatastore
	if dsMoID == "" {
		ds, err := client.GroupDatastore(ctx, group)
		dsMoID = ds.Value
		if err != nil {
			// HMS does not implement currentSpec for secondary groups.
			log.Infof("HMS did not report the replica datastore of group %q (%v); searching the datastores for %s",
				group.Name, err, replica)
			if dsMoID, err = s.volumes.FindDatastoreWithFile(ctx, replica); err != nil {
				return fmt.Errorf("find the replica disk of group %q (set parameter %q): %w",
					group.Name, ParamReplicaDatastore, err)
			}
		}
	}
	diskURL, err := s.volumes.DiskURLPath(ctx, dsMoID, replica)
	if err != nil {
		return err
	}
	if s.skipRegistration {
		log.Warnf("Skipping registration of replica disk %q as volume %s", diskURL, volumeID)
		return nil
	}
	log.Infof("Registering replica disk %q as volume %s", diskURL, volumeID)
	return s.volumes.RegisterVolume(ctx, volumeID, diskURL, params.targetStoragePolicy)
}

// DemoteVolume prepares a planned migration on the source site: the volume
// must not be attached, and an on-demand sync ships the final data. The
// controller calls it on every reconcile of a secondary volume, so the sync
// runs once per demote. On the target site it is a no-op.
func (s *Server) DemoteVolume(ctx context.Context,
	req *replication.DemoteVolumeRequest) (*replication.DemoteVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DemoteVolume: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	if _, err := parseClassParams(req.GetParameters(), false); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.InvalidArgument, "invalid parameters: %v", err)
	}
	name := groupName(volumeID)
	primary, err := client.FindOutgoingGroup(ctx, name)
	if errors.Is(err, hms.ErrNotFound) {
		// A replica, or not replicated at all.
		return &replication.DemoteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	s.mu.Lock()
	done := s.demoted[primary.Ref.Value]
	s.mu.Unlock()
	if done || replicationStopped(primary) {
		return &replication.DemoteVolumeResponse{}, nil
	}
	attached, err := s.volumes.IsAttached(ctx, volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to check attachment of volume %s: %v", volumeID, err)
	}
	if attached {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"volume %s is attached; stop the workload before demoting", volumeID)
	}
	if err := client.OnlineSync(ctx, primary); err != nil {
		return nil, toStatus(log, "final sync", err)
	}
	s.mu.Lock()
	s.demoted[primary.Ref.Value] = true
	s.mu.Unlock()
	log.Infof("DemoteVolume: final sync of group %q (%s) completed", name, primary.Ref.Value)
	return &replication.DemoteVolumeResponse{}, nil
}

// ResyncVolume reports whether a secondary volume is in sync with its
// primary. HMS replicates continuously, so there is nothing to start.
// Re-protecting a volume after a failover is not supported by HMS.
func (s *Server) ResyncVolume(ctx context.Context,
	req *replication.ResyncVolumeRequest) (*replication.ResyncVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Debugf("ResyncVolume: called with volume %q, force %t",
		req.GetReplicationSource().GetVolume().GetVolumeId(), req.GetForce())

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	if _, err := parseClassParams(req.GetParameters(), false); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.InvalidArgument, "invalid parameters: %v", err)
	}
	name := groupName(volumeID)
	primary, secondary, err := groups(ctx, client, volumeID)
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	switch {
	case secondary != nil && secondary.Status == hms.StatusRecovered,
		primary != nil && replicationStopped(primary):
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"group %q was recovered; HMS does not support re-protecting CNS groups, "+
				"disable and enable replication from the primary site", name)
	case secondary != nil:
		// notActive is the idle state between sync cycles.
		ready := secondary.Status == hms.StatusOK || secondary.Status == hms.StatusSync ||
			secondary.Status == hms.StatusNotActive
		return &replication.ResyncVolumeResponse{Ready: ready}, nil
	case primary != nil:
		// Demoted source of a planned migration whose target is not promoted yet.
		return &replication.ResyncVolumeResponse{Ready: false}, nil
	default:
		return nil, logger.LogNewErrorCodef(log, codes.NotFound, "no replication group %q on this site", name)
	}
}

// GetVolumeReplicationInfo reports the last replicated point in time and the
// replication health of a primary volume. The csi-addons controller turns a
// gRPC error into an Unknown Replicating condition, so known problems, such
// as a missing group, are reported as an ERROR status with a message instead.
func (s *Server) GetVolumeReplicationInfo(ctx context.Context,
	req *replication.GetVolumeReplicationInfoRequest) (*replication.GetVolumeReplicationInfoResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	name := groupName(volumeID)
	primary, secondary, err := groups(ctx, client, volumeID)
	if err != nil {
		return nil, toStatus(log, "find replication group", err)
	}
	if primary == nil {
		msg := fmt.Sprintf("no primary group %q on site %q; the volume is not replicated", name, client.LocalSite())
		if secondary != nil && secondary.Status == hms.StatusRecovered {
			msg = fmt.Sprintf("volume was recovered from group %q and is not replicated; "+
				"HMS does not support re-protecting CNS groups", name)
		}
		log.Warnf("GetVolumeReplicationInfo: %s", msg)
		return &replication.GetVolumeReplicationInfoResponse{
			Status:        replication.GetVolumeReplicationInfoResponse_ERROR,
			StatusMessage: msg,
		}, nil
	}

	resp := &replication.GetVolumeReplicationInfoResponse{}
	var progress *hms.PolicyProgress
	if !replicationStopped(primary) {
		progress, err = client.GetPolicyProgress(ctx, primary)
		if err != nil {
			return nil, toStatus(log, "get policy progress", err)
		}
	}
	if progress != nil {
		if at, ok := s.observeSync(primary, progress); ok {
			resp.LastSyncTime = timestamppb.New(at)
		}
		if progress.LastSyncDuration != nil {
			resp.LastSyncDuration = durationpb.New(*progress.LastSyncDuration)
		}
		if progress.LastSyncBytes != nil {
			resp.LastSyncBytes = *progress.LastSyncBytes
		}
	}

	switch {
	case replicationStopped(primary):
		resp.Status = replication.GetVolumeReplicationInfoResponse_ERROR
		resp.StatusMessage = fmt.Sprintf("group %q stopped replicating because the peer site recovered it", name)
	case primary.Status == hms.StatusError:
		resp.Status = replication.GetVolumeReplicationInfoResponse_ERROR
		resp.StatusMessage = fmt.Sprintf("group %q is in error", name)
	case progress != nil && progress.Health == hms.HealthStalled:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("group %q is %s but replication is stalled", name, primary.Status)
	case progress == nil || progress.CurrentGeneration == nil || *progress.CurrentGeneration == 0:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("group %q is %s; the initial full sync has not completed",
			name, primary.Status)
	case !primary.PeerHealthy:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("group %q is %s but the peer site is not connected", name, primary.Status)
	case primary.Status == hms.StatusPaused || primary.Status == hms.StatusUnknown:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("group %q is %s", name, primary.Status)
	default:
		resp.Status = replication.GetVolumeReplicationInfoResponse_HEALTHY
		resp.StatusMessage = fmt.Sprintf("group %q is %s, generation %d, policy %s",
			name, primary.Status, *progress.CurrentGeneration, progress.Health)
	}
	return resp, nil
}

// observeSync returns the time the driver first saw the current generation.
// It is an upper bound of the time the last sync cycle completed.
func (s *Server) observeSync(g *hms.Group, p *hms.PolicyProgress) (time.Time, bool) {
	if p.CurrentGeneration == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obs, ok := s.syncs[g.Ref.Value]
	if !ok || obs.generation != *p.CurrentGeneration {
		obs = syncObservation{generation: *p.CurrentGeneration, at: s.now()}
		s.syncs[g.Ref.Value] = obs
	}
	return obs.at, true
}

// GetReplicationDestinationInfo returns the destination volume ID, which is
// the source ID because recovered volumes keep their ID.
func (s *Server) GetReplicationDestinationInfo(ctx context.Context,
	req *replication.GetReplicationDestinationInfoRequest) (*replication.GetReplicationDestinationInfoResponse, error) {
	src := req.GetReplicationSource()
	if src.GetVolumegroup() != nil {
		return nil, status.Error(codes.Unimplemented, "volume group replication is not supported")
	}
	volumeID := src.GetVolume().GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	return &replication.GetReplicationDestinationInfoResponse{
		ReplicationDestination: &replication.ReplicationDestination{
			Type: &replication.ReplicationDestination_Volume{
				Volume: &replication.ReplicationDestination_VolumeDestination{VolumeId: volumeID},
			},
		},
	}, nil
}
