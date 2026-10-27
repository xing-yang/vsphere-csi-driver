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
	"sync"
	"testing"
	"time"

	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/hms"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
)

const (
	siteA    = "site-a"
	siteB    = "site-b"
	volumeID = "1b2c3d4e-0000-4000-8000-000000000001"
	dsURL    = "ds:///vmfs/volumes/vsan:1/"
	dsMoID   = "datastore-12"
)

// fakeVolumes is an in-memory VolumeBackend for one site.
type fakeVolumes struct {
	mu         sync.Mutex
	vols       map[string]*VolumeInfo
	attached   map[string]bool
	registered map[string]string
	// files maps a datastore path to the datastore that has it.
	files map[string]string
}

func newFakeVolumes(ids ...string) *fakeVolumes {
	v := &fakeVolumes{vols: map[string]*VolumeInfo{}, attached: map[string]bool{}, registered: map[string]string{}}
	for _, id := range ids {
		v.vols[id] = &VolumeInfo{ID: id, VolumeType: common.BlockVolumeType, DatastoreURL: dsURL}
	}
	return v
}

func (v *fakeVolumes) GetVolume(_ context.Context, id string) (*VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.vols[id], nil
}

func (v *fakeVolumes) IsAttached(_ context.Context, id string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attached[id], nil
}

func (v *fakeVolumes) RegisterVolume(_ context.Context, id, diskURLPath, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.vols[id] = &VolumeInfo{ID: id, VolumeType: common.BlockVolumeType, DatastoreURL: dsURL}
	v.registered[id] = diskURLPath
	return nil
}

func (v *fakeVolumes) DatastoreMoID(_ context.Context, url string) (string, error) {
	if url != dsURL {
		return "", status.Error(codes.NotFound, "unknown datastore")
	}
	return dsMoID, nil
}

func (v *fakeVolumes) FindDatastoreWithFile(_ context.Context, path string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ds, ok := v.files[path]; ok {
		return ds, nil
	}
	return "", status.Error(codes.NotFound, "no datastore has the file")
}

// noSpecClient is a secondary-site HMS that, like the real one, does not
// implement currentSpec for secondary groups.
type noSpecClient struct{ hms.Client }

func (noSpecClient) GroupDatastore(context.Context, *hms.Group) (hms.MoRef, error) {
	return hms.MoRef{}, &hms.Fault{Type: "NotImplemented", Message: "not implemented"}
}

func TestPromoteFindsReplicaWithoutGroupSpec(t *testing.T) {
	fabric, a, _ := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	g, err := fabric.Site(siteB).FindIncomingGroup(context.Background(), groupName(volumeID))
	if err != nil {
		t.Fatal(err)
	}
	vols := newFakeVolumes()
	vols.files = map[string]string{hms.ReplicaDiskPath(g): "datastore-77"}
	// An HMS without GetRecoveredCnsDisk.
	client := noSpecClient{oldHMS{fabric.Site(siteB)}}
	b := NewServer(func(context.Context, map[string]string) (hms.Client, error) { return client, nil }, vols)
	if err := promote(&testSite{server: b, volumes: vols}, false); err != nil {
		t.Fatal(err)
	}
	if url := vols.registered[volumeID]; !strings.Contains(url, "dsName=datastore-77") {
		t.Fatalf("registered disk %q", url)
	}
}

func TestPromoteUsesReplicaDatastoreParameter(t *testing.T) {
	_, a, b := newSitesWith(t, func(c hms.Client) hms.Client { return oldHMS{c} })
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{ParamTargetSite: siteA, ParamTargetDatastore: dsMoID,
		ParamReplicaDatastore: "datastore-88"}
	if _, err := b.server.PromoteVolume(context.Background(),
		&replication.PromoteVolumeRequest{ReplicationSource: src(), Parameters: params}); err != nil {
		t.Fatal(err)
	}
	if url := b.volumes.registered[volumeID]; !strings.Contains(url, "dsName=datastore-88") {
		t.Fatalf("registered disk %q", url)
	}
}

func (v *fakeVolumes) DiskURLPath(_ context.Context, ds, path string) (string, error) {
	return "https://vc/folder/" + path + "?dcPath=dc&dsName=" + ds, nil
}

func (v *fakeVolumes) RecoveryPlacement(context.Context) (string, string, error) {
	return "group-v4", "resgroup-9", nil
}

