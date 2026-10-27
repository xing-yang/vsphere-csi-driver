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
	"net"
	"os"
	"strings"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/replication"
	"google.golang.org/grpc"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

// csiAddonsReplicationProvider is implemented by controllers that support the
// csi-addons Replication service.
type csiAddonsReplicationProvider interface {
	NewCSIAddonsReplicationServers(ctx context.Context, version string) (
		identity.IdentityServer, replication.ControllerServer, error)
}

// startCSIAddonsServer serves the csi-addons Identity and Replication services
// on their own unix socket, which the csi-addons sidecar connects to with
// --csi-addons-address. It returns once the socket is listening.
func startCSIAddonsServer(ctx context.Context, endpoint string, ids identity.IdentityServer,
	rs replication.ControllerServer) (*grpc.Server, error) {
	log := logger.GetLogger(ctx)
	if !strings.HasPrefix(endpoint, UnixSocketPrefix) {
		return nil, logger.LogNewErrorf(log, "csi-addons endpoint must be a unix socket: %s", endpoint)
	}
	addr := strings.TrimPrefix(endpoint, UnixSocketPrefix)
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, logger.LogNewErrorf(log, "failed to remove %s. Err: %v", addr, err)
	}
	listener, err := net.Listen("unix", addr)
	if err != nil {
		return nil, logger.LogNewErrorf(log, "failed to listen on %s: %v", addr, err)
	}

	server := grpc.NewServer()
	identity.RegisterIdentityServer(server, ids)
	replication.RegisterControllerServer(server, rs)
	go func() {
		log.Infof("Serving csi-addons identity and replication services on %s", listener.Addr())
		if err := server.Serve(listener); err != nil {
			log.Errorf("csi-addons server stopped: %v", err)
		}
	}()
	return server, nil
}
