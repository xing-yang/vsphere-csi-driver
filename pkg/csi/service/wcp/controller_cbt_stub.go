//go:build !linux || !cgo

/*
Copyright 2025 The Kubernetes Authors.

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
	"fmt"
)

// queryChangedAreasFromVirtualDisks is a stub implementation for non-Linux platforms.
// VDDK is only available on Linux, so this returns an error on other platforms.
func (c *controller) queryChangedAreasFromVirtualDisks(ctx context.Context, volumeID, baseSnapshotID,
	targetSnapshotID string, startingOffset uint64, maxResults uint32) (*ChangedAreasResult, error) {
	return nil, fmt.Errorf("VDDK/Changed Block Tracking is only supported on Linux platforms")
}

// queryAllocatedBlocksFromVirtualDisks is a stub implementation for non-Linux platforms.
// VDDK is only available on Linux, so this returns an error on other platforms.
func (c *controller) queryAllocatedBlocksFromVirtualDisks(ctx context.Context, volumeID, snapshotID string,
	startingOffset uint64, maxResults uint32) (*AllocatedAreasResult, error) {
	return nil, fmt.Errorf("VDDK/Changed Block Tracking is only supported on Linux platforms")
}