// testSite is one Supervisor with its replication server and CNS volumes.
type testSite struct {
	server  *Server
	volumes *fakeVolumes
}

func newSites(t *testing.T) (*hms.FakeFabric, *testSite, *testSite) {
	t.Helper()
	fabric := hms.NewFakeFabric(siteA, siteB)
	site := func(name string, vols *fakeVolumes) *testSite {
		client := fabric.Site(name)
		return &testSite{
			server: NewServer(func(context.Context, map[string]string) (hms.Client, error) {
				return client, nil
			}, vols),
			volumes: vols,
		}
	}
	return fabric, site(siteA, newFakeVolumes(volumeID)), site(siteB, newFakeVolumes())
}

func src() *replication.ReplicationSource {
	return &replication.ReplicationSource{Type: &replication.ReplicationSource_Volume{
		Volume: &replication.ReplicationSource_VolumeSource{VolumeId: volumeID}}}
}

var vrcParams = map[string]string{ParamTargetSite: siteB, ParamTargetDatastore: targetDatastore,
	ParamSchedulingInterval: "5m"}

// targetDatastore is the replica datastore on site B.
const targetDatastore = "datastore-21"

func enable(t *testing.T, s *testSite) error {
	t.Helper()
	_, err := s.server.EnableVolumeReplication(context.Background(),
		&replication.EnableVolumeReplicationRequest{ReplicationSource: src(), Parameters: vrcParams})
	return err
}

func promote(s *testSite, force bool) error {
	_, err := s.server.PromoteVolume(context.Background(),
		&replication.PromoteVolumeRequest{ReplicationSource: src(), Parameters: vrcParams, Force: force})
	return err
}

func demote(s *testSite) error {
	_, err := s.server.DemoteVolume(context.Background(),
		&replication.DemoteVolumeRequest{ReplicationSource: src(), Parameters: vrcParams})
	return err
}

func resync(s *testSite) (*replication.ResyncVolumeResponse, error) {
	return s.server.ResyncVolume(context.Background(),
		&replication.ResyncVolumeRequest{ReplicationSource: src(), Parameters: vrcParams})
}

func info(t *testing.T, s *testSite) *replication.GetVolumeReplicationInfoResponse {
	t.Helper()
	res, err := s.server.GetVolumeReplicationInfo(context.Background(),
		&replication.GetVolumeReplicationInfoRequest{ReplicationSource: src()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("got %v, want code %s", err, code)
	}
}

func TestEnableIsIdempotentAndHealthy(t *testing.T) {
	_, a, b := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if err := enable(t, a); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	// The controller calls Enable on the target before promoting; it must
	// not configure a reverse replication.
	if err := enable(t, b); err != nil {
		t.Fatalf("enable on target: %v", err)
	}
	res := info(t, a)
	if res.Status != replication.GetVolumeReplicationInfoResponse_HEALTHY {
		t.Fatalf("status %s: %s", res.Status, res.StatusMessage)
	}
	if res.LastSyncTime == nil || res.LastSyncDuration == nil || res.LastSyncBytes == 0 {
		t.Fatalf("missing sync stats: %+v", res)
	}
	if res := info(t, b); res.Status != replication.GetVolumeReplicationInfoResponse_ERROR {
		t.Fatalf("target info status %s, want ERROR", res.Status)
	}
}

func TestEnableValidation(t *testing.T) {
	_, a, _ := newSites(t)
	for name, params := range map[string]map[string]string{
		"missing target":           {},
		"unknown parameter":        {ParamTargetSite: siteB, "bogus": "x"},
		"missing target datastore": {ParamTargetSite: siteB},
		"unpaired site":            {ParamTargetSite: "site-c", ParamTargetDatastore: targetDatastore},
	} {
		_, err := a.server.EnableVolumeReplication(context.Background(),
			&replication.EnableVolumeReplicationRequest{ReplicationSource: src(), Parameters: params})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: got %v, want FailedPrecondition", name, err)
		}
	}
}

