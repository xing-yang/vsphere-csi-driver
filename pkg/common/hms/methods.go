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

package hms

import (
	"context"
	"encoding/xml"
)

// Request and response bodies of the HMS methods the client uses. Element
// names are the VMODL wsdlNames; @task methods get the "_Task" suffix. Field
// order follows the VMODL declarations.

type loginRequest struct {
	XMLName xml.Name `xml:"HmsSessionManagerLogin"`
	This    MoRef    `xml:"_this"`
}

type loginHmsServerByTokenRequest struct {
	XMLName        xml.Name `xml:"HmsSessionManagerLoginHmsServerByToken"`
	This           MoRef    `xml:"_this"`
	Site           MoRef    `xml:"site"`
	DelegatedToken string   `xml:"delegatedToken"`
}

type getHmsServersRequest struct {
	XMLName xml.Name `xml:"HmsRemoteSiteManagerGetHmsServersInternal"`
	This    MoRef    `xml:"_this"`
}

type moRefListResponse struct {
	Returnval []MoRef `xml:"returnval"`
}

// serverInfoRequest is HmsRemoteHmsServerGetInfoInternal. HMS does not
// expose vmodl Fetch, so properties are read through the internal getters.
type serverInfoRequest struct {
	XMLName xml.Name `xml:"HmsRemoteHmsServerGetInfoInternal"`
	This    MoRef    `xml:"_this"`
}

// hmsServerInfo is HmsRemoteHmsServerInfo.
type hmsServerInfo struct {
	Name             string `xml:"name"`
	UUID             string `xml:"uuid"`
	Connected        bool   `xml:"connected"`
	PairedVcSiteName string `xml:"pairedVcSiteName"`
}

type serverInfoResponse struct {
	Returnval hmsServerInfo `xml:"returnval"`
}

type listReplicationsRequest struct {
	XMLName xml.Name
	This    MoRef `xml:"_this"`
	Start   int   `xml:"start"`
	Count   int   `xml:"count"`
}

// tabularInfo is HmsReplicationTabularInfo.
type tabularInfo struct {
	Replication MoRef  `xml:"replication"`
	Name        string `xml:"name"`
	Status      string `xml:"status"`
	// RPO is in minutes.
	RPO         int64   `xml:"rpo"`
	SourceSite  tabSite `xml:"sourceSite"`
	TargetSite  tabSite `xml:"targetSite"`
	PeerHealthy bool    `xml:"peerHealthy"`
}

// tabSite is HmsReplicationTabularInfoVcSite.
type tabSite struct {
	HmsServer *MoRef `xml:"hmsServer"`
	SiteName  string `xml:"siteName"`
}

type tabularPageResponse struct {
	Returnval struct {
		TotalCount int           `xml:"totalCount"`
		Info       []tabularInfo `xml:"info"`
	} `xml:"returnval"`
}

// groupSettings is HmsGroupSettings. The VM-only settings must be unset for
// CNS groups.
type groupSettings struct {
	Name                      string `xml:"name"`
	RPO                       int64  `xml:"rpo"`
	QuiesceGuestEnabled       bool   `xml:"quiesceGuestEnabled"`
	NetworkCompressionEnabled bool   `xml:"networkCompressionEnabled"`
}

// cnsVolumeReplicationSpec is HmsGroupCnsVolumeReplicationSpec.
type cnsVolumeReplicationSpec struct {
	Settings   groupSettings `xml:"settings"`
	VolumeUUID string        `xml:"volumeUuid"`
	// The field order follows the VMODL; vmomi rejects out-of-order fields.
	SourceDatastoreRef     MoRef  `xml:"sourceDatastoreRef"`
	TargetDatastoreRef     MoRef  `xml:"targetDatastoreRef"`
	TargetStorageProfileID string `xml:"targetStorageProfileId,omitempty"`
}

type configureCnsVolumeReplicationRequest struct {
	XMLName           xml.Name                 `xml:"HmsReplicationManagerConfigureCnsVolumeReplication_Task"`
	This              MoRef                    `xml:"_this"`
	Site              MoRef                    `xml:"site"`
	GroupSpec         cnsVolumeReplicationSpec `xml:"groupSpec"`
	LicensingEntityID string                   `xml:"licensingEntityId"`
}

type moRefResponse struct {
	Returnval MoRef `xml:"returnval"`
}

// groupRequest is a Group method without parameters.
type groupRequest struct {
	XMLName xml.Name
	This    MoRef `xml:"_this"`
}

type preserveDisksRequest struct {
	XMLName              xml.Name
	This                 MoRef `xml:"_this"`
	PreserveReplicaDisks bool  `xml:"preserveReplicaDisks"`
}

// policyProgress is HmsPolicyProgress.
type policyProgress struct {
	CurrentGeneration        *int64 `xml:"currentGeneration"`
	LastSyncTimeMs           *int64 `xml:"lastSyncTimeMs"`
	LastSyncBytesTransferred *int64 `xml:"lastSyncBytesTransferred"`
	LiveBytesTransferred     *int64 `xml:"liveBytesTransferred"`
	LiveBytesToTransfer      *int64 `xml:"liveBytesToTransfer"`
	Health                   string `xml:"health"`
}

type policyProgressResponse struct {
	Returnval *policyProgress `xml:"returnval"`
}

type currentSpecResponse struct {
	Returnval struct {
		TargetDatastoreRef *MoRef `xml:"targetDatastoreRef"`
	} `xml:"returnval"`
}

