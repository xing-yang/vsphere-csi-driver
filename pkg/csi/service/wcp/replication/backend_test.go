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
	"testing"

	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
)

func TestRESTConfigFromSecretAndEnv(t *testing.T) {
	t.Setenv(csitypes.EnvVarReplicationEndpoint, "http://env:8080/site-a")
	t.Setenv(csitypes.EnvVarReplicationSite, "site-a")
	t.Setenv(csitypes.EnvVarReplicationCluster, "domain-c1")

	cfg, err := restConfig(map[string]string{
		SecretKeyEndpoint:           "https://secret/site-b",
		SecretKeyUsername:           "user",
		SecretKeyPassword:           "pass",
		SecretKeyInsecureSkipVerify: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://secret/site-b" || cfg.Site != "site-a" || cfg.Cluster != "domain-c1" ||
		cfg.Username != "user" || cfg.Password != "pass" || !cfg.InsecureSkipVerify {
		t.Fatalf("unexpected config %+v", cfg)
	}
	if _, err := restConfig(map[string]string{SecretKeyInsecureSkipVerify: "maybe"}); err == nil {
		t.Fatal("expected an error for an invalid insecureSkipVerify")
	}
}

func TestRESTClientFactoryCachesClients(t *testing.T) {
	t.Setenv(csitypes.EnvVarReplicationBackend, BackendREST)
	t.Setenv(csitypes.EnvVarReplicationEndpoint, "http://env:8080/site-a")
	t.Setenv(csitypes.EnvVarReplicationSite, "site-a")
	t.Setenv(csitypes.EnvVarReplicationCluster, "domain-c1")
	ctx := context.Background()
	factory, err := NewClientFactoryFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c1, err := factory(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := factory(ctx, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected the same client for the same configuration")
	}
	c3, err := factory(ctx, map[string]string{SecretKeyEndpoint: "http://other/site-a"})
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 {
		t.Fatal("expected a different client for a different endpoint")
	}
}

func TestClientFactoryFromEnvRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	t.Setenv(csitypes.EnvVarReplicationBackend, "bogus")
	if _, err := NewClientFactoryFromEnv(ctx); err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
	t.Setenv(csitypes.EnvVarReplicationBackend, BackendFake)
	t.Setenv(csitypes.EnvVarReplicationSite, "same")
	t.Setenv(csitypes.EnvVarReplicationPeerSite, "same")
	if _, err := NewClientFactoryFromEnv(ctx); err == nil {
		t.Fatal("expected an error when site and peer are the same")
	}
}
