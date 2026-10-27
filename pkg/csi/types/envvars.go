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

	// Volume replication (WCP). The variables below are marked with the
	// replication backend that reads them: "both", "fake" or "hms". With the
	// hms backend, most settings come from the VolumeReplicationClass
	// parameters and its secret, not from these variables.

	// EnvVarCSIAddonsEndpoint specifies the unix socket on which the controller
	// serves the csi-addons Identity and Replication services. Used by both
	// backends; replication is served only when it is set.
	EnvVarCSIAddonsEndpoint = "CSI_ADDONS_ENDPOINT"

	// EnvVarReplicationBackend selects the volume replication backend: "fake"
	// (in-memory HMS with two simulated sites, the default when unset) or "hms"
	// (the HMS of the local DPVA). Used by both backends.
	EnvVarReplicationBackend = "X_CSI_REPLICATION_BACKEND"

	// EnvVarReplicationFakeSite is the site name of this Supervisor. Fake
	// backend only; defaults to "site-a". The hms backend ignores it.
	EnvVarReplicationFakeSite = "X_CSI_REPLICATION_FAKE_SITE"

	// EnvVarReplicationFakePeerSite is the site paired with this Supervisor.
	// Fake backend only; defaults to "site-b". With the hms backend, the peer is
	// the VolumeReplicationClass parameter "targetSite".
	EnvVarReplicationFakePeerSite = "X_CSI_REPLICATION_FAKE_PEER_SITE"

	// EnvVarReplicationFakeRole is "source" (default) or "target". With
	// "target" the fake backend acts as the replica site of every volume, so
	// that promote can be tested on a single Supervisor. Fake backend only.
	EnvVarReplicationFakeRole = "X_CSI_REPLICATION_FAKE_ROLE"

	// EnvVarReplicationFakeDatastore is the datastore managed object ID that
	// the fake backend reports for replicas in the target role. Fake backend
	// only.
	EnvVarReplicationFakeDatastore = "X_CSI_REPLICATION_FAKE_DATASTORE"

	// EnvVarReplicationEndpoint is the HMS endpoint of this site, for example
	// "https://<dpva>/vrms". hms backend only; it is a fallback for when the
	// VolumeReplicationClass secret does not set "hmsEndpoint".
	EnvVarReplicationEndpoint = "X_CSI_REPLICATION_ENDPOINT"

	// EnvVarReplicationSkipVolumeRegistration, when "true", makes PromoteVolume
	// recover the replica but not register it with CNS under the source volume
	// ID. Used by both backends, for testing only: for example with the fake
	// backend, or with hms on a Supervisor where the
	// supports_vsphere_dp_lp_modern_app capability is not activated.
	EnvVarReplicationSkipVolumeRegistration = "X_CSI_REPLICATION_SKIP_VOLUME_REGISTRATION"
)
