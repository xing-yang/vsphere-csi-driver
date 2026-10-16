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

package snapservice

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FakeFabric is an in-memory model of SnapService across paired sites. It is
// used by unit tests and by the fake replication backend until the SnapService
// 9.2 API is available. State transitions follow the volume replication POC
// design and are assumptions, not observed SnapService behavior.
type FakeFabric struct {
	mu    sync.Mutex
	sites map[string]*fakeSite
	pairs map[string]map[string]bool
	// autoSync marks replicas as fully synced as soon as they are created or
	// re-protected. When false, Sync must be called.
	autoSync bool
	// registerRecovered makes Promote report the recovered FCDs as already
	// registered with their source ID. When false, callers must register them.
	registerRecovered bool
	now               func() time.Time
}

type fakeSite struct {
	name string
	down bool
	// pgs is keyed by protection group name.
	pgs map[string]*ProtectionGroup
}

// FakeOption configures a FakeFabric.
type FakeOption func(*FakeFabric)

// WithAutoSync sets whether replicas are synced immediately. Default true.
func WithAutoSync(autoSync bool) FakeOption {
	return func(f *FakeFabric) { f.autoSync = autoSync }
}

// WithRegisterRecovered sets whether promote registers recovered FCDs with the
// source ID. Default false.
func WithRegisterRecovered(register bool) FakeOption {
	return func(f *FakeFabric) { f.registerRecovered = register }
}

// WithClock sets the clock used for replica timestamps.
func WithClock(now func() time.Time) FakeOption {
	return func(f *FakeFabric) { f.now = now }
}

// NewFakeFabric creates a fake with the given sites, all paired with each other.
func NewFakeFabric(sites []string, opts ...FakeOption) *FakeFabric {
	f := &FakeFabric{
		sites:    make(map[string]*fakeSite),
		pairs:    make(map[string]map[string]bool),
		autoSync: true,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(f)
	}
	for _, s := range sites {
		f.sites[s] = &fakeSite{name: s, pgs: make(map[string]*ProtectionGroup)}
		f.pairs[s] = make(map[string]bool)
	}
	for _, a := range sites {
		for _, b := range sites {
			if a != b {
				f.pairs[a][b] = true
			}
		}
	}
	return f
}

// Site returns a Client for the given site.
func (f *FakeFabric) Site(name string) Client {
	return &fakeClient{fabric: f, site: name}
}

// SetSiteDown simulates a site outage.
func (f *FakeFabric) SetSiteDown(name string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sites[name]; ok {
		s.down = down
	}
}

// Sync completes outstanding replication for every active protection group
// whose peer is reachable and dormant.
func (f *FakeFabric) Sync() {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for _, s := range f.sites {
		if s.down {
			continue
		}
		for _, pg := range s.pgs {
			if pg.State != StateActive {
				continue
			}
			peer, peerSite := f.peerLocked(s.name, pg)
			if peer == nil || peerSite.down || peer.State != StateDormant {
				continue
			}
			pg.LastReplicaTime = &now
			peer.LastReplicaTime = &now
			peer.InitialSyncDone = true
		}
	}
}

// ListProtectionGroups returns the protection groups on a site.
func (f *FakeFabric) ListProtectionGroups(site string) ([]*ProtectionGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(site)
	if err != nil {
		return nil, err
	}
	out := make([]*ProtectionGroup, 0, len(s.pgs))
	for _, pg := range s.pgs {
		out = append(out, f.observedLocked(site, pg))
	}
	return out, nil
}

// PairedSites returns the sites paired with the given site.
func (f *FakeFabric) PairedSites(site string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.localLocked(site); err != nil {
		return nil, err
	}
	var out []string
	for peer, paired := range f.pairs[site] {
		if paired {
			out = append(out, peer)
		}
	}
	return out, nil
}

// HasSite returns whether the site exists.
func (f *FakeFabric) HasSite(site string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sites[site]
	return ok
}

