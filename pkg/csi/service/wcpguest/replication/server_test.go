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
	"strings"
	"testing"
	"time"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientset "k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

const (
	testNamespace = "sv-ns"
	testVolume    = "tkc-uid-1234"
	testGuestPV   = "pvc-1234"
	testClass     = "vsphere-replication"
)

var testParams = map[string]string{ParamSupervisorClass: testClass}

type fixture struct {
	t          *testing.T
	server     *Server
	dyn        *dynamicfake.FakeDynamicClient
	supervisor *k8sfake.Clientset
	guest      *k8sfake.Clientset
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{vrResource: "VolumeReplicationList"})
	supervisor := k8sfake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testVolume, Namespace: testNamespace},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	})
	guest := k8sfake.NewClientset(&corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestPV},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: csitypes.Name, VolumeHandle: testVolume},
		}},
	})
	f := &fixture{t: t, dyn: dyn, supervisor: supervisor, guest: guest}
	f.server = NewServer(Config{
		Clients: func(context.Context) (dynamic.Interface, clientset.Interface, clientset.Interface, error) {
			return dyn, supervisor, guest, nil
		},
		SupervisorNamespace: testNamespace,
		Labels:              map[string]string{"tkc/TKGService": "tkc-uid"},
		PollInterval:        5 * time.Millisecond,
		WaitTimeout:         200 * time.Millisecond,
	})
	return f
}

func source(volumeID string) *replication.ReplicationSource {
	return &replication.ReplicationSource{Type: &replication.ReplicationSource_Volume{
		Volume: &replication.ReplicationSource_VolumeSource{VolumeId: volumeID},
	}}
}

func (f *fixture) resource() dynamic.ResourceInterface {
	return f.dyn.Resource(vrResource).Namespace(testNamespace)
}

func (f *fixture) getVR() *unstructured.Unstructured {
	f.t.Helper()
	obj, err := f.resource().Get(context.Background(), testVolume, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("get VolumeReplication: %v", err)
	}
	return obj
}

type condition struct {
	condType, status, reason, message string
}

// createVR creates a Supervisor VolumeReplication with the given spec state,
// generation and status.
func (f *fixture) createVR(specState string, generation, observed int64, state string, conds ...condition) {
	f.t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vrGroup + "/" + vrVersion,
		"kind":       vrKind,
		"metadata":   map[string]interface{}{"name": testVolume, "namespace": testNamespace},
		"spec": map[string]interface{}{
			"volumeReplicationClass": testClass,
			"replicationState":       specState,
			"dataSource": map[string]interface{}{
				"apiGroup": "", "kind": "PersistentVolumeClaim", "name": testVolume},
		},
	}}
	obj.SetGeneration(generation)
	setStatus(obj, observed, state, conds...)
	if _, err := f.resource().Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("create VolumeReplication: %v", err)
	}
}

func setStatus(obj *unstructured.Unstructured, observed int64, state string, conds ...condition) {
	var list []interface{}
	for _, c := range conds {
		list = append(list, map[string]interface{}{
			"type": c.condType, "status": c.status, "reason": c.reason, "message": c.message,
			"observedGeneration": obj.GetGeneration(), "lastTransitionTime": "2026-10-23T00:00:00Z",
		})
	}
	_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"state":              state,
		"observedGeneration": observed,
		"conditions":         list,
	}, "status")
}

// reconcileTo simulates the csi-addons controller on the Supervisor: once the
// VolumeReplication's spec has the given state, it reports state with conds.
func (f *fixture) reconcileTo(specState, state string, conds ...condition) {
	go func() {
		for i := 0; i < 100; i++ {
			obj, err := f.resource().Get(context.Background(), testVolume, metav1.GetOptions{})
			if err == nil {
				got, _, _ := unstructured.NestedString(obj.Object, "spec", "replicationState")
				if got == specState {
					setStatus(obj, obj.GetGeneration(), state, conds...)
					_, _ = f.resource().Update(context.Background(), obj, metav1.UpdateOptions{})
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func (f *fixture) attach() {
	f.t.Helper()
	pv := testGuestPV
	_, err := f.guest.StorageV1().VolumeAttachments().Create(context.Background(), &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-1"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: csitypes.Name,
			NodeName: "guest-node-1",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code codes.Code, substr string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("got error %v, want code %s", err, code)
	}
	if substr != "" && !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err, substr)
	}
}

func TestEnableCreatesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.resource().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("Enable created %d VolumeReplications", len(list.Items))
	}

	_, err = f.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		ReplicationSource: source(testVolume),
	})
	wantCode(t, err, codes.InvalidArgument, ParamSupervisorClass)

	_, err = f.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		ReplicationSource: source("missing"), Parameters: testParams,
	})
	wantCode(t, err, codes.NotFound, "Supervisor PVC")
}

