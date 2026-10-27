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
// guest cluster (pvCSI) flavor. pvCSI has no access to vSphere Replication, so
// it proxies every request to a csi-addons VolumeReplication on the Supervisor
// PVC that backs the guest volume. The guest volume ID is the Supervisor PVC
// name, and the Supervisor VolumeReplication has the same name. The vSphere
// CSI driver on the Supervisor does the replication.
//
// Each RPC sets the desired state on the Supervisor VolumeReplication and
// waits a short time for the Supervisor to reach it. If it has not, the RPC
// fails with Aborted and the csi-addons controller in the guest retries.
package replication

import (
	"context"
	"fmt"
	"time"

	"github.com/csi-addons/spec/lib/go/replication"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/node"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

const (
	// ParamSupervisorClass is the VolumeReplicationClass parameter that names
	// the Supervisor VolumeReplicationClass.
	ParamSupervisorClass = "supervisorVolumeReplicationClass"

	// AnnGuestInitiated marks Supervisor VolumeReplications that pvCSI created.
	AnnGuestInitiated = "csi.vsphere.guest-initiated-volume-replication"

	defaultPollInterval = 2 * time.Second
	defaultWaitTimeout  = 30 * time.Second
)

// Clients returns the clients that a request uses. pvCSI re-creates its
// clients when its configuration changes, so they are looked up per request.
type Clients func(ctx context.Context) (supervisorDynamic dynamic.Interface,
	supervisor clientset.Interface, guest clientset.Interface, err error)

// Config configures a Server.
type Config struct {
	Clients Clients
	// SupervisorNamespace is the guest cluster's Supervisor namespace.
	SupervisorNamespace string
	// Labels are set on the Supervisor VolumeReplications that pvCSI creates.
	Labels map[string]string
	// PollInterval and WaitTimeout control how long an RPC waits for the
	// Supervisor. The defaults are 2s and 30s.
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

// Server implements the csi-addons Replication controller service.
type Server struct {
	replication.UnimplementedControllerServer
	cfg   Config
	locks *node.VolumeLocks
}

var _ replication.ControllerServer = &Server{}

// NewServer returns a Replication server.
func NewServer(cfg Config) *Server {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.WaitTimeout == 0 {
		cfg.WaitTimeout = defaultWaitTimeout
	}
	return &Server{cfg: cfg, locks: node.NewVolumeLocks()}
}

// replicationRequest is implemented by every csi-addons replication request.
type replicationRequest interface {
	GetVolumeId() string
	GetReplicationSource() *replication.ReplicationSource
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

// request is the state of one RPC.
type request struct {
	volumeID   string
	vrs        *supervisorVRs
	supervisor clientset.Interface
	guest      clientset.Interface
	log        *zap.SugaredLogger
}

// begin validates the request, takes the per-volume lock and gets the
// clients. The caller must call the returned release function.
func (s *Server) begin(ctx context.Context, req replicationRequest) (*request, func(), error) {
	log := logger.GetLogger(ctx)
	volumeID, err := volumeIDFromRequest(req)
	if err != nil {
		return nil, nil, err
	}
	if !s.locks.TryAcquire(volumeID) {
		return nil, nil, status.Errorf(codes.Aborted, node.VolumeOperationAlreadyExistsFmt, volumeID)
	}
	release := func() { s.locks.Release(volumeID) }
	dyn, supervisor, guest, err := s.cfg.Clients(ctx)
	if err != nil {
		release()
		return nil, nil, status.Errorf(codes.Unavailable, "failed to create Supervisor clients: %v", err)
	}
	return &request{
		volumeID:   volumeID,
		vrs:        &supervisorVRs{client: dyn, namespace: s.cfg.SupervisorNamespace},
		supervisor: supervisor,
		guest:      guest,
		log:        log,
	}, release, nil
}

func supervisorClass(params map[string]string) (string, error) {
	class := params[ParamSupervisorClass]
	if class == "" {
		return "", status.Errorf(codes.InvalidArgument, "parameter %q is required", ParamSupervisorClass)
	}
	return class, nil
}

// checkSupervisorPVC verifies that the Supervisor PVC backing the volume
// exists and is bound.
func (s *Server) checkSupervisorPVC(ctx context.Context, r *request) error {
	pvc, err := r.supervisor.CoreV1().PersistentVolumeClaims(s.cfg.SupervisorNamespace).Get(
		ctx, r.volumeID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return logger.LogNewErrorCodef(r.log, codes.NotFound,
			"Supervisor PVC %s/%s not found", s.cfg.SupervisorNamespace, r.volumeID)
	}
	if err != nil {
		return logger.LogNewErrorCodef(r.log, codes.Unavailable,
			"failed to get Supervisor PVC %s/%s: %v", s.cfg.SupervisorNamespace, r.volumeID, err)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return logger.LogNewErrorCodef(r.log, codes.FailedPrecondition,
			"Supervisor PVC %s/%s is %s, not Bound", s.cfg.SupervisorNamespace, r.volumeID, pvc.Status.Phase)
	}
	return nil
}

// setState sets the state of the Supervisor VolumeReplication and waits until
// the Supervisor reports it.
func (s *Server) setState(ctx context.Context, r *request, class, state, want string) error {
	spec := vrSpec{
		Name:        r.volumeID,
		PVCName:     r.volumeID,
		Class:       class,
		State:       state,
		Labels:      s.cfg.Labels,
		Annotations: map[string]string{AnnGuestInitiated: "true"},
	}
	vr, err := r.vrs.apply(ctx, spec)
	if err != nil {
		return logger.LogNewErrorCodef(r.log, codes.Unavailable,
			"failed to set Supervisor VolumeReplication %s/%s to %s: %v",
			s.cfg.SupervisorNamespace, r.volumeID, state, err)
	}
	if vr.Metadata.DeletionTimestamp != nil {
		return logger.LogNewErrorCodef(r.log, codes.Aborted,
			"Supervisor VolumeReplication %s/%s is being deleted", s.cfg.SupervisorNamespace, r.volumeID)
	}
	if vr.Spec.VolumeReplicationClass != class {
		r.log.Warnf("Supervisor VolumeReplication %s/%s uses class %q, not %q",
			s.cfg.SupervisorNamespace, r.volumeID, vr.Spec.VolumeReplicationClass, class)
	}
	vr, err = s.waitFor(ctx, r, func(vr *volumeReplication) bool {
		if vr.observed() && vr.Status.State == want {
			return true
		}
		_, failed := vr.failure(want)
		return failed
	})
	if err != nil {
		return err
	}
	if msg, failed := vr.failure(want); failed {
		// Not FailedPrecondition: on Promote the guest controller would retry
		// with force, and the Supervisor controller already does that itself.
		return logger.LogNewErrorCodef(r.log, codes.Internal,
			"Supervisor VolumeReplication %s/%s failed to become %s: %s",
			s.cfg.SupervisorNamespace, r.volumeID, want, msg)
	}
	return nil
}

// waitFor polls the Supervisor VolumeReplication until done returns true, and
// returns it. It fails with Aborted on timeout, so that the csi-addons
// controller retries, and with NotFound if the VolumeReplication is deleted.
func (s *Server) waitFor(ctx context.Context, r *request,
	done func(*volumeReplication) bool) (*volumeReplication, error) {
	var last *volumeReplication
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, s.cfg.PollInterval, s.cfg.WaitTimeout, true,
		func(ctx context.Context) (bool, error) {
			vr, err := r.vrs.get(ctx, r.volumeID)
			if err != nil {
				// Transient API errors are retried until the timeout.
				lastErr = err
				return false, nil
			}
			if vr == nil {
				return false, status.Errorf(codes.NotFound, "Supervisor VolumeReplication %s/%s was deleted",
					s.cfg.SupervisorNamespace, r.volumeID)
			}
			last = vr
			return done(vr), nil
		})
	if err == nil {
		return last, nil
	}
	if status.Code(err) == codes.NotFound {
		return nil, logger.LogNewErrorCode(r.log, codes.NotFound, err.Error())
	}
	msg := fmt.Sprintf("timed out waiting for Supervisor VolumeReplication %s/%s", s.cfg.SupervisorNamespace,
		r.volumeID)
	if last != nil {
		msg = fmt.Sprintf("%s; %s", msg, last)
	}
	if lastErr != nil {
		msg = fmt.Sprintf("%s; last error: %v", msg, lastErr)
	}
	return nil, logger.LogNewErrorCode(r.log, codes.Aborted, msg)
}

// EnableVolumeReplication validates the request. The csi-addons controller
// calls it only before PromoteVolume, which creates the Supervisor
// VolumeReplication. Creating a primary here could fail over a replica, so
// nothing is created.
func (s *Server) EnableVolumeReplication(ctx context.Context,
	req *replication.EnableVolumeReplicationRequest) (*replication.EnableVolumeReplicationResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("EnableVolumeReplication: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := supervisorClass(req.GetParameters()); err != nil {
		return nil, err
	}
	if err := s.checkSupervisorPVC(ctx, r); err != nil {
		return nil, err
	}
	return &replication.EnableVolumeReplicationResponse{}, nil
}

// DisableVolumeReplication deletes the Supervisor VolumeReplication, which
// disables replication on the Supervisor, and waits until it is gone.
func (s *Server) DisableVolumeReplication(ctx context.Context,
	req *replication.DisableVolumeReplicationRequest) (*replication.DisableVolumeReplicationResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DisableVolumeReplication: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()

	if _, err := r.vrs.delete(ctx, r.volumeID); err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to delete Supervisor VolumeReplication %s/%s: %v", s.cfg.SupervisorNamespace, r.volumeID, err)
	}
	// The Supervisor's finalizer disables replication before the object goes.
	err = wait.PollUntilContextTimeout(ctx, s.cfg.PollInterval, s.cfg.WaitTimeout, true,
		func(ctx context.Context) (bool, error) {
			vr, err := r.vrs.get(ctx, r.volumeID)
			return err == nil && vr == nil, nil
		})
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Aborted,
			"waiting for the Supervisor to disable replication and delete VolumeReplication %s/%s",
			s.cfg.SupervisorNamespace, r.volumeID)
	}
	log.Infof("DisableVolumeReplication: Supervisor VolumeReplication %s/%s is deleted",
		s.cfg.SupervisorNamespace, r.volumeID)
	return &replication.DisableVolumeReplicationResponse{}, nil
}

