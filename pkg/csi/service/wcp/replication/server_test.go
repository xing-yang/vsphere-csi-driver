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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/snapservice"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
)

const (
	siteA    = "site-a"
	siteB    = "site-b"
	volumeID = "1b2c3d4e-0000-4000-8000-000000000001"
)

// fakeVolumes is an in-memory VolumeBackend for one site.
type fakeVolumes struct {
	mu         sync.Mutex
	vols       map[string]*VolumeInfo
	attached   map[string]bool
	registered []string
}

func newFakeVolumes(ids ...string) *fakeVolumes {
	v := &fakeVolumes{vols: map[string]*VolumeInfo{}, attached: map[string]bool{}}
	for _, id := range ids {
		v.vols[id] = &VolumeInfo{ID: id, VolumeType: common.BlockVolumeType}
	}
	return v
}

func (v *fakeVolumes) GetVolume(ctx context.Context, id string) (*VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.vols[id], nil
}

func (v *fakeVolumes) IsAttached(ctx context.Context, id string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attached[id], nil
}

func (v *fakeVolumes) RegisterVolume(ctx context.Context, id, diskURLPath, policyID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.vols[id] = &VolumeInfo{ID: id, VolumeType: common.BlockVolumeType}
	v.registered = append(v.registered, id)
	return nil
}

// testSite is one Supervisor with its replication server and CNS volumes.
type testSite struct {
	name    string
	server  *Server
	volumes *fakeVolumes
	params  map[string]string
}

// siteFactory returns the SnapService client a site's driver uses.
type siteFactory func(t *testing.T, fabric *snapservice.FakeFabric, site string) snapservice.Client

// directClient uses the fake fabric in process.
func directClient(t *testing.T, fabric *snapservice.FakeFabric, site string) snapservice.Client {
	return fabric.Site(site)
}