func TestPromoteCreatesPrimary(t *testing.T) {
	f := newFixture(t)
	f.reconcileTo(statePrimary, statusPrimary)
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	obj := f.getVR()
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "volumeReplicationClass"); got != testClass {
		t.Errorf("class = %q", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "dataSource", "name"); got != testVolume {
		t.Errorf("dataSource = %q", got)
	}
	if obj.GetLabels()["tkc/TKGService"] != "tkc-uid" || obj.GetAnnotations()[AnnGuestInitiated] != "true" {
		t.Errorf("labels %v, annotations %v", obj.GetLabels(), obj.GetAnnotations())
	}
}

func TestPromotePatchesSecondary(t *testing.T) {
	f := newFixture(t)
	f.createVR(stateSecondary, 0, 0, statusSecondary)
	f.reconcileTo(statePrimary, statusPrimary)
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(f.getVR().Object, "spec", "replicationState"); got != statePrimary {
		t.Fatalf("replicationState = %q", got)
	}
}

func TestPromoteWaitsForCurrentGeneration(t *testing.T) {
	f := newFixture(t)
	// The Supervisor reported Primary for an older spec.
	f.createVR(statePrimary, 2, 1, statusPrimary)
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.Aborted, "timed out")
}

func TestPromoteFailureIsNotFailedPrecondition(t *testing.T) {
	f := newFixture(t)
	f.createVR(statePrimary, 0, 0, statusSecondary,
		condition{condCompleted, "False", "FailedToPromote", "failed to promote volume"},
		condition{condDegraded, "True", reasonError, "volume failed to promote: recover group failed"})
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.Internal, "recover group failed")
}

func TestPromoteIgnoresIntermediateStatus(t *testing.T) {
	f := newFixture(t)
	// Observed, but still Secondary with a progress message and no failed
	// Completed condition: the Supervisor is still reconciling.
	f.createVR(statePrimary, 0, 0, statusSecondary)
	obj := f.getVR()
	_ = unstructured.SetNestedField(obj.Object, "destination info pending retrieval", "status", "message")
	if _, err := f.resource().Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.Aborted, "destination info pending retrieval")
}

func TestDemoteAttached(t *testing.T) {
	f := newFixture(t)
	f.createVR(statePrimary, 0, 0, statusPrimary)
	f.attach()
	_, err := f.server.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.FailedPrecondition, "guest-node-1")
	if got, _, _ := unstructured.NestedString(f.getVR().Object, "spec", "replicationState"); got != statePrimary {
		t.Fatalf("an attached volume was demoted: replicationState = %q", got)
	}
}

func TestDemote(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createVR(statePrimary, 0, 0, statusPrimary)
	f.reconcileTo(stateSecondary, statusSecondary)
	req := &replication.DemoteVolumeRequest{ReplicationSource: source(testVolume), Parameters: testParams}
	if _, err := f.server.DemoteVolume(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(f.getVR().Object, "spec", "replicationState"); got != stateSecondary {
		t.Fatalf("replicationState = %q", got)
	}
	// The controller calls Demote on every reconcile. A demoted volume is not
	// checked again, even if a pod mounted it since.
	f.attach()
	if _, err := f.server.DemoteVolume(ctx, req); err != nil {
		t.Fatalf("second Demote: %v", err)
	}
}

func TestDemoteCreatesSecondary(t *testing.T) {
	f := newFixture(t)
	f.reconcileTo(stateSecondary, statusSecondary)
	_, err := f.server.DemoteVolume(context.Background(), &replication.DemoteVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResync(t *testing.T) {
	tests := []struct {
		name      string
		specState string
		state     string
		conds     []condition
		ready     bool
		code      codes.Code
	}{
		{name: "in sync", specState: stateSecondary, state: statusSecondary,
			conds: []condition{{condDegraded, "False", reasonHealthy, ""}}, ready: true},
		{name: "resyncing", specState: stateSecondary, state: statusSecondary,
			conds: []condition{{condDegraded, "True", "VolumeDegraded", ""}, {condResyncing, "True", "ResyncTriggered", ""}}},
		{name: "not demoted yet", specState: stateSecondary, state: statusPrimary,
			conds: []condition{{condDegraded, "False", reasonHealthy, ""}}},
		{name: "failed", specState: stateSecondary, state: statusSecondary,
			conds: []condition{{condResyncing, "False", reasonFailedToResync, "volume is not resyncing"},
				{condDegraded, "True", reasonError, "volume failed to resync: reprotect not supported"}},
			code: codes.FailedPrecondition},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.createVR(tc.specState, 0, 0, tc.state, tc.conds...)
			resp, err := f.server.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
				ReplicationSource: source(testVolume), Parameters: testParams,
			})
			if tc.code != codes.OK {
				wantCode(t, err, tc.code, "reprotect not supported")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetReady() != tc.ready {
				t.Fatalf("ready = %t, want %t", resp.GetReady(), tc.ready)
			}
		})
	}
}

