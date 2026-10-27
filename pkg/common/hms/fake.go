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
// group and stops replication. Reprotect reverses a recovered group into the
// original volume and removes the original group; it completes at once
// unless HoldReprotects is set.
type FakeFabric struct {
	mu     sync.Mutex
	sites  map[string]*fakeSite
	nextID int
	// holdReprotects leaves started reprotects in progress until
	// FinishReprotects.
	holdReprotects bool
	pending        []func()
}

type fakeSite struct {
	name     string
	outgoing map[string]*fakeGroup
	incoming map[string]*fakeGroup
	// replicaPeer, if set, makes every lookup of a missing secondary group
	// create one, as if the peer site had configured replication.
	replicaPeer      string
	replicaDatastore MoRef
	// operations are the reprotect operations run on this site.
	operations map[string]*ReprotectOperation
}

type fakeGroup struct {
	id         string
	name       string
	status     string
	rpo        time.Duration
	generation int64
	peerSite   string
	// datastore is the target datastore of the group; sourceDatastore is
	// where the source volume is.
	datastore       MoRef
	sourceDatastore MoRef
	volumeID        string
	// disk is the replica disk of a secondary group.
	disk DatastorePath
	// checkpoint and checkpointState track the optimized reprotect baseline:
	// prepared and sealed on the primary, sealed and tracking on the secondary.
	checkpoint      string
	checkpointState string
	boundVolume     string
}

// NewFakeFabric returns a fabric in which all the given sites are paired.
func NewFakeFabric(sites ...string) *FakeFabric {
	f := &FakeFabric{sites: map[string]*fakeSite{}}
	for _, s := range sites {
		f.sites[s] = &fakeSite{name: s, outgoing: map[string]*fakeGroup{}, incoming: map[string]*fakeGroup{},
			operations: map[string]*ReprotectOperation{}}
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

// HoldReprotects leaves reprotects that start from now on in progress until
// FinishReprotects is called.
func (f *FakeFabric) HoldReprotects(hold bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdReprotects = hold
}

// FinishReprotects completes the reprotects held by HoldReprotects.
func (f *FakeFabric) FinishReprotects() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, finish := range f.pending {
		finish()
	}
	f.pending = nil
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
		id := fmt.Sprintf("GID-%d", c.fabric.nextID)
		g = &fakeGroup{id: id, name: name, status: StatusOK,
			rpo: 15 * time.Minute, generation: 1, peerSite: s.replicaPeer, datastore: s.replicaDatastore,
			volumeID: strings.TrimPrefix(name, "cns-"), disk: placeholderDisk(id, s.replicaDatastore)}
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
		peerSite: spec.TargetSite, datastore: spec.TargetDatastore, sourceDatastore: spec.SourceDatastore,
		volumeID: spec.VolumeID}
	secondary := *primary
	secondary.peerSite = c.site
	secondary.disk = placeholderDisk(id, spec.TargetDatastore)
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
	if g.checkpointState == "sealed" {
		g.checkpointState = "tracking"
	}
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

// placeholderDisk is where HMS creates the replica disk of a new secondary group.
func placeholderDisk(groupID string, datastore MoRef) DatastorePath {
	return DatastorePath{Datastore: datastore, Path: "hbr-pe-gid-" + groupID, FileName: "cnsVolume.vmdk"}
}

func (c *fakeClient) secondary(group *Group) (*fakeSite, *fakeGroup, error) {
	s, err := c.local()
	if err != nil {
		return nil, nil, err
	}
	g := s.incoming[group.Name]
	if g == nil || g.id != group.Ref.Value {
		return nil, nil, fmt.Errorf("secondary group %q: %w", group.Name, ErrNotFound)
	}
	return s, g, nil
}

func invalidOperation(operationID string) error {
	return &Fault{Type: "InvalidArgument", Message: "operationId " + operationID}
}

func (c *fakeClient) PrepareRecovery(_ context.Context, group *Group, operationID string) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.primary(group)
	if err != nil {
		return err
	}
	if g.checkpoint != "" {
		if g.checkpoint != operationID {
			return invalidOperation(operationID)
		}
		return nil
	}
	if g.status != StatusOK {
		return fmt.Errorf("group %q is %s: %w", g.name, g.status, ErrInvalidState)
	}
	g.generation++
	g.checkpoint, g.checkpointState = operationID, "prepared"
	return nil
}

func (c *fakeClient) SealRecovery(_ context.Context, group *Group, operationID string) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.primary(group)
	if err != nil {
		return err
	}
	if g.checkpoint != operationID {
		return invalidOperation(operationID)
	}
	g.checkpointState = "sealed"
	if peer := c.fabric.sites[g.peerSite]; peer != nil {
		if sec := peer.incoming[g.name]; sec != nil && sec.status != StatusRecovered {
			sec.checkpoint, sec.checkpointState = operationID, "sealed"
		}
	}
	return nil
}

