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

	"github.com/csi-addons/spec/lib/go/identity"
	"google.golang.org/protobuf/types/known/wrapperspb"

	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

// IdentityServer implements the csi-addons Identity service queried by the
// csi-addons sidecar.
type IdentityServer struct {
	identity.UnimplementedIdentityServer
	version string
}

var _ identity.IdentityServer = &IdentityServer{}

// NewIdentityServer returns a csi-addons IdentityServer.
func NewIdentityServer(version string) *IdentityServer {
	return &IdentityServer{version: version}
}

// GetIdentity returns the driver name and version.
func (s *IdentityServer) GetIdentity(ctx context.Context,
	req *identity.GetIdentityRequest) (*identity.GetIdentityResponse, error) {
	return &identity.GetIdentityResponse{
		Name:          csitypes.Name,
		VendorVersion: s.version,
	}, nil
}

// GetCapabilities advertises the controller service with volume replication.
func (s *IdentityServer) GetCapabilities(ctx context.Context,
	req *identity.GetCapabilitiesRequest) (*identity.GetCapabilitiesResponse, error) {
	return &identity.GetCapabilitiesResponse{
		Capabilities: []*identity.Capability{
			{
				Type: &identity.Capability_Service_{
					Service: &identity.Capability_Service{
						Type: identity.Capability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &identity.Capability_VolumeReplication_{
					VolumeReplication: &identity.Capability_VolumeReplication{
						Type: identity.Capability_VolumeReplication_VOLUME_REPLICATION,
					},
				},
			},
			{
				Type: &identity.Capability_VolumeReplication_{
					VolumeReplication: &identity.Capability_VolumeReplication{
						Type: identity.Capability_VolumeReplication_GET_REPLICATION_DESTINATION_INFO,
					},
				},
			},
		},
	}, nil
}

// Probe reports the service as ready.
func (s *IdentityServer) Probe(ctx context.Context,
	req *identity.ProbeRequest) (*identity.ProbeResponse, error) {
	return &identity.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
