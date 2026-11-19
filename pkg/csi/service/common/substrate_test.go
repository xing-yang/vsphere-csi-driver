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

package common

import (
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestIsSubstrateCreateRequest(t *testing.T) {
	tests := []struct {
		name   string
		req    *csi.CreateVolumeRequest
		expect bool
	}{
		{
			name:   "nil request",
			req:    nil,
			expect: false,
		},
		{
			name: "substrate request",
			req: &csi.CreateVolumeRequest{
				Name:       "substrate-0b6f1c7e-2c1d-4f7e-9a51-2f4f7f1d9c3a-data",
				Parameters: map[string]string{AttributeSupervisorStorageClass: "gold"},
			},
			expect: true,
		},
		{
			name:   "substrate request without parameters",
			req:    &csi.CreateVolumeRequest{Name: "substrate-uid-data"},
			expect: true,
		},
		{
			name: "external-provisioner request",
			req: &csi.CreateVolumeRequest{
				Name: "pvc-0b6f1c7e-2c1d-4f7e-9a51-2f4f7f1d9c3a",
				Parameters: map[string]string{
					AttributePvcName:      "data",
					AttributePvcNamespace: "default",
				},
			},
			expect: false,
		},
		{
			name: "external-provisioner request with substrate volume name prefix",
			req: &csi.CreateVolumeRequest{
				Name:       "substrate-data",
				Parameters: map[string]string{AttributePvName: "substrate-data"},
			},
			expect: false,
		},
		{
			name: "PVC metadata parameter in mixed case",
			req: &csi.CreateVolumeRequest{
				Name:       "substrate-uid-data",
				Parameters: map[string]string{"CSI.Storage.K8s.io/PVC/Name": "data"},
			},
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSubstrateCreateRequest(tt.req); got != tt.expect {
				t.Errorf("IsSubstrateCreateRequest() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestNormalizeSubstrateParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		expect  map[string]string
		wantErr bool
	}{
		{
			name:   "nil parameters",
			params: nil,
			expect: map[string]string{},
		},
		{
			name: "reserved keys are removed",
			params: map[string]string{
				AttributeSupervisorStorageClass:                      "gold",
				"csi.storage.k8s.io/fstype":                          "ext4",
				"csi.storage.k8s.io/provisioner-secret-name":         "secret",
				"CSI.Storage.K8s.io/Controller-Expand-Secret-Name":   "secret",
				"csi.storage.k8s.io/controller-publish-secret-names": "secret",
			},
			expect: map[string]string{AttributeSupervisorStorageClass: "gold"},
		},
		{
			name:   "ext4 fstype is accepted in any case",
			params: map[string]string{"csi.storage.k8s.io/fstype": "EXT4"},
			expect: map[string]string{},
		},
		{
			name:   "empty fstype is accepted",
			params: map[string]string{"csi.storage.k8s.io/fstype": ""},
			expect: map[string]string{},
		},
		{
			name:    "other fstype is rejected",
			params:  map[string]string{"csi.storage.k8s.io/fstype": "xfs"},
			wantErr: true,
		},
		{
			name:   "non-reserved keys are kept as is",
			params: map[string]string{"svStorageClass": "gold", AttributeStorageTopologyType: "zonal"},
			expect: map[string]string{"svStorageClass": "gold", AttributeStorageTopologyType: "zonal"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSubstrateParams(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeSubstrateParams() returned %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSubstrateParams() returned unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.expect) {
				t.Errorf("NormalizeSubstrateParams() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestNormalizeSubstrateParamsDoesNotModifyInput(t *testing.T) {
	params := map[string]string{"csi.storage.k8s.io/fstype": "ext4", AttributeSupervisorStorageClass: "gold"}
	if _, err := NormalizeSubstrateParams(params); err != nil {
		t.Fatalf("NormalizeSubstrateParams() returned unexpected error: %v", err)
	}
	if len(params) != 2 {
		t.Errorf("NormalizeSubstrateParams() modified its input: %v", params)
	}
}
