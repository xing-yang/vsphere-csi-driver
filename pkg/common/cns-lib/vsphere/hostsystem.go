/*
Copyright 2019 The Kubernetes Authors.

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

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"k8s.io/klog"
)

// HostSystem holds details of a host instance.
type HostSystem struct {
	// HostSystem represents the host system.
	*object.HostSystem
}

// GetAllAccessibleDatastores gets the list of accessible datastores for the given host
func (host *HostSystem) GetAllAccessibleDatastores(ctx context.Context) ([]*DatastoreInfo, error) {
	var hostSystemMo mo.HostSystem
	s := object.NewSearchIndex(host.Client())
	err := s.Properties(ctx, host.Reference(), []string{"datastore"}, &hostSystemMo)
	if err != nil {
		klog.Errorf("Failed to retrieve datastores for host %v with err: %v", host, err)
		return nil, err
	}
	var dsRefList []types.ManagedObjectReference
	dsRefList = append(dsRefList, hostSystemMo.Datastore...)

	var dsMoList []mo.Datastore
	pc := property.DefaultCollector(host.Client())
	properties := []string{"info"}
	err = pc.Retrieve(ctx, dsRefList, properties, &dsMoList)
	if err != nil {
		klog.Errorf("Failed to get datastore managed objects from datastore objects %v with properties %v: %v", dsRefList, properties, err)
		return nil, err
	}
	var dsObjList []*DatastoreInfo
	for _, dsMo := range dsMoList {
		dsObjList = append(dsObjList,
			&DatastoreInfo{
				&Datastore{object.NewDatastore(host.Client(), dsMo.Reference()),
					nil},
				dsMo.Info.GetDatastoreInfo()})
	}
	return dsObjList, nil
}
