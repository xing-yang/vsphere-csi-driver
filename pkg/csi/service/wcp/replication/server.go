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
// Supervisor (WCP) flavor, backed by SnapService protection groups. Each
// replicated volume has its own protection group named "cns-<volumeID>", and
// the volume keeps the same ID on both sites.
package replication

import (
	"context"
	"errors"
	"fmt"

	"github.com/csi-addons/spec/lib/go/replication"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/node"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/snapservice"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

const protectionGroupPrefix = "cns-"

// ClientFactory returns a SnapService client for the local site. secrets are
// the contents of the VolumeReplicationClass secret, if one is configured.
type ClientFactory func(ctx context.Context, secrets map[string]string) (snapservice.Client, error)

// Server implements the csi-addons Replication controller service.
type Server struct {
	replication.UnimplementedControllerServer
	newClient        ClientFactory
	volumes          VolumeBackend
	locks            *node.VolumeLocks
	skipRegistration bool
}

var _ replication.ControllerServer = &Server{}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithSkipVolumeRegistration makes PromoteVolume skip registering recovered
// disks with CNS. Only for testing with a fake SnapService, whose recovered
// disks do not exist.
func WithSkipVolumeRegistration(skip bool) ServerOption {
	return func(s *Server) { s.skipRegistration = skip }
}

// NewServer returns a Replication server.
func NewServer(newClient ClientFactory, volumes VolumeBackend, opts ...ServerOption) *Server {
	s := &Server{
		newClient: newClient,
		volumes:   volumes,
		locks:     node.NewVolumeLocks(),
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

func protectionGroupName(volumeID string) string {
	return protectionGroupPrefix + volumeID
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

// toStatus maps SnapService errors to gRPC codes. FailedPrecondition makes
// the csi-addons controller retry a promote with force, so it is only used
// where forcing is safe or where the controller treats it as a validation
// failure.
func toStatus(log *zap.SugaredLogger, op string, err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, snapservice.ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, snapservice.ErrPeerUnreachable), errors.Is(err, snapservice.ErrNoSitePair),
		errors.Is(err, snapservice.ErrInvalidState):
		code = codes.FailedPrecondition
	case errors.Is(err, snapservice.ErrPeerActive):
		// Not FailedPrecondition: a forced retry would not help and the peer
		// must be demoted first.
		code = codes.Aborted
	case errors.Is(err, snapservice.ErrSiteUnavailable):
		code = codes.Unavailable
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	}
	log.Errorf("%s failed: %v", op, err)
	return status.Errorf(code, "%s failed: %v", op, err)
}

// begin validates the request, takes the per-volume lock and creates a client.
// The caller must call the returned release function.
func (s *Server) begin(ctx context.Context, req replicationRequest,
	secrets map[string]string) (string, snapservice.Client, func(), error) {
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
		return "", nil, nil, status.Errorf(codes.Unavailable, "failed to create SnapService client: %v", err)
	}
	return volumeID, client, release, nil
}

// EnableVolumeReplication creates the protection group for the volume. The
// csi-addons controller calls it before every promote, so an existing
// protection group in any state is accepted and left to PromoteVolume.
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
	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if err == nil {
		log.Infof("EnableVolumeReplication: protection group %q already exists in state %s", name, pg.State)
		return &replication.EnableVolumeReplicationResponse{}, nil
	}
	if !errors.Is(err, snapservice.ErrNotFound) {
		return nil, toStatus(log, "get protection group", err)
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
	if params.targetSite == "" {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"parameter %q is required", ParamTargetSite)
	}
	paired, err := client.IsSitePaired(ctx, params.targetSite)
	if err != nil {
		return nil, toStatus(log, "check site pair", err)
	}
	if !paired {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"site %q is not paired with site %q", client.LocalSite(), params.targetSite)
	}

	pg, err = client.CreateProtectionGroup(ctx, snapservice.ProtectionGroupSpec{
		Name:           name,
		TargetEntities: snapservice.TargetEntities{FCDs: []string{volumeID}},
		Replication: &snapservice.ReplicationPolicy{
			TargetSite:          params.targetSite,
			TargetDatastore:     params.targetDatastore,
			TargetStoragePolicy: params.targetStoragePolicy,
			RPO:                 params.rpo,
		},
	})
	if err != nil {
		return nil, toStatus(log, "create protection group", err)
	}
	log.Infof("EnableVolumeReplication: created protection group %q (%s) replicating to %q",
		name, pg.ID, params.targetSite)
	return &replication.EnableVolumeReplicationResponse{}, nil
}