// PromoteVolume makes the Supervisor VolumeReplication primary. On the
// source site that enables replication. On the target site the Supervisor
// recovers the replica and registers it under the same volume ID, and the
// Supervisor csi-addons controller retries with force on its own.
func (s *Server) PromoteVolume(ctx context.Context,
	req *replication.PromoteVolumeRequest) (*replication.PromoteVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("PromoteVolume: called with volume %q, force %t",
		req.GetReplicationSource().GetVolume().GetVolumeId(), req.GetForce())

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	class, err := supervisorClass(req.GetParameters())
	if err != nil {
		return nil, err
	}
	if err := s.checkSupervisorPVC(ctx, r); err != nil {
		return nil, err
	}
	if err := s.setState(ctx, r, class, statePrimary, statusPrimary); err != nil {
		return nil, err
	}
	log.Infof("PromoteVolume: volume %s is primary on the Supervisor", r.volumeID)
	return &replication.PromoteVolumeResponse{}, nil
}

// DemoteVolume makes the Supervisor VolumeReplication secondary. On the
// source site the Supervisor ships a final sync. The Supervisor only sees its
// own VolumeAttachments, not attachments to guest nodes, so the volume must
// be detached in the guest first. The csi-addons controller calls Demote on
// every reconcile of a secondary, so a demoted volume returns at once.
func (s *Server) DemoteVolume(ctx context.Context,
	req *replication.DemoteVolumeRequest) (*replication.DemoteVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DemoteVolume: called with volume %q", req.GetReplicationSource().GetVolume().GetVolumeId())

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	class, err := supervisorClass(req.GetParameters())
	if err != nil {
		return nil, err
	}
	vr, err := r.vrs.get(ctx, r.volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to get Supervisor VolumeReplication %s/%s: %v", s.cfg.SupervisorNamespace, r.volumeID, err)
	}
	if vr != nil && vr.Spec.ReplicationState == stateSecondary && vr.observed() &&
		vr.Status.State == statusSecondary {
		return &replication.DemoteVolumeResponse{}, nil
	}
	if err := s.checkSupervisorPVC(ctx, r); err != nil {
		return nil, err
	}
	pv, attached, err := guestAttachment(ctx, r.guest, r.volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to check guest attachments of volume %s: %v", r.volumeID, err)
	}
	if attached != "" {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"volume %s (PV %s) is attached to guest node %s; stop the workload before demoting",
			r.volumeID, pv, attached)
	}
	if err := s.setState(ctx, r, class, stateSecondary, statusSecondary); err != nil {
		return nil, err
	}
	log.Infof("DemoteVolume: volume %s is secondary on the Supervisor", r.volumeID)
	return &replication.DemoteVolumeResponse{}, nil
}