// restClients returns a factory whose clients talk to one fake SnapService
// REST server, as two Supervisors sharing a mock server would.
func restClients() siteFactory {
	var server *httptest.Server
	return func(t *testing.T, fabric *snapservice.FakeFabric, site string) snapservice.Client {
		t.Helper()
		if server == nil {
			server = httptest.NewServer(snapservice.NewFakeServer(fabric).Handler())
			t.Cleanup(server.Close)
		}
		client, err := snapservice.NewRESTClient(snapservice.RESTConfig{
			Endpoint:     server.URL + "/" + site,
			Site:         site,
			Cluster:      "domain-c1",
			Username:     "user",
			Password:     "pass",
			PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
}

// backends runs a test against the in-process fake and over REST.
func backends(t *testing.T, test func(t *testing.T, newClient siteFactory)) {
	t.Run("direct", func(t *testing.T) { test(t, directClient) })
	t.Run("rest", func(t *testing.T) { test(t, restClients()) })
}

func newTestSites(t *testing.T, opts ...snapservice.FakeOption) (*snapservice.FakeFabric, *testSite, *testSite) {
	t.Helper()
	return newTestSitesWith(t, directClient, opts...)
}

func newTestSitesWith(t *testing.T, newClient siteFactory,
	opts ...snapservice.FakeOption) (*snapservice.FakeFabric, *testSite, *testSite) {
	t.Helper()
	fabric := snapservice.NewFakeFabric([]string{siteA, siteB}, opts...)
	newSite := func(name, peer string, vols *fakeVolumes) *testSite {
		client := newClient(t, fabric, name)
		factory := func(ctx context.Context, secrets map[string]string) (snapservice.Client, error) {
			return client, nil
		}
		return &testSite{
			name:    name,
			server:  NewServer(factory, vols),
			volumes: vols,
			params:  map[string]string{ParamTargetSite: peer, ParamSchedulingInterval: "5m"},
		}
	}
	// The volume exists on site A only; site B has a PV with the same handle
	// but no CNS volume until it is promoted.
	return fabric, newSite(siteA, siteB, newFakeVolumes(volumeID)), newSite(siteB, siteA, newFakeVolumes())
}

func volumeSource(id string) *replication.ReplicationSource {
	return &replication.ReplicationSource{
		Type: &replication.ReplicationSource_Volume{
			Volume: &replication.ReplicationSource_VolumeSource{VolumeId: id},
		},
	}
}

// markPrimary does what the csi-addons controller does for a primary VR.
func (s *testSite) markPrimary(ctx context.Context) error {
	if _, err := s.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		ReplicationSource: volumeSource(volumeID),
		Parameters:        s.params,
	}); err != nil {
		return err
	}
	return s.promote(ctx, false)
}

func (s *testSite) promote(ctx context.Context, force bool) error {
	_, err := s.server.PromoteVolume(ctx, &replication.PromoteVolumeRequest{
		ReplicationSource: volumeSource(volumeID),
		Parameters:        s.params,
		Force:             force,
	})
	return err
}

// markPrimaryWithForceRetry mirrors the controller retrying a promote with
// force after FailedPrecondition.
func (s *testSite) markPrimaryWithForceRetry(ctx context.Context) error {
	err := s.markPrimary(ctx)
	if status.Code(err) == codes.FailedPrecondition {
		return s.promote(ctx, true)
	}
	return err
}

func (s *testSite) demote(ctx context.Context) error {
	_, err := s.server.DemoteVolume(ctx, &replication.DemoteVolumeRequest{
		ReplicationSource: volumeSource(volumeID),
		Parameters:        s.params,
	})
	return err
}

func (s *testSite) resync(ctx context.Context) (bool, error) {
	resp, err := s.server.ResyncVolume(ctx, &replication.ResyncVolumeRequest{
		ReplicationSource: volumeSource(volumeID),
		Parameters:        s.params,
	})
	if err != nil {
		return false, err
	}
	return resp.GetReady(), nil
}

func (s *testSite) info(ctx context.Context) (*replication.GetVolumeReplicationInfoResponse, error) {
	return s.server.GetVolumeReplicationInfo(ctx, &replication.GetVolumeReplicationInfoRequest{
		ReplicationSource: volumeSource(volumeID),
	})
}

func (s *testSite) pgState(t *testing.T, fabric *snapservice.FakeFabric) snapservice.ProtectionGroupState {
	t.Helper()
	pg, err := fabric.Site(s.name).GetProtectionGroup(context.Background(), protectionGroupName(volumeID))
	if err != nil {
		t.Fatalf("site %s: get protection group: %v", s.name, err)
	}
	return pg.State
}

func expectCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("expected code %s, got %v", code, err)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnableVolumeReplication(t *testing.T) {
	ctx := context.Background()
	fabric, a, b := newTestSites(t)

	mustNoErr(t, a.markPrimary(ctx))
	if got := a.pgState(t, fabric); got != snapservice.StateActive {
		t.Fatalf("site A state = %s, want ACTIVE", got)
	}
	if got := b.pgState(t, fabric); got != snapservice.StateDormant {
		t.Fatalf("site B state = %s, want DORMANT", got)
	}
	// The controller calls enable again whenever the VR is not yet primary.
	mustNoErr(t, a.markPrimary(ctx))

	info, err := a.info(ctx)
	mustNoErr(t, err)
	if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_HEALTHY || info.GetLastSyncTime() == nil {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestEnableVolumeReplicationValidation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		volID  string
		params map[string]string
		setup  func(*testSite)
		code   codes.Code
	}{
		{name: "missing target site", volID: volumeID, params: map[string]string{}, code: codes.FailedPrecondition},
		{name: "unknown parameter", volID: volumeID,
			params: map[string]string{ParamTargetSite: siteB, "bogus": "x"}, code: codes.FailedPrecondition},
		{name: "bad scheduling interval", volID: volumeID,
			params: map[string]string{ParamTargetSite: siteB, ParamSchedulingInterval: "soon"},
			code:   codes.FailedPrecondition},
		{name: "unpaired site", volID: volumeID, params: map[string]string{ParamTargetSite: "site-c"},
			code: codes.FailedPrecondition},
		{name: "volume not found", volID: "missing", params: map[string]string{ParamTargetSite: siteB},
			code: codes.NotFound},
		{name: "file volume", volID: volumeID, params: map[string]string{ParamTargetSite: siteB},
			setup: func(s *testSite) { s.volumes.vols[volumeID].VolumeType = common.FileVolumeType },
			code:  codes.FailedPrecondition},
		{name: "no volume ID", volID: "", params: map[string]string{ParamTargetSite: siteB},
			code: codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, a, _ := newTestSites(t)
			if tc.setup != nil {
				tc.setup(a)
			}
			_, err := a.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
				ReplicationSource: volumeSource(tc.volID),
				Parameters:        tc.params,
			})
			expectCode(t, err, tc.code)
		})
	}
}

