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
	"fmt"
	"os"
	"strconv"
	"sync"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/hms"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

const (
	// BackendFake is an in-memory HMS fabric. Its state is lost when the
	// controller restarts, and the peer site exists only inside the process.
	BackendFake = "fake"
	// BackendHMS talks to the HMS of the local site.
	BackendHMS = "hms"

	fakeRoleSource = "source"
	fakeRoleTarget = "target"

	defaultFakeSite     = "site-a"
	defaultFakePeerSite = "site-b"
)

// Keys of the VolumeReplicationClass secret for the hms backend.
const (
	// SecretKeyHMSEndpoint is the local HMS, for example "https://dpva/vrms".
	// Falls back to X_CSI_REPLICATION_ENDPOINT.
	SecretKeyHMSEndpoint = "hmsEndpoint"
	// SecretKeyVCHost is the local vCenter whose STS issues HMS login tokens.
	// Defaults to the vCenter of the driver.
	SecretKeyVCHost      = "vcHost"
	SecretKeySSOUser     = "ssoUser"
	SecretKeySSOPassword = "ssoPassword"
	// SecretKeyPeerVCHost, SecretKeyPeerSSOUser and SecretKeyPeerSSOPassword
	// log in to the peer HMS. They are needed on the source site to enable
	// and disable replication.
	SecretKeyPeerVCHost      = "peerVcHost"
	SecretKeyPeerSSOUser     = "peerSsoUser"
	SecretKeyPeerSSOPassword = "peerSsoPassword"
	// SecretKeyPeerHMSEndpoint is the HMS of the peer site, for example
	// "https://peer-dpva/vrms". Resync after a failover runs the reprotect
	// there, on the site of the recovered group, with the peer credentials.
	SecretKeyPeerHMSEndpoint    = "peerHmsEndpoint"
	SecretKeyInsecureSkipVerify = "insecureSkipVerify"
	// SecretKeyHMSNamespace and SecretKeyHMSVersion override the SOAP
	// namespace and version of the HMS VMODL.
	SecretKeyHMSNamespace = "hmsNamespace"
	SecretKeyHMSVersion   = "hmsVersion"
)

// NewClientFactoryFromEnv returns the HMS client factory selected by
// X_CSI_REPLICATION_BACKEND. defaultVCHost returns the vCenter of the driver.
func NewClientFactoryFromEnv(ctx context.Context, defaultVCHost func() string) (ClientFactory, error) {
	log := logger.GetLogger(ctx)
	backend := os.Getenv(csitypes.EnvVarReplicationBackend)
	if backend == "" {
		backend = BackendFake
	}
	switch backend {
	case BackendFake:
		site := envOrDefault(csitypes.EnvVarReplicationFakeSite, defaultFakeSite)
		peer := envOrDefault(csitypes.EnvVarReplicationFakePeerSite, defaultFakePeerSite)
		if site == peer {
			return nil, fmt.Errorf("%s and %s must differ, both are %q",
				csitypes.EnvVarReplicationFakeSite, csitypes.EnvVarReplicationFakePeerSite, site)
		}
		fabric := hms.NewFakeFabric(site, peer)
		switch role := envOrDefault(csitypes.EnvVarReplicationFakeRole, fakeRoleSource); role {
		case fakeRoleSource:
		case fakeRoleTarget:
			ds := hms.MoRef{Type: "Datastore", Value: os.Getenv(csitypes.EnvVarReplicationFakeDatastore)}
			fabric.ActAsReplicaSite(site, peer, ds)
		default:
			return nil, fmt.Errorf("unsupported %s %q", csitypes.EnvVarReplicationFakeRole, role)
		}
		log.Warnf("Volume replication uses an in-memory fake HMS (site %q paired with %q, role %s); "+
			"replication state is lost on restart", site, peer,
			envOrDefault(csitypes.EnvVarReplicationFakeRole, fakeRoleSource))
		client := fabric.Site(site)
		return func(ctx context.Context, secrets map[string]string) (hms.Client, error) {
			return client, nil
		}, nil
	case BackendHMS:
		log.Infof("Volume replication uses the HMS backend")
		return newHMSClientFactory(defaultVCHost), nil
	default:
		return nil, fmt.Errorf("unsupported %s %q", csitypes.EnvVarReplicationBackend, backend)
	}
}

// newHMSClientFactory returns a factory that builds HMS clients from the
// VolumeReplicationClass secret and caches them so sessions are reused.
func newHMSClientFactory(defaultVCHost func() string) ClientFactory {
	var (
		mu      sync.Mutex
		clients = map[hms.Config]*hms.SOAPClient{}
	)
	return func(ctx context.Context, secrets map[string]string) (hms.Client, error) {
		cfg, err := hmsConfig(secrets, defaultVCHost)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[cfg]; ok {
			return c, nil
		}
		c, err := hms.NewSOAPClient(cfg)
		if err != nil {
			return nil, err
		}
		clients[cfg] = c
		return c, nil
	}
}

func hmsConfig(secrets map[string]string, defaultVCHost func() string) (hms.Config, error) {
	cfg := hms.Config{
		Endpoint:  secrets[SecretKeyHMSEndpoint],
		Namespace: secrets[SecretKeyHMSNamespace],
		Version:   secrets[SecretKeyHMSVersion],
		Local: hms.Credentials{
			VCHost:   secrets[SecretKeyVCHost],
			User:     secrets[SecretKeySSOUser],
			Password: secrets[SecretKeySSOPassword],
		},
		Peer: hms.Credentials{
			VCHost:   secrets[SecretKeyPeerVCHost],
			User:     secrets[SecretKeyPeerSSOUser],
			Password: secrets[SecretKeyPeerSSOPassword],
		},
		PeerEndpoint: secrets[SecretKeyPeerHMSEndpoint],
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = os.Getenv(csitypes.EnvVarReplicationEndpoint)
	}
	if cfg.Local.VCHost == "" && defaultVCHost != nil {
		cfg.Local.VCHost = defaultVCHost()
	}
	if v := secrets[SecretKeyInsecureSkipVerify]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid secret key %q: %w", SecretKeyInsecureSkipVerify, err)
		}
		cfg.Insecure = b
	}
	return cfg, nil
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
