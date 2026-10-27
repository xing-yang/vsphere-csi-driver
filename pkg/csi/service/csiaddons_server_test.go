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

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientset "k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/wcpguest"
	guestreplication "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/wcpguest/replication"
)

func TestGuestControllerProvidesCSIAddonsReplication(t *testing.T) {
	if _, ok := wcpguest.New().(csiAddonsReplicationProvider); !ok {
		t.Fatal("guest controller does not implement csiAddonsReplicationProvider")
	}
}

func TestCSIAddonsServer(t *testing.T) {
	ctx := context.Background()
	// Unix socket paths are limited to about 100 bytes, so avoid t.TempDir.
	dir, err := os.MkdirTemp("", "csiaddons")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	endpoint := UnixSocketPrefix + filepath.Join(dir, "csi-addons.sock")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{{Group: "replication.storage.openshift.io",
			Version: "v1alpha1", Resource: "volumereplications"}: "VolumeReplicationList"})
	kube := k8sfake.NewClientset()
	server, err := startCSIAddonsServer(ctx, endpoint,
		guestreplication.NewIdentityServer("test"),
		guestreplication.NewServer(guestreplication.Config{
			Clients: func(context.Context) (dynamic.Interface, clientset.Interface, clientset.Interface, error) {
				return dyn, kube, kube, nil
			},
			SupervisorNamespace: "sv-ns",
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	id, err := identity.NewIdentityClient(conn).GetIdentity(ctx, &identity.GetIdentityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if id.GetName() != "csi.vsphere.vmware.com" {
		t.Fatalf("identity name = %q", id.GetName())
	}

	// The replication service is reachable; with no Supervisor
	// VolumeReplication the volume is reported as not replicated.
	info, err := replication.NewControllerClient(conn).GetVolumeReplicationInfo(ctx,
		&replication.GetVolumeReplicationInfoRequest{
			ReplicationSource: &replication.ReplicationSource{
				Type: &replication.ReplicationSource_Volume{
					Volume: &replication.ReplicationSource_VolumeSource{VolumeId: "vol-1"},
				},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetStatus() != replication.GetVolumeReplicationInfoResponse_ERROR {
		t.Fatalf("expected ERROR status, got %s %q", info.GetStatus(), info.GetStatusMessage())
	}
}
