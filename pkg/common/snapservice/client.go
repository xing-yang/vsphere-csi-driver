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
)

// Client is the SnapService API for one site. The REST API scopes protection
// groups by vSphere cluster; implementations resolve the Supervisor's clusters
// themselves. Long running operations are SnapService tasks; implementations
// wait for the task to finish before returning, bounded by the context deadline.
type Client interface {
	// LocalSite returns the name of the site this client talks to.
	LocalSite() string

	// IsSitePaired returns whether the local site is paired with the given site.
	IsSitePaired(ctx context.Context, site string) (bool, error)

	// GetProtectionGroup returns the protection group with the given name. It
	// returns ErrNotFound if it does not exist.
	GetProtectionGroup(ctx context.Context, name string) (*ProtectionGroup, error)

	// CreateProtectionGroup creates a protection group.
	CreateProtectionGroup(ctx context.Context, spec ProtectionGroupSpec) (*ProtectionGroup, error)

	// DeleteProtectionGroup deletes a protection group. Member disks are kept.
	DeleteProtectionGroup(ctx context.Context, id string) error

	// Promote makes the local protection group active. A reachable peer must
	// already be demoted (planned migration), otherwise ErrPeerActive is
	// returned even with force. An unreachable peer returns ErrPeerUnreachable
	// unless force is set (unplanned failover).
	Promote(ctx context.Context, id string, force bool) (*PromoteResult, error)

	// Demote stops local protection and replication for the protection group.
	Demote(ctx context.Context, id string) error

	// Activate re-protects a demoted protection group so that it becomes a
	// replica of the active peer.
	Activate(ctx context.Context, id string) error
}
