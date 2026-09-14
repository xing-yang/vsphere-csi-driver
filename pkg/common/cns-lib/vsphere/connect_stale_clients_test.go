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

package vsphere

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cnssim "github.com/vmware/govmomi/cns/simulator"
	pbmsim "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
)

// vcsimFor starts a vcsim instance, with the CNS and PBM APIs registered so
// tests can build real, working clients against it, and returns its host and
// port.
func vcsimFor(t *testing.T) (string, int) {
	t.Helper()
	confPath := filepath.Join(t.TempDir(), "csi-vsphere.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"[Global]\ncluster-id = \"c\"\n\n"+
			"[VirtualCenter \"127.0.0.1\"]\nuser = \"user@vsphere.local\"\npassword = \"pass\"\n"+
			"datacenters = \"DC0\"\ninsecure-flag = \"true\"\n"), 0600))
	t.Setenv("VSPHERE_CSI_CONFIG", confPath)

	model := simulator.VPX()
	t.Cleanup(model.Remove)
	require.NoError(t, model.Create())
	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	t.Cleanup(server.Close)
	model.Service.RegisterSDK(cnssim.New())
	model.Service.RegisterSDK(pbmsim.New())

	port, err := strconv.Atoi(server.URL.Port())
	require.NoError(t, err)
	return server.URL.Hostname(), port
}

// unreachableAddr returns a host:port that refuses TCP connections, standing
// in for vpxd being briefly unreachable mid-patch.
func unreachableAddr(t *testing.T) (string, int) {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	server.Close() // now refuses connections
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return u.Hostname(), port
}

// connectedVC returns a VirtualCenter with a live SOAP session against vcsim.
func connectedVC(t *testing.T, ctx context.Context, host string, port int) *VirtualCenter {
	t.Helper()
	vc := &VirtualCenter{
		Config: &VirtualCenterConfig{
			Host: host, Port: port, Insecure: true,
			Username: "user", Password: "pass", // simulator.DefaultLogin
		},
		ClientMutex: &sync.Mutex{},
	}
	require.NoError(t, vc.Connect(ctx), "initial connect should succeed")
	return vc
}

// TestConnectRecreatesStaleDependentClients reproduces a vCenter service
// disruption (an in-place patch restarting vpxd/vsanvcmgmtd) that makes
// exactly one reconnect attempt fail after the session has already gone
// bad. Before this fix, a failed NewClient() call in the relogin branch was
// assigned straight into vc.Client, nulling it; the *next*, successful
// connect() then took the "client was never initialized" branch and returned
// early without rebuilding CnsClient -- leaving it bound to the vim25 client
// from before the disruption indefinitely, since connect()'s own health
// check only ever looks at vc.Client, which by then is healthy again. That is
// why the driver kept failing every CNS call with NotAuthenticated long after
// vCenter itself had recovered.
func TestConnectRecreatesStaleDependentClients(t *testing.T) {
	ctx := context.Background()
	host, port := vcsimFor(t)
	vc := connectedVC(t, ctx, host, port)

	require.NoError(t, vc.ConnectCns(ctx), "initial ConnectCns should succeed")

	originalClient := vc.Client
	staleCnsClient := vc.CnsClient
	require.NotNil(t, staleCnsClient)

	// Kill the session server-side, so the next connect() sees it as invalid
	// and attempts a full re-login -- the same trigger a vCenter service
	// restart produces.
	require.NoError(t, vc.Client.Logout(ctx))

	// Point at an address that refuses connections, so the reconnect
	// NewClient() call fails exactly once.
	downHost, downPort := unreachableAddr(t)
	realHost, realPort := vc.Config.Host, vc.Config.Port
	vc.Config.Host, vc.Config.Port = downHost, downPort

	err := vc.connect(ctx)
	require.Error(t, err, "a reconnect attempt against an unreachable vCenter should fail")
	assert.Same(t, originalClient, vc.Client,
		"a failed reconnect attempt must not null out vc.Client -- that is what let connect() "+
			"skip rebuilding CnsClient on the next, successful attempt")

	// vCenter is back: restore the real address and let the next connect()
	// succeed.
	vc.Config.Host, vc.Config.Port = realHost, realPort
	require.NoError(t, vc.connect(ctx))

	assert.NotSame(t, originalClient, vc.Client,
		"the session was invalid, so connect() should have built a fresh client")
	require.NotNil(t, vc.CnsClient)
	assert.NotSame(t, staleCnsClient, vc.CnsClient,
		"CnsClient must be rebuilt on the fresh vim25 client, not left bound to the one "+
			"that existed before the disruption")
}

// TestDisconnectClearsDependentClients covers the second path to the same
// staleness: Disconnect() used to nil only vc.Client, leaving
// PbmClient/CnsClient/VsanClient/VslmClient non-nil and bound to the session
// it had just logged out. A subsequent connect() that takes the "client was
// never initialized" branch (vc.Client == nil) has no reason to look at
// them, so they would stay stale indefinitely.
func TestDisconnectClearsDependentClients(t *testing.T) {
	ctx := context.Background()
	host, port := vcsimFor(t)
	vc := connectedVC(t, ctx, host, port)

	require.NoError(t, vc.ConnectCns(ctx))
	require.NoError(t, vc.ConnectPbm(ctx))
	require.NotNil(t, vc.CnsClient)
	require.NotNil(t, vc.PbmClient)

	require.NoError(t, vc.Disconnect(ctx))

	assert.Nil(t, vc.Client)
	assert.Nil(t, vc.CnsClient, "Disconnect must drop CnsClient too, or a future connect() "+
		"that takes the initialisation branch would leave it bound to the session just logged out")
	assert.Nil(t, vc.PbmClient)
}
