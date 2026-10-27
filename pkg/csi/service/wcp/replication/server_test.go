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
	client := noSpecClient{fabric.Site(siteB)}
	b := NewServer(func(context.Context, map[string]string) (hms.Client, error) { return client, nil }, vols)
	if err := promote(&testSite{server: b, volumes: vols}, false); err != nil {
		t.Fatal(err)
	}
	if url := vols.registered[volumeID]; !strings.Contains(url, "dsName=datastore-77") {
		t.Fatalf("registered disk %q", url)
	}
}

func TestPromoteUsesReplicaDatastoreParameter(t *testing.T) {
	_, a, b := newSites(t)
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
		!strings.Contains(res.StatusMessage, "re-protecting") {
		t.Fatalf("new primary status %s: %s", res.Status, res.StatusMessage)
	}
	_, err = resync(a)
	wantCode(t, err, codes.FailedPrecondition)
	// Failback is not supported.
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