// ResyncVolume reports whether the Supervisor considers the secondary volume
// in sync. The Supervisor csi-addons controller resyncs its secondary
// VolumeReplication on its own, so there is nothing to start. On the old
// primary site after a failover, that resync re-protects the volume: it
// overwrites the volume with the data of the peer site. The Supervisor only
// sees its own VolumeAttachments, so the guest attachment is checked here.
func (s *Server) ResyncVolume(ctx context.Context,
	req *replication.ResyncVolumeRequest) (*replication.ResyncVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Debugf("ResyncVolume: called with volume %q, force %t",
		req.GetReplicationSource().GetVolume().GetVolumeId(), req.GetForce())

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	vr, err := r.vrs.get(ctx, r.volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to get Supervisor VolumeReplication %s/%s: %v", s.cfg.SupervisorNamespace, r.volumeID, err)
	}
	if vr == nil {
		return nil, logger.LogNewErrorCodef(log, codes.NotFound,
			"Supervisor VolumeReplication %s/%s not found", s.cfg.SupervisorNamespace, r.volumeID)
	}
	pv, attached, err := guestAttachment(ctx, r.guest, r.volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to check guest attachments of volume %s: %v", r.volumeID, err)
	}
	if attached != "" {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"volume %s (PV %s) is attached to guest node %s; stop the workload, a resync can overwrite "+
				"the volume with the data of the peer site", r.volumeID, pv, attached)
	}
	if vr.Spec.ReplicationState != stateSecondary || !vr.observed() || vr.Status.State != statusSecondary {
		log.Debugf("ResyncVolume: waiting for %s", vr)
		return &replication.ResyncVolumeResponse{Ready: false}, nil
	}
	if c := vr.condition(condResyncing); c != nil && c.Reason == reasonFailedToResync {
		return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
			"Supervisor VolumeReplication %s/%s failed to resync: %s", s.cfg.SupervisorNamespace, r.volumeID,
			vr.errorDetail(c.Message))
	}
	c := vr.condition(condDegraded)
	ready := c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonHealthy
	return &replication.ResyncVolumeResponse{Ready: ready}, nil
}