// peerLocked returns the protection group linked to pg on another site.
func (f *FakeFabric) peerLocked(site string, pg *ProtectionGroup) (*ProtectionGroup, *fakeSite) {
	for name, s := range f.sites {
		if name == site {
			continue
		}
		if p, ok := s.pgs[pg.Name]; ok && p.GlobalID == pg.GlobalID {
			return p, s
		}
	}
	return nil, nil
}

// localLocked returns the site a client talks to.
func (f *FakeFabric) localLocked(name string) (*fakeSite, error) {
	s, ok := f.sites[name]
	if !ok {
		return nil, fmt.Errorf("unknown site %q", name)
	}
	if s.down {
		return nil, fmt.Errorf("site %q: %w", name, ErrSiteUnavailable)
	}
	return s, nil
}

// remoteLocked returns a peer site.
func (f *FakeFabric) remoteLocked(name string) (*fakeSite, error) {
	s, ok := f.sites[name]
	if !ok {
		return nil, fmt.Errorf("unknown site %q", name)
	}
	if s.down {
		return nil, fmt.Errorf("site %q: %w", name, ErrPeerUnreachable)
	}
	return s, nil
}

func (f *FakeFabric) pgByIDLocked(s *fakeSite, id string) (*ProtectionGroup, error) {
	for _, pg := range s.pgs {
		if pg.ID == id {
			return pg, nil
		}
	}
	return nil, ErrNotFound
}

// observedLocked returns a copy of pg with derived fields filled in.
func (f *FakeFabric) observedLocked(site string, pg *ProtectionGroup) *ProtectionGroup {
	out := *pg
	out.TargetEntities.FCDs = append([]string(nil), pg.TargetEntities.FCDs...)
	out.TargetEntities.VMs = append([]string(nil), pg.TargetEntities.VMs...)
	if pg.Replication != nil {
		r := *pg.Replication
		out.Replication = &r
	}
	peer, peerSite := f.peerLocked(site, pg)
	switch pg.State {
	case StateActive, StatePaused:
		out.ReplicationHealthy = peer != nil && !peerSite.down && peer.State == StateDormant
	case StateDormant:
		out.ReplicationHealthy = peer != nil && !peerSite.down && peer.State == StateActive
	default:
		out.ReplicationHealthy = false
	}
	return &out
}

type fakeClient struct {
	fabric *FakeFabric
	site   string
}

var _ Client = &fakeClient{}

func (c *fakeClient) LocalSite() string {
	return c.site
}

func (c *fakeClient) IsSitePaired(ctx context.Context, site string) (bool, error) {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.localLocked(c.site); err != nil {
		return false, err
	}
	return f.pairs[c.site][site], nil
}

func (c *fakeClient) GetProtectionGroup(ctx context.Context, name string) (*ProtectionGroup, error) {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return nil, err
	}
	pg, ok := s.pgs[name]
	if !ok {
		return nil, ErrNotFound
	}
	return f.observedLocked(c.site, pg), nil
}

func (c *fakeClient) CreateProtectionGroup(ctx context.Context,
	spec ProtectionGroupSpec) (*ProtectionGroup, error) {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return nil, err
	}
	if _, ok := s.pgs[spec.Name]; ok {
		return nil, ErrAlreadyExists
	}
	if spec.Replication == nil || spec.Replication.TargetSite == "" {
		return nil, fmt.Errorf("replication target site is required")
	}
	target := spec.Replication.TargetSite
	if !f.pairs[c.site][target] {
		return nil, ErrNoSitePair
	}
	targetSite, err := f.remoteLocked(target)
	if err != nil {
		return nil, err
	}
	if _, ok := targetSite.pgs[spec.Name]; ok {
		return nil, fmt.Errorf("site %q: %w", target, ErrAlreadyExists)
	}

	now := f.now()
	pg := &ProtectionGroup{
		ID:             uuid.NewString(),
		GlobalID:       uuid.NewString(),
		Name:           spec.Name,
		State:          StateActive,
		TargetEntities: spec.TargetEntities,
		Replication:    spec.Replication,
	}
	replicaPolicy := *spec.Replication
	replicaPolicy.TargetSite = c.site
	replica := &ProtectionGroup{
		ID:             uuid.NewString(),
		GlobalID:       pg.GlobalID,
		Name:           spec.Name,
		State:          StateDormant,
		TargetEntities: spec.TargetEntities,
		Replication:    &replicaPolicy,
	}
	if f.autoSync {
		pg.LastReplicaTime = &now
		replica.LastReplicaTime = &now
		replica.InitialSyncDone = true
	}
	pg.InitialSyncDone = true
	s.pgs[pg.Name] = pg
	targetSite.pgs[replica.Name] = replica
	return f.observedLocked(c.site, pg), nil
}

