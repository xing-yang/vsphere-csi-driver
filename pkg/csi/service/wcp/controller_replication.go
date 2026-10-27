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

package wcp

import (
	"context"
	"os"
	"strconv"

	"github.com/csi-addons/spec/lib/go/identity"
	csiaddonsreplication "github.com/csi-addons/spec/lib/go/replication"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/wcp/replication"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

// NewCSIAddonsReplicationServers returns the csi-addons Identity and
// Replication services. It must be called after Init.
func (c *controller) NewCSIAddonsReplicationServers(ctx context.Context, version string) (
	identity.IdentityServer, csiaddonsreplication.ControllerServer, error) {
	log := logger.GetLogger(ctx)
	vcHost := func() string { return c.manager.VcenterConfig.Host.String() }
	newClient, err := replication.NewClientFactoryFromEnv(ctx, vcHost)
	if err != nil {
		return nil, nil, err
	}
	volumes := replication.NewCNSVolumeBackend(func() *common.Manager { return c.manager }, c.k8sClient)
	skip, _ := strconv.ParseBool(os.Getenv(csitypes.EnvVarReplicationSkipVolumeRegistration))
	if skip {
		log.Warnf("%s is set: promoted volumes are not registered with CNS",
			csitypes.EnvVarReplicationSkipVolumeRegistration)
	}
	return replication.NewIdentityServer(version),
		replication.NewServer(newClient, volumes, replication.WithSkipVolumeRegistration(skip)), nil
}