func TestLastSyncTimeAdvancesWithGeneration(t *testing.T) {
	_, a, _ := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	a.server.now = func() time.Time { return now }
	first := info(t, a).LastSyncTime.AsTime()
	now = now.Add(time.Minute)
	if got := info(t, a).LastSyncTime.AsTime(); !got.Equal(first) {
		t.Fatalf("sync time moved without a new generation: %v -> %v", first, got)
	}
	if err := demote(a); err != nil { // runs one sync cycle
		t.Fatal(err)
	}
	if got := info(t, a).LastSyncTime.AsTime(); !got.Equal(now) {
		t.Fatalf("sync time %v, want %v", got, now)
	}
}

func TestPlannedMigration(t *testing.T) {
	fabric, a, b := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if res, err := resync(b); err != nil || !res.Ready {
		t.Fatalf("target resync: %v %v", res, err)
	}

	a.volumes.attached[volumeID] = true
	wantCode(t, demote(a), codes.FailedPrecondition)
	a.volumes.attached[volumeID] = false
	if err := demote(a); err != nil {
		t.Fatal(err)
	}
	if err := demote(a); err != nil {
		t.Fatalf("repeated demote: %v", err)
	}
	if res, err := resync(a); err != nil || res.Ready {
		t.Fatalf("demoted source resync: %v %v", res, err)
	}
	if err := demote(b); err != nil {
		t.Fatalf("demote on target: %v", err)
	}

	if err := promote(b, false); err != nil {
		t.Fatal(err)
	}
	url := b.volumes.registered[volumeID]
	if !strings.Contains(url, "hbr-pe-gid-") || !strings.Contains(url, "cnsVolume.vmdk") ||
		!strings.Contains(url, targetDatastore) {
		t.Fatalf("registered disk %q", url)
	}
	g, err := fabric.Site(siteB).FindIncomingGroup(context.Background(), groupName(volumeID))
	if err != nil || g.Status != hms.StatusRecovered {
		t.Fatalf("secondary group %+v %v", g, err)
	}
	// Promote is called on every reconcile.
	if err := promote(b, false); err != nil {
		t.Fatalf("repeated promote: %v", err)
	}

	if res := info(t, a); res.Status != replication.GetVolumeReplicationInfoResponse_ERROR {
		t.Fatalf("old primary status %s", res.Status)
	}
	if res := info(t, b); res.Status != replication.GetVolumeReplicationInfoResponse_ERROR ||
		!strings.Contains(res.StatusMessage, "re-protect") {
		t.Fatalf("new primary status %s: %s", res.Status, res.StatusMessage)
	}
	// Failing back needs a resync first.
	wantCode(t, promote(a, false), codes.FailedPrecondition)

	if _, err := a.server.DisableVolumeReplication(context.Background(),
		&replication.DisableVolumeReplicationRequest{ReplicationSource: src(), Parameters: vrcParams}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.Site(siteB).FindIncomingGroup(context.Background(), groupName(volumeID)); err == nil {
		t.Fatal("secondary group still exists after disable")
	}
}

func TestPromoteSourceIsNoop(t *testing.T) {
	_, a, b := newSites(t)
	wantCode(t, promote(b, false), codes.NotFound)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if err := promote(a, false); err != nil {
		t.Fatal(err)
	}
	if len(a.volumes.registered) != 0 {
		t.Fatal("source volume was registered")
	}
}

func TestForcedPromoteSkipsRegistrationWhenVolumeExists(t *testing.T) {
	_, a, b := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	b.volumes.vols[volumeID] = &VolumeInfo{ID: volumeID, VolumeType: common.BlockVolumeType}
	if err := promote(b, true); err != nil {
		t.Fatal(err)
	}
	if len(b.volumes.registered) != 0 {
		t.Fatal("existing volume was registered again")
	}
}

func TestDisableWithoutGroup(t *testing.T) {
	_, a, _ := newSites(t)
	if _, err := a.server.DisableVolumeReplication(context.Background(),
		&replication.DisableVolumeReplicationRequest{ReplicationSource: src()}); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeGroupsUnsupported(t *testing.T) {
	_, a, _ := newSites(t)
	_, err := a.server.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{
		ReplicationSource: &replication.ReplicationSource{Type: &replication.ReplicationSource_Volumegroup{
			Volumegroup: &replication.ReplicationSource_VolumeGroupSource{VolumeGroupId: "g"}}},
	})
	wantCode(t, err, codes.Unimplemented)
}

func TestFakeTargetRolePromote(t *testing.T) {
	fabric := hms.NewFakeFabric(siteB, siteA)
	fabric.ActAsReplicaSite(siteB, siteA, hms.MoRef{Type: "Datastore", Value: dsMoID})
	client := fabric.Site(siteB)
	vols := newFakeVolumes()
	b := &testSite{
		server: NewServer(func(context.Context, map[string]string) (hms.Client, error) { return client, nil },
			vols, WithSkipVolumeRegistration(true)),
		volumes: vols,
	}
	if err := enable(t, b); err != nil {
		t.Fatalf("enable on target: %v", err)
	}
	if res, err := resync(b); err != nil || !res.Ready {
		t.Fatalf("resync: %v %v", res, err)
	}
	if err := promote(b, false); err != nil {
		t.Fatal(err)
	}
	if len(vols.registered) != 0 {
		t.Fatal("registered although registration is skipped")
	}
	g, err := client.FindIncomingGroup(context.Background(), groupName(volumeID))
	if err != nil || g.Status != hms.StatusRecovered {
		t.Fatalf("group %+v %v", g, err)
	}
}

// oldHMS is an HMS build without CNS reprotect support, which rejects the
// methods it does not have.
type oldHMS struct{ hms.Client }

var errNoMethod = &hms.Fault{Type: "InvalidRequest",
	Message: "Invalid managed method 'HmsGetPeReprotectOperation' requested for Managed Object Type"}

func (oldHMS) PrepareRecovery(context.Context, *hms.Group, string) error { return errNoMethod }
func (oldHMS) SealRecovery(context.Context, *hms.Group, string) error    { return errNoMethod }
func (oldHMS) RecoveredDisk(context.Context, *hms.Group) (*hms.DatastorePath, error) {
	return nil, errNoMethod
}
func (oldHMS) BindRecoveredVolume(context.Context, *hms.Group, string, hms.MoRef) error {
	return errNoMethod
}
func (oldHMS) StartReprotect(context.Context, *hms.Group, hms.ReprotectSpec) error {
	return errNoMethod
}
func (oldHMS) ReprotectOperation(context.Context, string) (*hms.ReprotectOperation, error) {
	return nil, errNoMethod
}
func (oldHMS) ResumeReprotect(context.Context, *hms.Group, string) error { return errNoMethod }
func (o oldHMS) Peer(ctx context.Context) (hms.Client, error) {
	peer, err := o.Client.Peer(ctx)
	if err != nil {
		return nil, err
	}
	return oldHMS{peer}, nil
}

// noPeerHMS has no peer HMS endpoint configured.
type noPeerHMS struct{ hms.Client }

func (noPeerHMS) Peer(context.Context) (hms.Client, error) {
	return nil, hms.ErrPeerUnreachable
}

// newSitesWith is newSites with the clients wrapped by wrap.
func newSitesWith(t *testing.T, wrap func(hms.Client) hms.Client) (*hms.FakeFabric, *testSite, *testSite) {
	t.Helper()
	fabric := hms.NewFakeFabric(siteA, siteB)
	site := func(name string, vols *fakeVolumes) *testSite {
		client := wrap(fabric.Site(name))
		return &testSite{
			server: NewServer(func(context.Context, map[string]string) (hms.Client, error) {
				return client, nil
			}, vols),
			volumes: vols,
		}
	}
	return fabric, site(siteA, newFakeVolumes(volumeID)), site(siteB, newFakeVolumes())
}

func wantReady(t *testing.T, s *testSite, want bool) {
	t.Helper()
	res, err := resync(s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready != want {
		t.Fatalf("resync ready %t, want %t", res.Ready, want)
	}
}

func wantHealthy(t *testing.T, s *testSite) {
	t.Helper()
	if res := info(t, s); res.Status != replication.GetVolumeReplicationInfoResponse_HEALTHY {
		t.Fatalf("status %s: %s", res.Status, res.StatusMessage)
	}
}

// reprotectMode returns the synchronization mode of the reprotect of group on site.
func reprotectMode(t *testing.T, fabric *hms.FakeFabric, site string, group *hms.Group) string {
	t.Helper()
	op, err := fabric.Site(site).ReprotectOperation(context.Background(), hms.ReprotectOperationID(group))
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase != hms.ReprotectPhaseComplete {
		t.Fatalf("reprotect phase %s", op.Phase)
	}
	return op.SynchronizationMode
}

func TestFailoverReprotectAndFailback(t *testing.T) {
	fabric, a, b := newSites(t)
	ctx := context.Background()
	name := groupName(volumeID)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	forward, err := fabric.Site(siteA).FindOutgoingGroup(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	// Planned migration to site B. The demote seals a recovery checkpoint.
	if err := demote(a); err != nil {
		t.Fatal(err)
	}
	if err := promote(b, false); err != nil {
		t.Fatal(err)
	}
	if url := b.volumes.registered[volumeID]; !strings.Contains(url, "hbr-pe-gid-"+forward.Ref.Value) ||
		!strings.Contains(url, targetDatastore) {
		t.Fatalf("registered disk %q", url)
	}

	// Site A stays secondary and resyncs, which re-protects the volume.
	wantCode(t, promote(a, false), codes.FailedPrecondition)
	a.volumes.attached[volumeID] = true
	_, err = resync(a)
	wantCode(t, err, codes.FailedPrecondition)
	a.volumes.attached[volumeID] = false
	fabric.HoldReprotects(true)
	wantReady(t, a, false) // starts the reprotect
	wantReady(t, a, false) // still running
	fabric.HoldReprotects(false)
	fabric.FinishReprotects()
	wantReady(t, a, true)
	if mode := reprotectMode(t, fabric, siteB, forward); mode != "optimized" {
		t.Fatalf("reprotect mode %s", mode)
	}
	wantHealthy(t, b)
	if err := demote(a); err != nil {
		t.Fatalf("demote of the new secondary: %v", err)
	}

	// Fail back to site A. Its original volume is the replica, so nothing is
	// registered.
	reverse, err := fabric.Site(siteB).FindOutgoingGroup(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if reverse.Ref.Value == forward.Ref.Value {
		t.Fatal("reprotect kept the group ID")
	}
	if err := demote(b); err != nil {
		t.Fatal(err)
	}
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if err := promote(a, false); err != nil {
		t.Fatal(err)
	}
	if len(a.volumes.registered) != 0 {
		t.Fatalf("registered %v on the original site", a.volumes.registered)
	}
	wantReady(t, b, false)
	wantReady(t, b, true)
	if mode := reprotectMode(t, fabric, siteA, reverse); mode != "optimized" {
		t.Fatalf("second reprotect mode %s", mode)
	}
	wantHealthy(t, a)
	if _, err := fabric.Site(siteB).FindOutgoingGroup(ctx, name); err == nil {
		t.Fatal("reverse group still exists after the second reprotect")
	}
}

func TestReprotectWithoutCheckpointCopiesWholeVolume(t *testing.T) {
	fabric, a, b := newSites(t)
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	forward, err := fabric.Site(siteA).FindOutgoingGroup(context.Background(), groupName(volumeID))
	if err != nil {
		t.Fatal(err)
	}
	// Unplanned failover: no demote on site A.
	if err := promote(b, true); err != nil {
		t.Fatal(err)
	}
	wantReady(t, a, false)
	wantReady(t, a, true)
	if mode := reprotectMode(t, fabric, siteB, forward); mode != "fullSync" {
		t.Fatalf("reprotect mode %s", mode)
	}
}

func TestFailoverWithoutReprotectSupport(t *testing.T) {
	_, a, b := newSitesWith(t, func(c hms.Client) hms.Client { return oldHMS{c} })
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if err := demote(a); err != nil {
		t.Fatalf("demote falls back to a plain sync: %v", err)
	}
	if err := promote(b, false); err != nil {
		t.Fatal(err)
	}
	if url := b.volumes.registered[volumeID]; !strings.Contains(url, "hbr-pe-gid-") {
		t.Fatalf("registered disk %q", url)
	}
	_, err := resync(a)
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "does not support re-protecting") {
		t.Fatalf("resync error %v", err)
	}
}

func TestResyncNeedsPeerHMS(t *testing.T) {
	_, a, b := newSitesWith(t, func(c hms.Client) hms.Client { return noPeerHMS{c} })
	if err := enable(t, a); err != nil {
		t.Fatal(err)
	}
	if err := promote(b, true); err != nil {
		t.Fatal(err)
	}
	_, err := resync(a)
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), SecretKeyPeerHMSEndpoint) {
		t.Fatalf("resync error %v", err)
	}
}
