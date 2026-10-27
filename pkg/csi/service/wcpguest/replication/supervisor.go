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

package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// The csi-addons VolumeReplication API. The types are read and written as
// unstructured objects so that the driver does not depend on the
// kubernetes-csi-addons module.
const (
	vrGroup   = "replication.storage.openshift.io"
	vrVersion = "v1alpha1"
	vrKind    = "VolumeReplication"

	// Values of spec.replicationState.
	statePrimary   = "primary"
	stateSecondary = "secondary"

	// Values of status.state.
	statusPrimary   = "Primary"
	statusSecondary = "Secondary"

	condCompleted   = "Completed"
	condDegraded    = "Degraded"
	condResyncing   = "Resyncing"
	condReplicating = "Replicating"

	reasonFailedToResync = "FailedToResync"
	reasonHealthy        = "Healthy"
	reasonError          = "Error"
)

var vrResource = schema.GroupVersionResource{Group: vrGroup, Version: vrVersion, Resource: "volumereplications"}

// volumeReplication is the part of a csi-addons VolumeReplication that the
// proxy reads.
type volumeReplication struct {
	Metadata struct {
		Name              string            `json:"name"`
		Generation        int64             `json:"generation"`
		DeletionTimestamp *metav1.Time      `json:"deletionTimestamp,omitempty"`
		Labels            map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec struct {
		VolumeReplicationClass string `json:"volumeReplicationClass"`
		ReplicationState       string `json:"replicationState"`
		DataSource             struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"dataSource"`
	} `json:"spec"`
	Status struct {
		State              string             `json:"state,omitempty"`
		Message            string             `json:"message,omitempty"`
		Conditions         []metav1.Condition `json:"conditions,omitempty"`
		ObservedGeneration int64              `json:"observedGeneration,omitempty"`
		LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
		LastSyncBytes      *int64             `json:"lastSyncBytes,omitempty"`
		LastSyncDuration   *metav1.Duration   `json:"lastSyncDuration,omitempty"`
	} `json:"status"`
}

// observed reports whether the csi-addons controller on the Supervisor has
// reconciled the current spec. Until then the status describes an older spec.
func (vr *volumeReplication) observed() bool {
	return vr.Status.ObservedGeneration == vr.Metadata.Generation
}

// condition returns the condition of the given type if it was set for the
// current spec.
func (vr *volumeReplication) condition(condType string) *metav1.Condition {
	for i := range vr.Status.Conditions {
		c := &vr.Status.Conditions[i]
		if c.Type == condType && c.ObservedGeneration == vr.Metadata.Generation {
			return c
		}
	}
	return nil
}

// failure returns the Supervisor's error message if it failed to reach the
// requested state for the current spec. status.message alone is not a
// failure: the csi-addons controller also updates the status, with the old
// state and a message such as "destination info pending retrieval", while a
// reconcile is still in progress.
func (vr *volumeReplication) failure(want string) (string, bool) {
	if !vr.observed() || vr.Status.State == want {
		return "", false
	}
	if c := vr.condition(condCompleted); c != nil && c.Status == metav1.ConditionFalse {
		return vr.errorDetail(c.Message), true
	}
	return "", false
}

// errorDetail returns the driver's error from the Degraded condition, which
// the csi-addons controller sets on a failed RPC. The Completed and Resyncing
// conditions only have generic messages such as "failed to demote volume".
func (vr *volumeReplication) errorDetail(fallback string) string {
	if c := vr.condition(condDegraded); c != nil && c.Status == metav1.ConditionTrue && c.Reason == reasonError {
		return c.Message
	}
	return fallback
}

func (vr *volumeReplication) String() string {
	return fmt.Sprintf("VolumeReplication %s (spec %s, generation %d, observed %d, state %q, message %q)",
		vr.Metadata.Name, vr.Spec.ReplicationState, vr.Metadata.Generation, vr.Status.ObservedGeneration,
		vr.Status.State, vr.Status.Message)
}

// vrSpec is what pvCSI sets on a Supervisor VolumeReplication.
type vrSpec struct {
	Name        string
	PVCName     string
	Class       string
	State       string
	Labels      map[string]string
	Annotations map[string]string
}

// supervisorVRs reads and writes VolumeReplications in the guest cluster's
// Supervisor namespace.
type supervisorVRs struct {
	client    dynamic.Interface
	namespace string
}

func (s *supervisorVRs) resource() dynamic.ResourceInterface {
	return s.client.Resource(vrResource).Namespace(s.namespace)
}

// get returns the VolumeReplication, or nil if it does not exist.
func (s *supervisorVRs) get(ctx context.Context, name string) (*volumeReplication, error) {
	obj, err := s.resource().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromUnstructured(obj)
}

// apply creates the VolumeReplication, or sets its replication state if it
// exists. Other fields of an existing VolumeReplication are left unchanged.
func (s *supervisorVRs) apply(ctx context.Context, spec vrSpec) (*volumeReplication, error) {
	vr, err := s.get(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	if vr == nil {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": vrGroup + "/" + vrVersion,
			"kind":       vrKind,
			"metadata": map[string]interface{}{
				"name":      spec.Name,
				"namespace": s.namespace,
			},
			"spec": map[string]interface{}{
				"volumeReplicationClass": spec.Class,
				"replicationState":       spec.State,
				"dataSource": map[string]interface{}{
					"apiGroup": "",
					"kind":     "PersistentVolumeClaim",
					"name":     spec.PVCName,
				},
			},
		}}
		obj.SetLabels(spec.Labels)
		obj.SetAnnotations(spec.Annotations)
		created, err := s.resource().Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return fromUnstructured(created)
	}
	if vr.Spec.ReplicationState == spec.State {
		return vr, nil
	}
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"replicationState": spec.State},
	})
	if err != nil {
		return nil, err
	}
	patched, err := s.resource().Patch(ctx, spec.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(patched)
}

// delete deletes the VolumeReplication. It returns false if it was already gone.
func (s *supervisorVRs) delete(ctx context.Context, name string) (bool, error) {
	err := s.resource().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func fromUnstructured(obj *unstructured.Unstructured) (*volumeReplication, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to encode VolumeReplication %s: %w", obj.GetName(), err)
	}
	vr := &volumeReplication{}
	if err := json.Unmarshal(data, vr); err != nil {
		return nil, fmt.Errorf("failed to parse VolumeReplication %s: %w", obj.GetName(), err)
	}
	return vr, nil
}

// lastSyncDuration returns status.lastSyncDuration, if set.
func (vr *volumeReplication) lastSyncDuration() *time.Duration {
	if vr.Status.LastSyncDuration == nil {
		return nil
	}
	d := vr.Status.LastSyncDuration.Duration
	return &d
}