func TestPlannedMigrationAndFailback(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, b := newTestSitesWith(t, newClient)

		// Site A primary, site B secondary.
		mustNoErr(t, a.markPrimary(ctx))
		mustNoErr(t, b.demote(ctx))
		ready, err := b.resync(ctx)
		mustNoErr(t, err)
		if !ready {
			t.Fatal("site B should be ready after the initial sync")
		}

		// Demote is refused while the workload still has the volume attached.
		a.volumes.attached[volumeID] = true
		expectCode(t, a.demote(ctx), codes.FailedPrecondition)
		a.volumes.attached[volumeID] = false
		mustNoErr(t, a.demote(ctx))
		mustNoErr(t, a.demote(ctx)) // Idempotent.
		if got := a.pgState(t, fabric); got != snapservice.StateDemoted {
			t.Fatalf("site A state = %s, want DEMOTED", got)
		}

		// Site A cannot re-protect until site B is promoted.
		ready, err = a.resync(ctx)
		mustNoErr(t, err)
		if ready {
			t.Fatal("site A must not be ready before site B is promoted")
		}

		// Promote site B: the recovered disk is registered with the same ID.
		mustNoErr(t, b.markPrimary(ctx))
		if got := b.pgState(t, fabric); got != snapservice.StateActive {
			t.Fatalf("site B state = %s, want ACTIVE", got)
		}
		if len(b.volumes.registered) != 1 || b.volumes.registered[0] != volumeID {
			t.Fatalf("site B registered %v, want [%s]", b.volumes.registered, volumeID)
		}

		// Site A re-protects as a replica of site B.
		ready, err = a.resync(ctx)
		mustNoErr(t, err)
		if !ready {
			t.Fatal("site A should be ready after re-protect")
		}
		if got := a.pgState(t, fabric); got != snapservice.StateDormant {
			t.Fatalf("site A state = %s, want DORMANT", got)
		}
		info, err := b.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_HEALTHY {
			t.Fatalf("site B info status = %s, want HEALTHY", info.GetStatus())
		}

		// Fail back to site A. The volume still exists there, so nothing is registered.
		mustNoErr(t, b.demote(ctx))
		mustNoErr(t, a.markPrimary(ctx))
		ready, err = b.resync(ctx)
		mustNoErr(t, err)
		if !ready {
			t.Fatal("site B should be ready after failback")
		}
		if len(a.volumes.registered) != 0 {
			t.Fatalf("site A registered %v, want none", a.volumes.registered)
		}
	})
}

func TestPromoteWithActivePeerIsRefusedEvenWithForce(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, b := newTestSitesWith(t, newClient)
		mustNoErr(t, a.markPrimary(ctx))

		// Aborted rather than FailedPrecondition, so the controller does not
		// retry with force and create a split brain.
		expectCode(t, b.markPrimary(ctx), codes.Aborted)
		expectCode(t, b.promote(ctx, true), codes.Aborted)
		if got := b.pgState(t, fabric); got != snapservice.StateDormant {
			t.Fatalf("site B state = %s, want DORMANT", got)
		}
	})
}

func TestUnplannedFailover(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, b := newTestSitesWith(t, newClient)
		mustNoErr(t, a.markPrimary(ctx))

		fabric.SetSiteDown(siteA, true)
		// The first promote fails with FailedPrecondition and the forced retry succeeds.
		expectCode(t, b.markPrimary(ctx), codes.FailedPrecondition)
		mustNoErr(t, b.markPrimaryWithForceRetry(ctx))
		if got := b.pgState(t, fabric); got != snapservice.StateActive {
			t.Fatalf("site B state = %s, want ACTIVE", got)
		}
		info, err := b.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_DEGRADED {
			t.Fatalf("site B info status = %s, want DEGRADED while site A is down", info.GetStatus())
		}

		// Site A comes back still active; the admin marks it secondary.
		fabric.SetSiteDown(siteA, false)
		mustNoErr(t, a.demote(ctx))
		ready, err := a.resync(ctx)
		mustNoErr(t, err)
		if !ready {
			t.Fatal("site A should be ready after re-protect")
		}
	})
}

