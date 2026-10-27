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

package hms

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FakeFabric is an in-memory set of paired HMS sites that follows the HMS
// behavior for CNS groups: configure creates a primary group and a secondary
// group with the same ID, the first full sync completes immediately, each
// OnlineSync completes one more cycle, and recovery commits the secondary
// group and stops replication. Reprotect is not supported, as in HMS.
type FakeFabric struct {
	mu     sync.Mutex
	sites  map[string]*fakeSite
	nextID int
}

type fakeSite struct {
	name     string
	outgoing map[string]*fakeGroup
	incoming map[string]*fakeGroup
	// replicaPeer, if set, makes every lookup of a missing secondary group
	// create one, as if the peer site had configured replication.
	replicaPeer      string
	replicaDatastore MoRef
}

type fakeGroup struct {
	id         string
	name       string
	status     string
	rpo        time.Duration
	generation int64
	peerSite   string
	datastore  MoRef
	volumeID   string
}

// NewFakeFabric returns a fabric in which all the given sites are paired.
func NewFakeFabric(sites ...string) *FakeFabric {
	f := &FakeFabric{sites: map[string]*fakeSite{}}
	for _, s := range sites {
		f.sites[s] = &fakeSite{name: s, outgoing: map[string]*fakeGroup{}, incoming: map[string]*fakeGroup{}}
	}
	return f
}

// Site returns a Client for the HMS of the given site.
func (f *FakeFabric) Site(name string) Client {
	return &fakeClient{fabric: f, site: name}
}

// ActAsReplicaSite makes site behave as the target of every volume: looking
// up a secondary group that does not exist creates it, replicated from peer
// with its replica on datastore. Use it to exercise the target-side flow on a
// single Supervisor.
func (f *FakeFabric) ActAsReplicaSite(site, peer string, datastore MoRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sites[site]; s != nil {
		s.replicaPeer = peer
		s.replicaDatastore = datastore
	}
}

// SetStatus overrides the status of a group on a site, for tests.
func (f *FakeFabric) SetStatus(site, name, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sites[site]
	if g := s.outgoing[name]; g != nil {
		g.status = status
	}
	if g := s.incoming[name]; g != nil {
		g.status = status
	}
}

type fakeClient struct {
	fabric *FakeFabric
	site   string
}

var _ Client = &fakeClient{}

func (c *fakeClient) LocalSite() string { return c.site }

func (c *fakeClient) local() (*fakeSite, error) {
	s := c.fabric.sites[c.site]
	if s == nil {
		return nil, fmt.Errorf("unknown site %q", c.site)
	}
	return s, nil
}

func (g *fakeGroup) toGroup() *Group {
	ref := MoRef{Type: "HmsPrimaryGroup", Value: g.id}
	return &Group{
		Ref: ref, Name: g.name, Status: g.status, RPO: g.rpo, PeerHealthy: true,
		Peer: &MoRef{Type: "HmsRemoteHmsServer", Value: g.peerSite},
	}
}

func (c *fakeClient) find(name string, outgoing bool) (*Group, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return nil, err
	}
	groups := s.incoming
	if outgoing {
		groups = s.outgoing
	}
	g := groups[name]
	if g == nil && !outgoing && s.replicaPeer != "" && strings.HasPrefix(name, "cns-") {
		c.fabric.nextID++
		g = &fakeGroup{id: fmt.Sprintf("GID-%d", c.fabric.nextID), name: name, status: StatusOK,
			rpo: 15 * time.Minute, generation: 1, peerSite: s.replicaPeer, datastore: s.replicaDatastore,
			volumeID: strings.TrimPrefix(name, "cns-")}
		s.incoming[name] = g
	}
	if g == nil {
		return nil, fmt.Errorf("group %q on site %q: %w", name, c.site, ErrNotFound)
	}
	out := g.toGroup()
	if !outgoing {
		out.Ref.Type = "HmsSecondaryGroup"
	}
	return out, nil
}

func (c *fakeClient) FindOutgoingGroup(_ context.Context, name string) (*Group, error) {
	return c.find(name, true)
}

func (c *fakeClient) FindIncomingGroup(_ context.Context, name string) (*Group, error) {
	return c.find(name, false)
}