func (c *fakeClient) DeleteProtectionGroup(ctx context.Context, id string) error {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return err
	}
	pg, err := f.pgByIDLocked(s, id)
	if err != nil {
		return err
	}
	// Deleting the source also removes a reachable dormant replica.
	if peer, peerSite := f.peerLocked(c.site, pg); peer != nil && !peerSite.down &&
		peer.State == StateDormant {
		delete(peerSite.pgs, peer.Name)
	}
	delete(s.pgs, pg.Name)
	return nil
}

func (c *fakeClient) Promote(ctx context.Context, id string, force bool) (*PromoteResult, error) {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return nil, err
	}
	pg, err := f.pgByIDLocked(s, id)
	if err != nil {
		return nil, err
	}
	switch pg.State {
	case StateActive:
		// Idempotent.
	case StateDormant, StateDemoted, StateRecovery:
		peer, peerSite := f.peerLocked(c.site, pg)
		peerReachable := peer != nil && !peerSite.down
		if peerReachable && (peer.State == StateActive || peer.State == StatePaused) {
			// Force must not create a split brain with a live peer.
			return nil, ErrPeerActive
		}
		if !peerReachable && !force {
			return nil, ErrPeerUnreachable
		}
		pg.State = StateActive
		if peer != nil {
			pg.Replication.TargetSite = peerSite.name
		}
	default:
		return nil, fmt.Errorf("promote from %s: %w", pg.State, ErrInvalidState)
	}
	res := &PromoteResult{}
	for _, fcd := range pg.TargetEntities.FCDs {
		d := RecoveredDisk{
			SourceID:    fcd,
			DiskURLPath: fmt.Sprintf("ds:///vmfs/volumes/vsan:%s/%s.vmdk", c.site, fcd),
		}
		if f.registerRecovered {
			d.RecoveredID = fcd
		}
		res.RecoveredDisks = append(res.RecoveredDisks, d)
	}
	return res, nil
}

func (c *fakeClient) Demote(ctx context.Context, id string) error {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return err
	}
	pg, err := f.pgByIDLocked(s, id)
	if err != nil {
		return err
	}
	switch pg.State {
	case StateDemoted, StateDormant:
		return nil
	case StateActive, StatePaused:
		// Demote ships a final delta to a reachable dormant peer.
		if peer, peerSite := f.peerLocked(c.site, pg); peer != nil && !peerSite.down &&
			peer.State == StateDormant {
			now := f.now()
			peer.LastReplicaTime = &now
			peer.InitialSyncDone = true
		}
		pg.State = StateDemoted
		return nil
	default:
		return fmt.Errorf("demote from %s: %w", pg.State, ErrInvalidState)
	}
}

func (c *fakeClient) Activate(ctx context.Context, id string) error {
	f := c.fabric
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.localLocked(c.site)
	if err != nil {
		return err
	}
	pg, err := f.pgByIDLocked(s, id)
	if err != nil {
		return err
	}
	switch pg.State {
	case StateDormant:
		return nil
	case StateDemoted:
		peer, peerSite := f.peerLocked(c.site, pg)
		if peer == nil || peerSite.down {
			return ErrPeerUnreachable
		}
		if peer.State != StateActive {
			return fmt.Errorf("activate requires an active peer, peer is %s: %w", peer.State, ErrInvalidState)
		}
		pg.State = StateDormant
		pg.InitialSyncDone = false
		if f.autoSync {
			now := f.now()
			pg.LastReplicaTime = &now
			pg.InitialSyncDone = true
		}
		return nil
	default:
		return fmt.Errorf("activate from %s: %w", pg.State, ErrInvalidState)
	}
}