func TestLocalSiteDownIsUnavailable(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, _ := newTestSitesWith(t, newClient)
		mustNoErr(t, a.markPrimary(ctx))
		fabric.SetSiteDown(siteA, true)
		_, err := a.info(ctx)
		expectCode(t, err, codes.Unavailable)
	})
}

func TestResyncNotReadyUntilInitialSync(t *testing.T) {
	ctx := context.Background()
	fabric, a, b := newTestSites(t, snapservice.WithAutoSync(false))
	mustNoErr(t, a.markPrimary(ctx))
	mustNoErr(t, b.demote(ctx))

	ready, err := b.resync(ctx)
	mustNoErr(t, err)
	if ready {
		t.Fatal("site B must not be ready before the initial sync")
	}
	fabric.Sync()
	ready, err = b.resync(ctx)
	mustNoErr(t, err)
	if !ready {
		t.Fatal("site B should be ready after sync")
	}
}

func TestResyncOnPrimaryFails(t *testing.T) {
	ctx := context.Background()
	_, a, _ := newTestSites(t)
	mustNoErr(t, a.markPrimary(ctx))
	_, err := a.resync(ctx)
	expectCode(t, err, codes.FailedPrecondition)
}

func TestPromoteWithRecoveredVolumeRegisteredBySnapService(t *testing.T) {
	ctx := context.Background()
	_, a, b := newTestSites(t, snapservice.WithRegisterRecovered(true))
	mustNoErr(t, a.markPrimary(ctx))
	mustNoErr(t, a.demote(ctx))
	// SnapService registered the volume, so CNS already has it.
	b.volumes.vols[volumeID] = &VolumeInfo{ID: volumeID, VolumeType: common.BlockVolumeType}
	mustNoErr(t, b.markPrimary(ctx))
	if len(b.volumes.registered) != 0 {
		t.Fatalf("site B registered %v, want none", b.volumes.registered)
	}
}

func TestDisableVolumeReplication(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, b := newTestSitesWith(t, newClient)
		disable := func(s *testSite) error {
			_, err := s.server.DisableVolumeReplication(ctx, &replication.DisableVolumeReplicationRequest{
				ReplicationSource: volumeSource(volumeID),
				Parameters:        s.params,
			})
			return err
		}

		// Nothing to disable yet.
		mustNoErr(t, disable(a))

		mustNoErr(t, a.markPrimary(ctx))
		// The secondary leaves its replica alone.
		mustNoErr(t, disable(b))
		if got := b.pgState(t, fabric); got != snapservice.StateDormant {
			t.Fatalf("site B state = %s, want DORMANT", got)
		}
		// The primary deletes the protection group and its replica.
		mustNoErr(t, disable(a))
		for _, site := range []string{siteA, siteB} {
			_, err := fabric.Site(site).GetProtectionGroup(ctx, protectionGroupName(volumeID))
			if err != snapservice.ErrNotFound {
				t.Fatalf("site %s: expected protection group to be deleted, got %v", site, err)
			}
		}
	})
}

func TestConcurrentOperationIsAborted(t *testing.T) {
	ctx := context.Background()
	_, a, _ := newTestSites(t)
	if !a.server.locks.TryAcquire(volumeID) {
		t.Fatal("failed to take lock")
	}
	defer a.server.locks.Release(volumeID)
	expectCode(t, a.markPrimary(ctx), codes.Aborted)
}

func TestVolumeGroupIsUnimplemented(t *testing.T) {
	ctx := context.Background()
	_, a, _ := newTestSites(t)
	_, err := a.server.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{
		ReplicationSource: &replication.ReplicationSource{
			Type: &replication.ReplicationSource_Volumegroup{
				Volumegroup: &replication.ReplicationSource_VolumeGroupSource{VolumeGroupId: "group"},
			},
		},
	})
	expectCode(t, err, codes.Unimplemented)
}

