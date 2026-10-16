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

package snapservice

// REST wire format shared by RESTClient and the fake HTTP server.
//
// Paths and field names follow the public SnapService 9.1 REST API where it
// exists (protection groups, ?action=promote|demote|activate, vmw-task=true,
// tasks, vAPI error_type). Everything below that is marked ASSUMED is a guess
// for the 9.2 API and must be checked against the published spec.

import (
	"errors"
	"net/http"
	"time"
)

const (
	apiPrefix = "/api/snapservice"

	sessionHeader = "vmware-api-session-id"

	taskStatusPending   = "PENDING"
	taskStatusRunning   = "RUNNING"
	taskStatusSucceeded = "SUCCEEDED"
	taskStatusFailed    = "FAILED"
)

type wireTargetEntities struct {
	VMs []string `json:"vms,omitempty"`
	// ASSUMED: FCD membership field name.
	FCDs []string `json:"fcds,omitempty"`
}

type wireReplicationPolicy struct {
	TargetSite          string `json:"target_site"`
	TargetDatastore     string `json:"target_datastore,omitempty"`
	TargetStoragePolicy string `json:"target_storage_policy,omitempty"`
	RPOMinutes          int64  `json:"rpo_minutes,omitempty"`
}

type wireProtectionGroupSpec struct {
	Name           string                  `json:"name"`
	TargetEntities wireTargetEntities      `json:"target_entities"`
	Replication    []wireReplicationPolicy `json:"replication_policies,omitempty"`
}

type wireProtectionGroup struct {
	PG                 string                  `json:"pg"`
	GlobalID           string                  `json:"global_id"`
	Name               string                  `json:"name"`
	Status             string                  `json:"status"`
	TargetEntities     wireTargetEntities      `json:"target_entities"`
	Replication        []wireReplicationPolicy `json:"replication_policies,omitempty"`
	ReplicationHealthy bool                    `json:"replication_healthy"`
	// ASSUMED: initial sync and last replica time reporting.
	InitialSyncDone bool       `json:"initial_sync_done"`
	LastReplicaTime *time.Time `json:"last_replica_time,omitempty"`
}

type wirePromoteSpec struct {
	Force bool `json:"force"`
}

// ASSUMED: promote task result.
type wireRecoveredDisk struct {
	SourceID    string `json:"source_id"`
	RecoveredID string `json:"recovered_id,omitempty"`
	DiskURLPath string `json:"disk_url_path"`
}

type wirePromoteResult struct {
	RecoveredDisks []wireRecoveredDisk `json:"recovered_disks,omitempty"`
}

type wireSite struct {
	Site string `json:"site"`
	Name string `json:"name"`
}

type wireTask struct {
	Status string             `json:"status"`
	Result *wirePromoteResult `json:"result,omitempty"`
	Error  *wireError         `json:"error,omitempty"`
}

type wireMessage struct {
	ID             string `json:"id"`
	DefaultMessage string `json:"default_message"`
}

// wireError is a vAPI REST error.
type wireError struct {
	ErrorType string        `json:"error_type"`
	Messages  []wireMessage `json:"messages,omitempty"`
}

// wireErrors maps sentinel errors to vAPI error types and message IDs.
// ASSUMED: message IDs.
var wireErrors = []struct {
	err       error
	errorType string
	messageID string
	status    int
}{
	{ErrNotFound, "NOT_FOUND", "snapservice.protection_group.not_found", http.StatusNotFound},
	{ErrAlreadyExists, "ALREADY_EXISTS", "snapservice.protection_group.already_exists", http.StatusBadRequest},
	{ErrPeerActive, "NOT_ALLOWED_IN_CURRENT_STATE", "snapservice.protection_group.peer_active",
		http.StatusBadRequest},
	{ErrPeerUnreachable, "NOT_ALLOWED_IN_CURRENT_STATE", "snapservice.site.peer_unreachable",
		http.StatusBadRequest},
	{ErrInvalidState, "NOT_ALLOWED_IN_CURRENT_STATE", "snapservice.protection_group.invalid_state",
		http.StatusBadRequest},
	{ErrNoSitePair, "INVALID_ARGUMENT", "snapservice.site.not_paired", http.StatusBadRequest},
	{ErrSiteUnavailable, "SERVICE_UNAVAILABLE", "snapservice.unavailable", http.StatusServiceUnavailable},
}