func (c *fakeClient) ConfigureCnsVolumeReplication(_ context.Context, spec CnsVolumeReplicationSpec) (*Group, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return nil, err
	}
	peer := c.fabric.sites[spec.TargetSite]
	if peer == nil || spec.TargetSite == c.site {
		return nil, fmt.Errorf("site %q is not paired with %q: %w", spec.TargetSite, c.site, ErrNoPeer)
	}
	if spec.SourceDatastore.Value == "" {
		return nil, &Fault{Type: "InvalidArgument", Message: "groupSpec.sourceDatastoreRef"}
	}
	if spec.TargetDatastore.Value == "" {
		return nil, &Fault{Type: "InvalidArgument", Message: "groupSpec.targetDatastoreRef"}
	}
	if _, ok := s.outgoing[spec.Name]; ok {
		return nil, &Fault{Type: "ConfigureReplicationFault", Message: "group " + spec.Name + " already exists"}
	}
	for _, g := range s.outgoing {
		if g.volumeID == spec.VolumeID {
			return nil, &Fault{Type: "ConfigureReplicationFault",
				Message: "volume " + spec.VolumeID + " is already replicated"}
		}
	}
	c.fabric.nextID++
	id := fmt.Sprintf("GID-%d", c.fabric.nextID)
	rpo := ((spec.RPO + time.Minute - 1) / time.Minute) * time.Minute
	primary := &fakeGroup{id: id, name: spec.Name, status: StatusOK, rpo: rpo, generation: 1,
		peerSite: spec.TargetSite, datastore: spec.TargetDatastore, volumeID: spec.VolumeID}
	secondary := *primary
	secondary.peerSite = c.site
	s.outgoing[spec.Name] = primary
	peer.incoming[spec.Name] = &secondary
	return primary.toGroup(), nil
}

func (c *fakeClient) primary(group *Group) (*fakeSite, *fakeGroup, error) {
	s, err := c.local()
	if err != nil {
		return nil, nil, err
	}
	g := s.outgoing[group.Name]
	if g == nil || g.id != group.Ref.Value {
		return nil, nil, fmt.Errorf("primary group %q: %w", group.Name, ErrNotFound)
	}
	return s, g, nil
}

func (c *fakeClient) GetPolicyProgress(_ context.Context, group *Group) (*PolicyProgress, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.primary(group)
	if err != nil {
		return nil, err
	}
	if g.status == StatusRecovered {
		// The policy is unregistered after the secondary is recovered.
		return nil, nil
	}
	gen := g.generation
	d := time.Second
	bytes := int64(1 << 20)
	return &PolicyProgress{CurrentGeneration: &gen, LastSyncDuration: &d, LastSyncBytes: &bytes,
		Health: HealthIdle}, nil
}

func (c *fakeClient) OnlineSync(_ context.Context, group *Group) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.primary(group)
	if err != nil {
		return err
	}
	if g.status != StatusOK {
		return fmt.Errorf("group %q is %s: %w", g.name, g.status, ErrInvalidState)
	}
	g.generation++
	return nil
}

func (c *fakeClient) Unconfigure(_ context.Context, group *Group, _ bool) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, g, err := c.primary(group)
	if err != nil {
		return err
	}
	if peer := c.fabric.sites[g.peerSite]; peer != nil {
		delete(peer.incoming, g.name)
	}
	delete(s.outgoing, g.name)
	return nil
}

func (c *fakeClient) RecoverGroup(_ context.Context, group *Group, spec RecoverSpec) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return err
	}
	g := s.incoming[group.Name]
	if g == nil || g.id != group.Ref.Value {
		return fmt.Errorf("secondary group %q: %w", group.Name, ErrNotFound)
	}
	if spec.Type != RecoveryPlannedMigration && spec.Type != RecoveryDisaster {
		return &Fault{Type: "NotSupported", Message: "recovery type " + string(spec.Type)}
	}
	if spec.Folder.Value == "" || spec.Pool.Value == "" {
		return &Fault{Type: "InvalidArgument", Message: "folder and pool are required"}
	}
	if g.status == StatusRecovered {
		return &Fault{Type: "HmsAlreadyRecoveredFault", Message: "group " + g.name + " is already recovered"}
	}
	g.status = StatusRecovered
	// The primary unregisters its policy and stops replicating. HMS reports
	// the secondary's status on the primary too.
	if peer := c.fabric.sites[g.peerSite]; peer != nil {
		if p := peer.outgoing[g.name]; p != nil {
			p.status = StatusRecovered
		}
	}
	return nil
}

func (c *fakeClient) GroupDatastore(_ context.Context, group *Group) (MoRef, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return MoRef{}, err
	}
	for _, groups := range []map[string]*fakeGroup{s.outgoing, s.incoming} {
		if g := groups[group.Name]; g != nil {
			if g.datastore.Value == "" {
				return MoRef{}, fmt.Errorf("group %q has no datastore", group.Name)
			}
			return g.datastore, nil
		}
	}
	return MoRef{}, fmt.Errorf("group %q: %w", group.Name, ErrNotFound)
}
