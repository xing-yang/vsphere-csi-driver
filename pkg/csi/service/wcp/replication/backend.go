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

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/snapservice"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

const (
	// BackendFake is an in-memory SnapService. Its state is lost when the
	// controller restarts, and the peer site exists only inside the process.
	BackendFake = "fake"
	// BackendREST talks to a SnapService REST endpoint, such as the shared
	// fake server (cmd/fake-snapservice).
	BackendREST = "rest"

	defaultFakeSite     = "site-a"
	defaultFakePeerSite = "site-b"
)

// Keys of the VolumeReplicationClass secret for the rest backend. Missing
// keys fall back to the X_CSI_REPLICATION_* environment variables.
const (
	SecretKeyEndpoint           = "endpoint"
	SecretKeySite               = "site"
	SecretKeyCluster            = "cluster"
	SecretKeyUsername           = "username"
	SecretKeyPassword           = "password"
	SecretKeyInsecureSkipVerify = "insecureSkipVerify"
)

// NewClientFactoryFromEnv returns the SnapService client factory selected by
// X_CSI_REPLICATION_BACKEND.
func NewClientFactoryFromEnv(ctx context.Context) (ClientFactory, error) {
	log := logger.GetLogger(ctx)
	backend := os.Getenv(csitypes.EnvVarReplicationBackend)
	if backend == "" {
		backend = BackendFake
	}
	switch backend {
	case BackendFake:
		site := envOrDefault(csitypes.EnvVarReplicationSite, defaultFakeSite)
		peer := envOrDefault(csitypes.EnvVarReplicationPeerSite, defaultFakePeerSite)
		if site == peer {
			return nil, fmt.Errorf("%s and %s must differ, both are %q",
				csitypes.EnvVarReplicationSite, csitypes.EnvVarReplicationPeerSite, site)
		}
		log.Warnf("Volume replication uses the in-memory fake SnapService (site %q paired with %q); "+
			"replication state is lost on restart", site, peer)
		fabric := snapservice.NewFakeFabric([]string{site, peer})
		client := fabric.Site(site)
		return func(ctx context.Context, secrets map[string]string) (snapservice.Client, error) {
			return client, nil
		}, nil
	case BackendREST:
		log.Infof("Volume replication uses the SnapService REST backend")
		return newRESTClientFactory(), nil
	default:
		return nil, fmt.Errorf("unsupported %s %q", csitypes.EnvVarReplicationBackend, backend)
	}
}

// newRESTClientFactory returns a factory that builds REST clients from the
// VolumeReplicationClass secret and caches them so sessions are reused.
func newRESTClientFactory() ClientFactory {
	var (
		mu      sync.Mutex
		clients = map[snapservice.RESTConfig]*snapservice.RESTClient{}
	)
	return func(ctx context.Context, secrets map[string]string) (snapservice.Client, error) {
		cfg, err := restConfig(secrets)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[cfg]; ok {
			return c, nil
		}
		c, err := snapservice.NewRESTClient(cfg)
		if err != nil {
			return nil, err
		}
		clients[cfg] = c
		return c, nil
	}
}

func restConfig(secrets map[string]string) (snapservice.RESTConfig, error) {
	get := func(key, env string) string {
		if v := secrets[key]; v != "" {
			return v
		}
		if env != "" {
			return os.Getenv(env)
		}
		return ""
	}
	cfg := snapservice.RESTConfig{
		Endpoint: get(SecretKeyEndpoint, csitypes.EnvVarReplicationEndpoint),
		Site:     get(SecretKeySite, csitypes.EnvVarReplicationSite),
		Cluster:  get(SecretKeyCluster, csitypes.EnvVarReplicationCluster),
		Username: get(SecretKeyUsername, ""),
		Password: get(SecretKeyPassword, ""),
	}
	if v := secrets[SecretKeyInsecureSkipVerify]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid secret key %q: %w", SecretKeyInsecureSkipVerify, err)
		}
		cfg.InsecureSkipVerify = b
	}
	return cfg, nil
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