func toWireError(err error) (*wireError, int) {
	for _, e := range wireErrors {
		if errors.Is(err, e.err) {
			return &wireError{
				ErrorType: e.errorType,
				Messages:  []wireMessage{{ID: e.messageID, DefaultMessage: err.Error()}},
			}, e.status
		}
	}
	return &wireError{
		ErrorType: "ERROR",
		Messages:  []wireMessage{{ID: "snapservice.error", DefaultMessage: err.Error()}},
	}, http.StatusInternalServerError
}

// fromWireError returns an error that wraps the matching sentinel error.
func fromWireError(we *wireError) error {
	msg := we.ErrorType
	var id string
	if len(we.Messages) > 0 {
		id = we.Messages[0].ID
		msg = we.Messages[0].DefaultMessage
	}
	for _, e := range wireErrors {
		if id == e.messageID {
			return &apiError{sentinel: e.err, msg: msg}
		}
	}
	for _, e := range wireErrors {
		if we.ErrorType == e.errorType && e.errorType != "NOT_ALLOWED_IN_CURRENT_STATE" {
			return &apiError{sentinel: e.err, msg: msg}
		}
	}
	if we.ErrorType == "NOT_ALLOWED_IN_CURRENT_STATE" {
		return &apiError{sentinel: ErrInvalidState, msg: msg}
	}
	return &apiError{msg: we.ErrorType + ": " + msg}
}

// apiError is an error returned by the SnapService REST API.
type apiError struct {
	sentinel error
	msg      string
}

func (e *apiError) Error() string { return "snapservice: " + e.msg }

func (e *apiError) Unwrap() error { return e.sentinel }

func toWirePG(pg *ProtectionGroup) wireProtectionGroup {
	w := wireProtectionGroup{
		PG:                 pg.ID,
		GlobalID:           pg.GlobalID,
		Name:               pg.Name,
		Status:             string(pg.State),
		TargetEntities:     wireTargetEntities(pg.TargetEntities),
		ReplicationHealthy: pg.ReplicationHealthy,
		InitialSyncDone:    pg.InitialSyncDone,
		LastReplicaTime:    pg.LastReplicaTime,
	}
	if pg.Replication != nil {
		w.Replication = []wireReplicationPolicy{toWirePolicy(pg.Replication)}
	}
	return w
}

func fromWirePG(w *wireProtectionGroup) *ProtectionGroup {
	pg := &ProtectionGroup{
		ID:                 w.PG,
		GlobalID:           w.GlobalID,
		Name:               w.Name,
		State:              ProtectionGroupState(w.Status),
		TargetEntities:     TargetEntities(w.TargetEntities),
		ReplicationHealthy: w.ReplicationHealthy,
		InitialSyncDone:    w.InitialSyncDone,
		LastReplicaTime:    w.LastReplicaTime,
	}
	if len(w.Replication) > 0 {
		pg.Replication = fromWirePolicy(&w.Replication[0])
	}
	return pg
}

func toWireSpec(spec ProtectionGroupSpec) wireProtectionGroupSpec {
	w := wireProtectionGroupSpec{
		Name:           spec.Name,
		TargetEntities: wireTargetEntities(spec.TargetEntities),
	}
	if spec.Replication != nil {
		w.Replication = []wireReplicationPolicy{toWirePolicy(spec.Replication)}
	}
	return w
}

func fromWireSpec(w *wireProtectionGroupSpec) ProtectionGroupSpec {
	spec := ProtectionGroupSpec{
		Name:           w.Name,
		TargetEntities: TargetEntities(w.TargetEntities),
	}
	if len(w.Replication) > 0 {
		spec.Replication = fromWirePolicy(&w.Replication[0])
	}
	return spec
}

func toWirePolicy(p *ReplicationPolicy) wireReplicationPolicy {
	return wireReplicationPolicy{
		TargetSite:          p.TargetSite,
		TargetDatastore:     p.TargetDatastore,
		TargetStoragePolicy: p.TargetStoragePolicy,
		RPOMinutes:          int64(p.RPO / time.Minute),
	}
}

func fromWirePolicy(w *wireReplicationPolicy) *ReplicationPolicy {
	return &ReplicationPolicy{
		TargetSite:          w.TargetSite,
		TargetDatastore:     w.TargetDatastore,
		TargetStoragePolicy: w.TargetStoragePolicy,
		RPO:                 time.Duration(w.RPOMinutes) * time.Minute,
	}
}