func TestResyncMissing(t *testing.T) {
	f := newFixture(t)
	_, err := f.server.ResyncVolume(context.Background(), &replication.ResyncVolumeRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.NotFound, "")
}

func TestGetVolumeReplicationInfo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	req := &replication.GetVolumeReplicationInfoRequest{ReplicationSource: source(testVolume)}

	resp, err := f.server.GetVolumeReplicationInfo(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != replication.GetVolumeReplicationInfoResponse_ERROR {
		t.Fatalf("missing VR: status %s", resp.GetStatus())
	}

	f.createVR(statePrimary, 0, 0, statusPrimary,
		condition{condReplicating, "True", "Replicating", "volume is replicating: group is ok"})
	obj := f.getVR()
	_ = unstructured.SetNestedField(obj.Object, "2026-10-23T01:02:03Z", "status", "lastSyncTime")
	_ = unstructured.SetNestedField(obj.Object, "1m30s", "status", "lastSyncDuration")
	_ = unstructured.SetNestedField(obj.Object, int64(4096), "status", "lastSyncBytes")
	if _, err := f.resource().Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	resp, err = f.server.GetVolumeReplicationInfo(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != replication.GetVolumeReplicationInfoResponse_HEALTHY ||
		!strings.Contains(resp.GetStatusMessage(), "group is ok") {
		t.Fatalf("status %s %q", resp.GetStatus(), resp.GetStatusMessage())
	}
	if got := resp.GetLastSyncTime().AsTime(); !got.Equal(time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)) {
		t.Errorf("lastSyncTime = %s", got)
	}
	if resp.GetLastSyncDuration().AsDuration() != 90*time.Second || resp.GetLastSyncBytes() != 4096 {
		t.Errorf("lastSyncDuration = %s, lastSyncBytes = %d",
			resp.GetLastSyncDuration().AsDuration(), resp.GetLastSyncBytes())
	}
}

func TestGetVolumeReplicationInfoNotReplicating(t *testing.T) {
	f := newFixture(t)
	f.createVR(statePrimary, 0, 0, statusPrimary,
		condition{condReplicating, "False", "NotReplicating", "volume is not replicating: recovered"})
	resp, err := f.server.GetVolumeReplicationInfo(context.Background(),
		&replication.GetVolumeReplicationInfoRequest{ReplicationSource: source(testVolume)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != replication.GetVolumeReplicationInfoResponse_DEGRADED {
		t.Fatalf("status %s", resp.GetStatus())
	}
}

func TestDisable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	req := &replication.DisableVolumeReplicationRequest{ReplicationSource: source(testVolume), Parameters: testParams}
	f.createVR(statePrimary, 0, 0, statusPrimary)
	if _, err := f.server.DisableVolumeReplication(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Already gone.
	if _, err := f.server.DisableVolumeReplication(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestDisableWaitsForFinalizer(t *testing.T) {
	f := newFixture(t)
	f.createVR(statePrimary, 0, 0, statusPrimary)
	// The Supervisor's finalizer keeps the object until replication is disabled.
	f.dyn.PrependReactor("delete", "volumereplications",
		func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	_, err := f.server.DisableVolumeReplication(context.Background(), &replication.DisableVolumeReplicationRequest{
		ReplicationSource: source(testVolume), Parameters: testParams,
	})
	wantCode(t, err, codes.Aborted, "")
}

func TestVolumeGroupUnsupported(t *testing.T) {
	f := newFixture(t)
	_, err := f.server.PromoteVolume(context.Background(), &replication.PromoteVolumeRequest{
		ReplicationSource: &replication.ReplicationSource{Type: &replication.ReplicationSource_Volumegroup{
			Volumegroup: &replication.ReplicationSource_VolumeGroupSource{VolumeGroupId: "g"},
		}},
		Parameters: testParams,
	})
	wantCode(t, err, codes.Unimplemented, "")
}
