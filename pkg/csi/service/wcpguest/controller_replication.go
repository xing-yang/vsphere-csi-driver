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

package wcpguest

import (
	"context"
	"fmt"

	"github.com/csi-addons/spec/lib/go/identity"
	csiaddonsreplication "github.com/csi-addons/spec/lib/go/replication"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/wcpguest/replication"
)

// NewCSIAddonsReplicationServers returns the csi-addons Identity and
// Replication services, which proxy to VolumeReplications on the Supervisor.
// It must be called after Init.
func (c *controller) NewCSIAddonsReplicationServers(ctx context.Context, version string) (
	identity.IdentityServer, csiaddonsreplication.ControllerServer, error) {
	if c.supervisorNamespace == "" {
		return nil, nil, fmt.Errorf("the Supervisor namespace is not known")
	}
	clients := func(ctx context.Context) (dynamic.Interface, clientset.Interface, clientset.Interface, error) {
		// ReloadConfiguration can replace these concurrently, so read each once.
		restClientConfig, supervisorClient, guestClient := c.restClientConfig, c.supervisorClient, c.guestClient
		if restClientConfig == nil || supervisorClient == nil || guestClient == nil {
			return nil, nil, nil, fmt.Errorf("the Supervisor or guest client is not initialized")
		}
		dyn, err := dynamic.NewForConfig(restClientConfig)
		if err != nil {
			return nil, nil, nil, err
		}
		return dyn, supervisorClient, guestClient, nil
	}
	// The same TKC label that pvCSI sets on Supervisor PVCs and snapshots.
	labels := map[string]string{}
	if c.tanzukubernetesClusterName != "" && c.guestClusterDist != "" {
		labels[fmt.Sprintf("%s/%s", c.tanzukubernetesClusterName, c.guestClusterDist)] = c.tanzukubernetesClusterUID
	}
	return replication.NewIdentityServer(version), replication.NewServer(replication.Config{
		Clients:             clients,
		SupervisorNamespace: c.supervisorNamespace,
		Labels:              labels,
	}), nil
}