// GetVolumeReplicationInfo copies the last sync and the replication health
// from the Supervisor VolumeReplication.
func (s *Server) GetVolumeReplicationInfo(ctx context.Context,
	req *replication.GetVolumeReplicationInfoRequest) (*replication.GetVolumeReplicationInfoResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)

	r, release, err := s.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	defer release()
	vr, err := r.vrs.get(ctx, r.volumeID)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Unavailable,
			"failed to get Supervisor VolumeReplication %s/%s: %v", s.cfg.SupervisorNamespace, r.volumeID, err)
	}
	if vr == nil {
		msg := fmt.Sprintf("Supervisor VolumeReplication %s/%s not found", s.cfg.SupervisorNamespace, r.volumeID)
		log.Warnf("GetVolumeReplicationInfo: %s", msg)
		return &replication.GetVolumeReplicationInfoResponse{
			Status:        replication.GetVolumeReplicationInfoResponse_ERROR,
			StatusMessage: msg,
		}, nil
	}

	resp := &replication.GetVolumeReplicationInfoResponse{}
	if vr.Status.LastSyncTime != nil {
		resp.LastSyncTime = timestamppb.New(vr.Status.LastSyncTime.Time)
	}
	if d := vr.lastSyncDuration(); d != nil {
		resp.LastSyncDuration = durationpb.New(*d)
	}
	if vr.Status.LastSyncBytes != nil {
		resp.LastSyncBytes = *vr.Status.LastSyncBytes
	}
	c := vr.condition(condReplicating)
	switch {
	case vr.Spec.ReplicationState != statePrimary || vr.Status.State != statusPrimary:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = fmt.Sprintf("Supervisor VolumeReplication is not primary: %s", vr)
	case c == nil:
		resp.Status = replication.GetVolumeReplicationInfoResponse_UNKNOWN
		resp.StatusMessage = "the Supervisor has not reported the replication status"
	case c.Status == metav1.ConditionTrue:
		resp.Status = replication.GetVolumeReplicationInfoResponse_HEALTHY
		resp.StatusMessage = c.Message
	case c.Status == metav1.ConditionFalse:
		resp.Status = replication.GetVolumeReplicationInfoResponse_DEGRADED
		resp.StatusMessage = c.Message
	default:
		resp.Status = replication.GetVolumeReplicationInfoResponse_UNKNOWN
		resp.StatusMessage = c.Message
	}
	return resp, nil
}

// GetReplicationDestinationInfo returns the destination volume ID. It is the
// source ID, because the target site keeps the Supervisor PVC name.
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

// guestAttachment returns the guest PV of the volume and the guest node it is
// attached to, if any.
func guestAttachment(ctx context.Context, guest clientset.Interface, volumeID string) (string, string, error) {
	pvs, err := guest.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}
	pvName := ""
	for _, pv := range pvs.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name && pv.Spec.CSI.VolumeHandle == volumeID {
			pvName = pv.Name
			break
		}
	}
	if pvName == "" {
		return "", "", nil
	}
	vas, err := guest.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return pvName, "", err
	}
	for _, va := range vas.Items {
		if va.Spec.Attacher != csitypes.Name || va.Spec.Source.PersistentVolumeName == nil ||
			*va.Spec.Source.PersistentVolumeName != pvName {
			continue
		}
		// A VolumeAttachment that is not attached yet may still attach.
		if va.Status.Attached || va.DeletionTimestamp == nil {
			return pvName, va.Spec.NodeName, nil
		}
	}
	return pvName, "", nil
}
