/*
Copyright 2018 The Kubernetes Authors.

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

package types

import (
	cnsconfig "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
)

const (
	// EnvSupervisorClientQPS  is the QPS for all clients to the supervisor cluster API server
	EnvSupervisorClientQPS = "SUPERVISOR_CLIENT_QPS"

	// EnvSupervisorClientBurst  is the Burst for all clients to the supervisor cluster API server
	EnvSupervisorClientBurst = "SUPERVISOR_CLIENT_BURST"

	// EnvInClusterClientQPS is the QPS for all clients to the API server
	EnvInClusterClientQPS = "INCLUSTER_CLIENT_QPS"

	// EnvInClusterClientBurst is the Burst for all clients to the API server
	EnvInClusterClientBurst = "INCLUSTER_CLIENT_BURST"

	// EnvVarEndpoint specifies the CSI endpoint for CSI driver.
	EnvVarEndpoint = "CSI_ENDPOINT"

	// EnvVarNamespace specifies the namespace in which CSI driver is installed.
	EnvVarNamespace = cnsconfig.EnvCSINamespace

	// EnvVarMode is the name of the environment variable used to specify
	// the service mode of the plugin. Valid values are:
	// * controller
	// * node
	//
	// Depending on the value, either controller and node service will be
	// activated (The identity service is always activated).
	EnvVarMode = "X_CSI_MODE"

	// EnvVarCSIAddonsEndpoint specifies the unix socket on which the controller
	// serves the csi-addons Identity and Replication services.
	EnvVarCSIAddonsEndpoint = "CSI_ADDONS_ENDPOINT"

	// EnvVarReplicationBackend selects the SnapService backend for volume
	// replication: "fake" (in-memory) or "rest".
	EnvVarReplicationBackend = "X_CSI_REPLICATION_BACKEND"

	// EnvVarReplicationSite is the SnapService site name of this Supervisor.
	EnvVarReplicationSite = "X_CSI_REPLICATION_SITE"

	// EnvVarReplicationPeerSite is the SnapService site paired with this
	// Supervisor. Used by the fake backend.
	EnvVarReplicationPeerSite = "X_CSI_REPLICATION_PEER_SITE"

	// EnvVarReplicationEndpoint is the SnapService REST endpoint, used by the
	// rest backend when the VolumeReplicationClass secret does not set one.
	EnvVarReplicationEndpoint = "X_CSI_REPLICATION_ENDPOINT"

	// EnvVarReplicationCluster is the vSphere cluster that owns protection
	// groups, used by the rest backend when the secret does not set one.
	EnvVarReplicationCluster = "X_CSI_REPLICATION_CLUSTER"

	// EnvVarReplicationSkipVolumeRegistration skips registering promoted volumes
	// with CNS. Only for testing with the fake SnapService.
	EnvVarReplicationSkipVolumeRegistration = "X_CSI_REPLICATION_SKIP_VOLUME_REGISTRATION"
)
