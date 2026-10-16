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
	"fmt"
	"strconv"
	"strings"
	"time"
)

// VolumeReplicationClass parameters understood by the driver. The csi-addons
// controller passes all VolumeReplicationClass parameters except the
// "replication.storage.openshift.io/" prefixed ones.
const (
	// ParamTargetSite is the SnapService site to replicate to. Required.
	ParamTargetSite = "targetSite"
	// ParamTargetDatastore is the datastore on the target site for replicas.
	ParamTargetDatastore = "targetDatastore"
	// ParamTargetStoragePolicy is the storage policy for replicas.
	ParamTargetStoragePolicy = "targetStoragePolicy"
	// ParamSchedulingInterval is the csi-addons scheduling interval, used as the RPO.
	ParamSchedulingInterval = "schedulingInterval"
)

// classParams are the parsed VolumeReplicationClass parameters.
type classParams struct {
	targetSite          string
	targetDatastore     string
	targetStoragePolicy string
	rpo                 time.Duration
}

// parseClassParams parses VolumeReplicationClass parameters. Unknown
// parameters are rejected so that typos surface as a Validated=False condition.
func parseClassParams(params map[string]string, requireTarget bool) (*classParams, error) {
	p := &classParams{}
	for k, v := range params {
		switch k {
		case ParamTargetSite:
			p.targetSite = v
		case ParamTargetDatastore:
			p.targetDatastore = v
		case ParamTargetStoragePolicy:
			p.targetStoragePolicy = v
		case ParamSchedulingInterval:
			d, err := parseSchedulingInterval(v)
			if err != nil {
				return nil, err
			}
			p.rpo = d
		default:
			return nil, fmt.Errorf("unknown parameter %q", k)
		}
	}
	if requireTarget && p.targetSite == "" {
		return nil, fmt.Errorf("parameter %q is required", ParamTargetSite)
	}
	return p, nil
}

// parseSchedulingInterval parses the csi-addons scheduling interval format
// (for example "5m", "1h" or "1d").
func parseSchedulingInterval(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid %s %q", ParamSchedulingInterval, v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s %q", ParamSchedulingInterval, v)
	}
	return d, nil
}