func (c *fakeClient) RecoveredDisk(_ context.Context, group *Group) (*DatastorePath, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.secondary(group)
	if err != nil {
		return nil, err
	}
	if g.status != StatusRecovered {
		return nil, &Fault{Type: "GroupNotRecovered", Message: "group " + g.name + " is not recovered"}
	}
	disk := g.disk
	return &disk, nil
}

func (c *fakeClient) BindRecoveredVolume(_ context.Context, group *Group, volumeID string, datastore MoRef) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	_, g, err := c.secondary(group)
	if err != nil {
		return err
	}
	if g.status != StatusRecovered {
		return &Fault{Type: "GroupNotRecovered", Message: "group " + g.name + " is not recovered"}
	}
	if volumeID != g.volumeID || datastore != g.disk.Datastore {
		return &Fault{Type: "InvalidArgument", Message: "spec.recoveredVolumeUuid"}
	}
	if g.boundVolume != "" && g.boundVolume != volumeID {
		return &Fault{Type: "InvalidArgument", Message: "volumeUuid"}
	}
	g.boundVolume = volumeID
	return nil
}

// StartReprotect creates the reverse group: a primary group on this site that
// replicates the recovered volume into the original volume on the peer site.
// The original primary group and the recovered group are then removed.
func (c *fakeClient) StartReprotect(_ context.Context, group *Group, spec ReprotectSpec) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, g, err := c.secondary(group)
	if err != nil {
		return err
	}
	if g.status != StatusRecovered {
		return &Fault{Type: "GroupNotRecovered", Message: "group " + g.name + " is not recovered"}
	}
	if spec.VolumeID != g.volumeID || spec.Datastore != g.disk.Datastore {
		return &Fault{Type: "InvalidArgument", Message: "spec.recoveredVolumeUuid"}
	}
	if op := s.operations[spec.OperationID]; op != nil {
		if op.OriginalGroupID != g.id {
			return invalidOperation(spec.OperationID)
		}
		return nil
	}
	origin := c.fabric.sites[g.peerSite]
	if origin == nil || origin.outgoing[g.name] == nil {
		return &Fault{Type: "InvalidArgument", Message: "original group of " + g.name + " not found"}
	}
	original := origin.outgoing[g.name]
	mode := "fullSync"
	if !spec.ForceFullSync && g.checkpointState == "tracking" && g.checkpoint == spec.OperationID &&
		g.boundVolume == spec.VolumeID {
		mode = "optimized"
	}
	reverseID := g.id + "-r"
	op := &ReprotectOperation{OperationID: spec.OperationID, OriginalGroupID: g.id, Phase: "configuring",
		SynchronizationMode: "pending"}
	s.operations[spec.OperationID] = op
	finish := func() {
		s.outgoing[g.name] = &fakeGroup{id: reverseID, name: g.name, status: StatusOK, rpo: g.rpo, generation: 1,
			peerSite: origin.name, datastore: original.sourceDatastore, sourceDatastore: spec.Datastore,
			volumeID: spec.VolumeID}
		origin.incoming[g.name] = &fakeGroup{id: reverseID, name: g.name, status: StatusOK, rpo: g.rpo,
			generation: 1, peerSite: s.name, datastore: original.sourceDatastore,
			sourceDatastore: spec.Datastore, volumeID: spec.VolumeID,
			// The original volume is adopted as the replica disk.
			disk: DatastorePath{Datastore: original.sourceDatastore, Path: "fcd", FileName: spec.VolumeID + ".vmdk"}}
		delete(origin.outgoing, g.name)
		delete(s.incoming, g.name)
		op.Phase = ReprotectPhaseComplete
		op.SynchronizationMode = mode
		op.ReverseGroup = &MoRef{Type: "HmsPrimaryGroup", Value: reverseID}
	}
	if c.fabric.holdReprotects {
		c.fabric.pending = append(c.fabric.pending, finish)
	} else {
		finish()
	}
	return nil
}

func (c *fakeClient) ReprotectOperation(_ context.Context, operationID string) (*ReprotectOperation, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return nil, err
	}
	op := s.operations[operationID]
	if op == nil {
		return nil, fmt.Errorf("reprotect operation %s: %w", operationID, ErrNotFound)
	}
	out := *op
	return &out, nil
}

func (c *fakeClient) ResumeReprotect(_ context.Context, _ *Group, operationID string) error {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	s, err := c.local()
	if err != nil {
		return err
	}
	op := s.operations[operationID]
	if op == nil {
		return invalidOperation(operationID)
	}
	op.Error = ""
	return nil
}

// Peer returns the client of another site of the fabric.
func (c *fakeClient) Peer(_ context.Context) (Client, error) {
	c.fabric.mu.Lock()
	defer c.fabric.mu.Unlock()
	var names []string
	for name := range c.fabric.sites {
		if name != c.site {
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		return nil, fmt.Errorf("%w: site %q has %d peers in the fake fabric", ErrPeerUnreachable, c.site, len(names))
	}
	return &fakeClient{fabric: c.fabric, site: names[0]}, nil
}