// DisableVolumeReplication deletes the protection group on the primary site.
// On a secondary site the replica is left for the source site to remove.
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
	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if errors.Is(err, snapservice.ErrNotFound) {
		log.Infof("DisableVolumeReplication: protection group %q not found", name)
		return &replication.DisableVolumeReplicationResponse{}, nil
	}
	if err != nil {
		return nil, toStatus(log, "get protection group", err)
	}
	if pg.State != snapservice.StateActive && pg.State != snapservice.StatePaused {
		log.Infof("DisableVolumeReplication: protection group %q is %s, leaving it to the source site",
			name, pg.State)
		return &replication.DisableVolumeReplicationResponse{}, nil
	}
	if err := client.DeleteProtectionGroup(ctx, pg.ID); err != nil &&
		!errors.Is(err, snapservice.ErrNotFound) {
		return nil, toStatus(log, "delete protection group", err)
	}
	log.Infof("DisableVolumeReplication: deleted protection group %q", name)
	return &replication.DisableVolumeReplicationResponse{}, nil
}

// PromoteVolume makes the local protection group active and makes sure the
// CNS volume exists on this site with its original ID.
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

	if _, err := parseClassParams(req.GetParameters(), false); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.InvalidArgument, "invalid parameters: %v", err)
	}
	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if err != nil {
		return nil, toStatus(log, "get protection group", err)
	}
	res, err := client.Promote(ctx, pg.ID, req.GetForce())
	if err != nil {
		return nil, toStatus(log, "promote protection group", err)
	}
	if err := s.ensureVolume(ctx, volumeID, res); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"protection group %q promoted but volume %s is not available: %v", name, volumeID, err)
	}
	log.Infof("PromoteVolume: protection group %q is active", name)
	return &replication.PromoteVolumeResponse{}, nil
}

// ensureVolume registers the recovered disk with CNS under the source volume
// ID if SnapService did not already.
func (s *Server) ensureVolume(ctx context.Context, volumeID string, res *snapservice.PromoteResult) error {
	log := logger.GetLogger(ctx)
	vol, err := s.volumes.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}
	if vol != nil {
		return nil
	}
	for _, d := range res.RecoveredDisks {
		if d.SourceID != volumeID {
			continue
		}
		if d.RecoveredID != "" && d.RecoveredID != volumeID {
			return errors.New("SnapService registered the recovered disk as " + d.RecoveredID)
		}
		if d.DiskURLPath == "" {
			return errors.New("SnapService did not report the recovered disk path")
		}
		if s.skipRegistration {
			log.Warnf("Skipping registration of recovered disk %q as volume %s", d.DiskURLPath, volumeID)
			return nil
		}
		log.Infof("Registering recovered disk %q as volume %s", d.DiskURLPath, volumeID)
		// TODO: Use the storage policy of the PV's StorageClass.
		return s.volumes.RegisterVolume(ctx, volumeID, d.DiskURLPath, "")
	}
	return errors.New("SnapService did not report a recovered disk for the volume")
}