type recoverGroupRequest struct {
	XMLName      xml.Name `xml:"HmsSecondaryGroupRecoverGroup_Task"`
	This         MoRef    `xml:"_this"`
	Folder       MoRef    `xml:"folder"`
	Pool         MoRef    `xml:"pool"`
	Sync         bool     `xml:"sync"`
	PowerOn      bool     `xml:"powerOn"`
	RecoveryType string   `xml:"recoveryType"`
}

type taskInfoRequest struct {
	XMLName xml.Name `xml:"HmsTaskGetInfoInternal"`
	This    MoRef    `xml:"_this"`
}

// taskInfo is the subset of vim.TaskInfo the client reads.
type taskInfo struct {
	State string `xml:"state"`
	Error *struct {
		LocalizedMessage string `xml:"localizedMessage"`
		Fault            struct {
			Type string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
		} `xml:"fault"`
	} `xml:"error"`
	Result *MoRef `xml:"result"`
}

type taskInfoResponse struct {
	Returnval taskInfo `xml:"returnval"`
}

func named(local string) xml.Name { return xml.Name{Local: local} }

// The functions below are the raw calls. They do not handle sessions.

func login(ctx context.Context, t *soapTransport, security string) error {
	return t.call(ctx, &loginRequest{This: sessionManager}, nil, security)
}

func loginHmsServerByToken(ctx context.Context, t *soapTransport, site MoRef, token string) error {
	return t.call(ctx, &loginHmsServerByTokenRequest{This: sessionManager, Site: site, DelegatedToken: token},
		nil, "")
}

func getHmsServers(ctx context.Context, t *soapTransport) ([]MoRef, error) {
	var res moRefListResponse
	err := t.call(ctx, &getHmsServersRequest{This: siteManager}, &res, "")
	return res.Returnval, err
}

func getServerInfo(ctx context.Context, t *soapTransport, server MoRef) (*hmsServerInfo, error) {
	var res serverInfoResponse
	if err := t.call(ctx, &serverInfoRequest{This: server}, &res, ""); err != nil {
		return nil, err
	}
	return &res.Returnval, nil
}

func listReplications(ctx context.Context, t *soapTransport, outgoing bool, start, count int) (
	*tabularPageResponse, error) {
	req := &listReplicationsRequest{This: replicationManager, Start: start, Count: count,
		XMLName: named("HmsReplicationManagerGetOutgoingReplications")}
	if !outgoing {
		req.This = replicaManager
		req.XMLName = named("HmsReplicaManagerGetIncomingReplications")
	}
	var res tabularPageResponse
	if err := t.call(ctx, req, &res, ""); err != nil {
		return nil, err
	}
	return &res, nil
}

func configureCnsVolumeReplication(ctx context.Context, t *soapTransport, site MoRef,
	spec cnsVolumeReplicationSpec) (MoRef, error) {
	var res moRefResponse
	err := t.call(ctx, &configureCnsVolumeReplicationRequest{
		This: replicationManager, Site: site, GroupSpec: spec,
		// Ignored by HMS but not optional in the VMODL.
		LicensingEntityID: "",
	}, &res, "")
	return res.Returnval, err
}

func getPolicyProgress(ctx context.Context, t *soapTransport, group MoRef) (*policyProgress, error) {
	var res policyProgressResponse
	err := t.call(ctx, &groupRequest{XMLName: named("HmsGroupGetPolicyProgress"), This: group}, &res, "")
	return res.Returnval, err
}

func currentSpec(ctx context.Context, t *soapTransport, group MoRef) (*currentSpecResponse, error) {
	var res currentSpecResponse
	if err := t.call(ctx, &groupRequest{XMLName: named("HmsGroupCurrentSpec"), This: group}, &res, ""); err != nil {
		return nil, err
	}
	return &res, nil
}

func onlineSync(ctx context.Context, t *soapTransport, group MoRef) (MoRef, error) {
	var res moRefResponse
	err := t.call(ctx, &groupRequest{XMLName: named("HmsGroupOnlineSync_Task"), This: group}, &res, "")
	return res.Returnval, err
}

func unconfigure(ctx context.Context, t *soapTransport, group MoRef, preserve bool) (MoRef, error) {
	var res moRefResponse
	err := t.call(ctx, &preserveDisksRequest{XMLName: named("HmsGroupUnconfigure_Task"), This: group,
		PreserveReplicaDisks: preserve}, &res, "")
	return res.Returnval, err
}

func recoverGroup(ctx context.Context, t *soapTransport, group MoRef, spec RecoverSpec) (MoRef, error) {
	var res moRefResponse
	err := t.call(ctx, &recoverGroupRequest{
		// Recovery methods only exist on HmsSecondaryGroup; list results may
		// carry the base type.
		This:   MoRef{Type: "HmsSecondaryGroup", Value: group.Value},
		Folder: spec.Folder, Pool: spec.Pool,
		Sync: false, PowerOn: false, RecoveryType: string(spec.Type),
	}, &res, "")
	return res.Returnval, err
}

func getTaskInfo(ctx context.Context, t *soapTransport, task MoRef) (*taskInfo, error) {
	var res taskInfoResponse
	if err := t.call(ctx, &taskInfoRequest{This: MoRef{Type: "HmsTask", Value: task.Value}}, &res, ""); err != nil {
		return nil, err
	}
	return &res.Returnval, nil
}