func TestGetReplicationDestinationInfo(t *testing.T) {
	_, a, _ := newTestSites(t)
	resp, err := a.server.GetReplicationDestinationInfo(context.Background(),
		&replication.GetReplicationDestinationInfoRequest{ReplicationSource: volumeSource(volumeID)})
	mustNoErr(t, err)
	if got := resp.GetReplicationDestination().GetVolume().GetVolumeId(); got != volumeID {
		t.Fatalf("destination volume = %q, want %q", got, volumeID)
	}
}

func TestParseSchedulingInterval(t *testing.T) {
	tests := map[string]time.Duration{"5m": 5 * time.Minute, "1h": time.Hour, "2d": 48 * time.Hour}
	for in, want := range tests {
		got, err := parseSchedulingInterval(in)
		if err != nil || got != want {
			t.Errorf("parseSchedulingInterval(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0m", "-1h", "xd", "1w"} {
		if _, err := parseSchedulingInterval(in); err == nil {
			t.Errorf("parseSchedulingInterval(%q) should fail", in)
		}
	}
}

func TestIdentityCapabilities(t *testing.T) {
	ids := NewIdentityServer("test")
	resp, err := ids.GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
	mustNoErr(t, err)
	var service, replicationCap bool
	for _, c := range resp.GetCapabilities() {
		if c.GetService().GetType() == identity.Capability_Service_CONTROLLER_SERVICE {
			service = true
		}
		if c.GetVolumeReplication().GetType() == identity.Capability_VolumeReplication_VOLUME_REPLICATION {
			replicationCap = true
		}
	}
	if !service || !replicationCap {
		t.Fatalf("missing capabilities: %v", resp.GetCapabilities())
	}
	probe, err := ids.Probe(context.Background(), &identity.ProbeRequest{})
	mustNoErr(t, err)
	if !probe.GetReady().GetValue() {
		t.Fatal("probe should report ready")
	}
}

func TestPromoteSkipsRegistrationWhenConfigured(t *testing.T) {
	ctx := context.Background()
	_, a, b := newTestSites(t)
	b.server = NewServer(b.server.newClient, b.volumes, WithSkipVolumeRegistration(true))
	mustNoErr(t, a.markPrimary(ctx))
	mustNoErr(t, a.demote(ctx))
	mustNoErr(t, b.markPrimary(ctx))
	if len(b.volumes.registered) != 0 {
		t.Fatalf("site B registered %v, want none", b.volumes.registered)
	}
}

func TestVolumeReplicationInfoReportsProblemsAsStatus(t *testing.T) {
	backends(t, func(t *testing.T, newClient siteFactory) {
		ctx := context.Background()
		fabric, a, _ := newTestSitesWith(t, newClient)

		// No protection group, for example after it was deleted outside Kubernetes.
		info, err := a.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_ERROR ||
			!strings.Contains(info.GetStatusMessage(), "does not exist") {
			t.Fatalf("missing protection group: got %s %q", info.GetStatus(), info.GetStatusMessage())
		}

		mustNoErr(t, a.markPrimary(ctx))
		info, err = a.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_HEALTHY ||
			!strings.Contains(info.GetStatusMessage(), "replicating") {
			t.Fatalf("healthy: got %s %q", info.GetStatus(), info.GetStatusMessage())
		}

		// The group was demoted outside Kubernetes while the VR is still primary.
		mustNoErr(t, a.demote(ctx))
		info, err = a.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_ERROR ||
			!strings.Contains(info.GetStatusMessage(), "DEMOTED") {
			t.Fatalf("demoted: got %s %q", info.GetStatus(), info.GetStatusMessage())
		}

		// The peer site is down.
		mustNoErr(t, a.markPrimary(ctx))
		fabric.SetSiteDown(siteB, true)
		info, err = a.info(ctx)
		mustNoErr(t, err)
		if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_DEGRADED {
			t.Fatalf("peer down: got %s %q", info.GetStatus(), info.GetStatusMessage())
		}
	})
}