// DemoteVolume demotes the local protection group. The volume must not be
// attached, so that the final delta shipped by the demote is consistent.
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
	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if err != nil {
		return nil, toStatus(log, "get protection group", err)
	}
	if pg.State == snapservice.StateDemoted || pg.State == snapservice.StateDormant {
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
	if err := client.Demote(ctx, pg.ID); err != nil {
		return nil, toStatus(log, "demote protection group", err)
	}
	log.Infof("DemoteVolume: protection group %q demoted", name)
	return &replication.DemoteVolumeResponse{}, nil
}

// ResyncVolume re-protects a demoted protection group as a replica of the
// active peer and reports ready once the replica is in sync.
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
	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if err != nil {
		return nil, toStatus(log, "get protection group", err)
	}
	switch pg.State {
	case snapservice.StateDormant:
	case snapservice.StateDemoted:
		// Re-protecting discards local changes since the demote, which is what
		// both a regular and a forced resync ask for.
		err := client.Activate(ctx, pg.ID)
		if errors.Is(err, snapservice.ErrInvalidState) || errors.Is(err, snapservice.ErrPeerUnreachable) {
			// The peer is not active yet, for example between demote and
			// promote of a planned migration.
			log.Infof("ResyncVolume: protection group %q cannot be re-protected yet: %v", name, err)
			return &replication.ResyncVolumeResponse{Ready: false}, nil
		}
		if err != nil {
			return nil, toStatus(log, "activate protection group", err)
		}
		log.Infof("ResyncVolume: protection group %q re-protected", name)
		if pg, err = client.GetProtectionGroup(ctx, name); err != nil {
			return nil, toStatus(log, "get protection group", err)
		}
	default:
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"protection group %q is %s; demote the volume before resyncing", name, pg.State)
	}
	ready := pg.State == snapservice.StateDormant && pg.InitialSyncDone && pg.ReplicationHealthy
	return &replication.ResyncVolumeResponse{Ready: ready}, nil
}

// GetVolumeReplicationInfo reports the last replicated point in time and the
// replication health. The csi-addons controller only calls it for primary
// volumes and turns a gRPC error into an Unknown Replicating condition, so
// problems that are known, such as a missing or inactive protection group, are
// reported as an ERROR status with a message instead.
func (s *Server) GetVolumeReplicationInfo(ctx context.Context,
	req *replication.GetVolumeReplicationInfoRequest) (*replication.GetVolumeReplicationInfoResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)

	volumeID, client, release, err := s.begin(ctx, req, req.GetSecrets())
	if err != nil {
		return nil, err
	}
	defer release()

	name := protectionGroupName(volumeID)
	pg, err := client.GetProtectionGroup(ctx, name)
	if errors.Is(err, snapservice.ErrNotFound) {
		log.Warnf("GetVolumeReplicationInfo: protection group %q not found for primary volume %s", name, volumeID)
		return &replication.GetVolumeReplicationInfoResponse{
			Status: replication.GetVolumeReplicationInfoResponse_ERROR,
			StatusMessage: fmt.Sprintf("protection group %q does not exist on site %q; "+
				"the volume is not replicated", name, client.LocalSite()),
		}, nil
	}
	if err != nil {
		return nil, toStatus(log, "get protection group", err)
	}
	resp := &replication.GetVolumeReplicationInfoResponse{}
	if pg.LastReplicaTime != nil {
		resp.LastSyncTime = timestamppb.New(*pg.LastReplicaTime)
	}
	switch {
	case pg.State != snapservice.StateActive && pg.State != snapservice.StatePaused:
		resp.Status = replication.GetVolumeReplicationInfoResponse_ERROR
		resp.StatusMessage = fmt.Sprintf("protection group %q is %s, expected %s for a primary volume",
			name, pg.State, snapservice.StateActive)
	case !pg.ReplicationHealthy:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("protection group %q is %s but replication to the peer site is not healthy",
			name, pg.State)
	default:
		resp.Status = replication.GetVolumeReplicationInfoResponse_HEALTHY
		resp.StatusMessage = fmt.Sprintf("protection group %q is %s and replicating", name, pg.State)
	}
	return resp, nil
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
