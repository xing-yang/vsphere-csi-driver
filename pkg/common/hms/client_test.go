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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHMS is a canned vmomi SOAP endpoint. Responses follow the vmomi wire
// format; they are not captured from a real HMS yet.
type fakeHMS struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	loggedIn bool
	// expireOnce makes the first authenticated call fail with NotAuthenticated.
	expireOnce bool
	taskPolls  int
}

const envelope = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"
 xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<soapenv:Body>%s</soapenv:Body></soapenv:Envelope>`

func fault(typ, msg string) string {
	return fmt.Sprintf(`<soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>%s</faultstring>`+
		`<detail><%sFault xmlns="urn:hmssrv" xsi:type="%s"></%sFault></detail></soapenv:Fault>`, msg, typ, typ, typ)
}

func (f *fakeHMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	body := string(data)
	if got := r.Header.Get("SOAPAction"); got != `"urn:hmssrv/9.2.1"` {
		f.t.Errorf("SOAPAction %q", got)
	}
	var env struct {
		Body struct {
			Method struct {
				XMLName xml.Name
			} `xml:",any"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(data, &env); err != nil {
		f.t.Errorf("bad request: %v", err)
	}
	method := env.Body.Method.XMLName
	if method.Space != DefaultNamespace {
		f.t.Errorf("%s in namespace %q", method.Local, method.Space)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method.Local)
	f.bodies[method.Local] = body

	reply := func(s string) { fmt.Fprintf(w, envelope, s) }
	if method.Local == "HmsSessionManagerLogin" {
		if !strings.Contains(body, "<saml2:Assertion") || !strings.Contains(body, "wsse:Security") {
			f.t.Errorf("login without SAML token: %s", body)
		}
		f.loggedIn = true
		http.SetCookie(w, &http.Cookie{Name: "vmware_soap_session", Value: "s1"})
		reply(`<HmsSessionManagerLoginResponse xmlns="urn:hmssrv"><returnval/></HmsSessionManagerLoginResponse>`)
		return
	}
	if c, err := r.Cookie("vmware_soap_session"); err != nil || c.Value != "s1" || !f.loggedIn {
		w.WriteHeader(http.StatusInternalServerError)
		reply(fault("NotAuthenticated", "not logged in"))
		return
	}
	if f.expireOnce {
		f.expireOnce = false
		f.loggedIn = false
		w.WriteHeader(http.StatusInternalServerError)
		reply(fault("NotAuthenticated", "session expired"))
		return
	}

	switch method.Local {
	case "HmsReplicationManagerGetOutgoingReplications":
		reply(`<HmsReplicationManagerGetOutgoingReplicationsResponse xmlns="urn:hmssrv"><returnval>` +
			`<startPosition>0</startPosition><requestedCount>500</requestedCount><totalCount>2</totalCount>` +
			`<info xsi:type="HmsOutgoingReplicationTabularInfo"><replication type="HmsPrimaryGroup">GID-1</replication>` +
			`<name>other</name><status>OK</status><rpo>60</rpo><peerHealthy>true</peerHealthy></info>` +
			`<info xsi:type="HmsOutgoingReplicationTabularInfo"><replication type="HmsPrimaryGroup">GID-2</replication>` +
			`<name>cns-vol1</name><status>sync</status><rpo>15</rpo>` +
			`<sourceSite xsi:type="HmsReplicationTabularInfoVcSite"><hms type="HmsServiceInstance">service-instance</hms>` +
			`<siteName>a</siteName></sourceSite>` +
			`<targetSite xsi:type="HmsReplicationTabularInfoVcSite"><hmsServer type="HmsRemoteHmsServer">peer-1</hmsServer>` +
			`<siteName>vc-b</siteName></targetSite><peerHealthy>true</peerHealthy></info>` +
			`</returnval></HmsReplicationManagerGetOutgoingReplicationsResponse>`)
	case "HmsReplicaManagerGetIncomingReplications":
		reply(`<HmsReplicaManagerGetIncomingReplicationsResponse xmlns="urn:hmssrv"><returnval>` +
			`<totalCount>0</totalCount></returnval></HmsReplicaManagerGetIncomingReplicationsResponse>`)
	case "HmsRemoteSiteManagerGetHmsServersInternal":
		reply(`<HmsRemoteSiteManagerGetHmsServersInternalResponse xmlns="urn:hmssrv">` +
			`<returnval type="HmsRemoteHmsServer">peer-1</returnval></HmsRemoteSiteManagerGetHmsServersInternalResponse>`)
	case "HmsRemoteHmsServerGetInfoInternal":
		reply(`<HmsRemoteHmsServerGetInfoInternalResponse xmlns="urn:hmssrv"><returnval xsi:type="HmsRemoteHmsServerInfo">` +
			`<name>hms-b</name><uuid>u</uuid><connected>true</connected><pairedVcSiteName>vc-b</pairedVcSiteName>` +
			`</returnval></HmsRemoteHmsServerGetInfoInternalResponse>`)
	case "HmsSessionManagerLoginHmsServerByToken":
		reply(`<HmsSessionManagerLoginHmsServerByTokenResponse xmlns="urn:hmssrv"><returnval/>` +
			`</HmsSessionManagerLoginHmsServerByTokenResponse>`)
	case "HmsReplicationManagerConfigureCnsVolumeReplication_Task", "HmsGroupUnconfigure_Task",
		"HmsSecondaryGroupRecoverGroup_Task", "HmsPrepareCnsRecovery_Task", "HmsSealCnsRecovery_Task",
		"HmsBindRecoveredCnsVolume_Task", "HmsReverseConfigurePeReplication_Task",
		"HmsResumePeReprotectOperation_Task":
		reply(fmt.Sprintf(`<%sResponse xmlns="urn:hmssrv"><returnval type="HmsTask">task-7</returnval></%sResponse>`,
			method.Local, method.Local))
	case "HmsGroupOnlineSync_Task":
		reply(fault("HmsSyncOngoingFault", "a sync is running"))
	case "HmsTaskGetInfoInternal":
		f.taskPolls++
		state := "running"
		if f.taskPolls > 1 {
			state = "success"
		}
		task := f.calls[len(f.calls)-1-f.taskPolls]
		if strings.HasPrefix(task, "HmsSecondaryGroupRecoverGroup") {
			reply(`<HmsTaskGetInfoInternalResponse xmlns="urn:hmssrv"><returnval><key>task-7</key>` +
				`<state>error</state><error><fault xsi:type="HmsAlreadyRecoveredFault"></fault>` +
				`<localizedMessage>already recovered</localizedMessage></error></returnval></HmsTaskGetInfoInternalResponse>`)
			return
		}
		reply(fmt.Sprintf(`<HmsTaskGetInfoInternalResponse xmlns="urn:hmssrv"><returnval><key>task-7</key>`+
			`<state>%s</state><result xsi:type="ManagedObjectReference" type="HmsPrimaryGroup">GID-3</result>`+
			`</returnval></HmsTaskGetInfoInternalResponse>`, state))
	case "HmsGetRecoveredCnsDisk":
		reply(`<HmsGetRecoveredCnsDiskResponse xmlns="urn:hmssrv"><returnval>` +
			`<datastore type="Datastore">datastore-34</datastore><path>hbr-pe-gid-GID-2</path>` +
			`<fileName>cnsVolume.vmdk</fileName></returnval></HmsGetRecoveredCnsDiskResponse>`)
	case "HmsGetPeReprotectOperation":
		if !strings.Contains(body, "<operationId>"+testOperationID+"</operationId>") {
			w.WriteHeader(http.StatusInternalServerError)
			reply(fault("InvalidArgument", "A specified parameter was not correct: operationId"))
			return
		}
		reply(`<HmsGetPeReprotectOperationResponse xmlns="urn:hmssrv"><returnval>` +
			`<operationId>` + testOperationID + `</operationId><originalGroupId>GID-2</originalGroupId>` +
			`<phase>complete</phase><reverseGroup type="HmsPrimaryGroup">GID-9</reverseGroup>` +
			`<synchronizationMode>optimized</synchronizationMode></returnval></HmsGetPeReprotectOperationResponse>`)
	case "HmsGroupGetPolicyProgress":
		reply(`<HmsGroupGetPolicyProgressResponse xmlns="urn:hmssrv"><returnval>` +
			`<currentGeneration>4</currentGeneration><lastSyncTimeMs>1500</lastSyncTimeMs>` +
			`<lastSyncBytesTransferred>4096</lastSyncBytesTransferred><health>idle</health>` +
			`</returnval></HmsGroupGetPolicyProgressResponse>`)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		reply(fault("NotImplemented", method.Local))
	}
}

const testOperationID = "065e02f7-e8de-5c9b-a3d5-a7e54cccc077"

func newTestClient(t *testing.T) (*SOAPClient, *fakeHMS) {
	t.Helper()
	f := &fakeHMS{t: t, bodies: map[string]string{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	c, err := NewSOAPClient(Config{
		Endpoint: server.URL + "/vrms",
		Local:    Credentials{VCHost: "vc-a", User: "u", Password: "p"},
		Peer:     Credentials{VCHost: "vc-b", User: "pu", Password: "pp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.WithTokenIssuer(func(_ context.Context, creds Credentials, _ bool) (string, error) {
		return `<saml2:Assertion xmlns:saml2="urn:oasis:names:tc:SAML:2.0:assertion" ID="` + creds.VCHost +
			`"></saml2:Assertion>`, nil
	})
	return c, f
}

func TestFindGroupAndProgress(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	g, err := c.FindOutgoingGroup(ctx, "cns-vol1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Ref != (MoRef{Type: "HmsPrimaryGroup", Value: "GID-2"}) || g.Status != StatusSync ||
		g.RPO != 15*time.Minute || g.Peer == nil || g.Peer.Value != "peer-1" {
		t.Fatalf("group %+v", g)
	}
	if _, err := c.FindIncomingGroup(ctx, "cns-vol1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("incoming: %v", err)
	}
	p, err := c.GetPolicyProgress(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if *p.CurrentGeneration != 4 || *p.LastSyncDuration != 1500*time.Millisecond || *p.LastSyncBytes != 4096 ||
		p.Health != HealthIdle {
		t.Fatalf("progress %+v", p)
	}
	if !strings.Contains(f.bodies["HmsGroupGetPolicyProgress"], `<_this type="HmsPrimaryGroup">GID-2</_this>`) {
		t.Fatalf("request %s", f.bodies["HmsGroupGetPolicyProgress"])
	}
	if n := strings.Count(strings.Join(f.calls, " "), "HmsSessionManagerLogin "); n != 1 {
		t.Fatalf("logged in %d times: %v", n, f.calls)
	}
}

func TestConfigureLogsInToPeerAndWaitsForTask(t *testing.T) {
	c, f := newTestClient(t)
	g, err := c.ConfigureCnsVolumeReplication(context.Background(), CnsVolumeReplicationSpec{
		Name: "cns-vol1", TargetSite: "vc-b", VolumeID: "vol1",
		SourceDatastore: MoRef{Type: "Datastore", Value: "datastore-12"},
		TargetDatastore: MoRef{Type: "Datastore", Value: "datastore-34"}, RPO: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.Ref.Value != "GID-3" || g.RPO != 2*time.Minute {
		t.Fatalf("group %+v", g)
	}
	if !strings.Contains(f.bodies["HmsSessionManagerLoginHmsServerByToken"], `ID=&#34;vc-b&#34;`) {
		t.Fatal("peer login did not use a token from the peer vCenter")
	}
	req := f.bodies["HmsReplicationManagerConfigureCnsVolumeReplication_Task"]
	for _, want := range []string{
		`<_this type="HmsReplicationManager">replication-manager</_this>`,
		`<site type="HmsRemoteHmsServer">peer-1</site>`,
		`<settings><name>cns-vol1</name><rpo>2</rpo><quiesceGuestEnabled>false</quiesceGuestEnabled>`,
		`<volumeUuid>vol1</volumeUuid><sourceDatastoreRef type="Datastore">datastore-12</sourceDatastoreRef>` +
			`<targetDatastoreRef type="Datastore">datastore-34</targetDatastoreRef></groupSpec>`,
		`<licensingEntityId></licensingEntityId>`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request lacks %s:\n%s", want, req)
		}
	}
	if f.taskPolls != 2 {
		t.Fatalf("polled task %d times", f.taskPolls)
	}
}

func TestConfigureUnpairedSite(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.ConfigureCnsVolumeReplication(context.Background(), CnsVolumeReplicationSpec{TargetSite: "vc-z"})
	if !errors.Is(err, ErrNoPeer) {
		t.Fatalf("got %v", err)
	}
}

func TestReloginAfterSessionExpiry(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	if _, err := c.FindOutgoingGroup(ctx, "cns-vol1"); err != nil {
		t.Fatal(err)
	}
	f.expireOnce = true
	if _, err := c.FindOutgoingGroup(ctx, "cns-vol1"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if n := strings.Count(strings.Join(f.calls, " ")+" ", "HmsSessionManagerLogin "); n != 2 {
		t.Fatalf("logged in %d times: %v", n, f.calls)
	}
}

func TestFaultMapping(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	g := &Group{Ref: MoRef{Type: "HmsPrimaryGroup", Value: "GID-2"}, Name: "cns-vol1"}
	if err := c.OnlineSync(ctx, g); !errors.Is(err, ErrSyncOngoing) {
		t.Fatalf("online sync: %v", err)
	}
	err := c.RecoverGroup(ctx, g, RecoverSpec{
		Folder: MoRef{Type: "Folder", Value: "group-v4"}, Pool: MoRef{Type: "ResourcePool", Value: "resgroup-9"},
		Type: RecoveryPlannedMigration,
	})
	if !errors.Is(err, ErrAlreadyRecovered) {
		t.Fatalf("recover: %v", err)
	}
	req := f.bodies["HmsSecondaryGroupRecoverGroup_Task"]
	for _, want := range []string{
		`<_this type="HmsSecondaryGroup">GID-2</_this>`, `<folder type="Folder">group-v4</folder>`,
		`<sync>false</sync>`, `<recoveryType>plannedMigration</recoveryType>`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request lacks %s:\n%s", want, req)
		}
	}
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"dpva.example.com":            "https://dpva.example.com/vrms",
		"https://dpva.example.com":    "https://dpva.example.com/vrms",
		"https://dpva:8443/vrms":      "https://dpva:8443/vrms",
		"https://dpva.example.com/hm": "https://dpva.example.com/hm",
	} {
		got, err := endpointURL(in)
		if err != nil || got != want {
			t.Errorf("endpointURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// resetTaskPolls makes the next task of the fake HMS take two polls.
func (f *fakeHMS) resetTaskPolls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskPolls = 0
}

func TestRecoveryCheckpointCalls(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	g := &Group{Ref: MoRef{Type: "HmsGroup", Value: "GID-2"}, Name: "cns-vol1",
		Peer: &MoRef{Type: "HmsRemoteHmsServer", Value: "peer-1"}}
	if err := c.PrepareRecovery(ctx, g, testOperationID); err != nil {
		t.Fatal(err)
	}
	f.resetTaskPolls()
	if err := c.SealRecovery(ctx, g, testOperationID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"HmsPrepareCnsRecovery_Task", "HmsSealCnsRecovery_Task"} {
		want := `<_this type="HmsPrimaryGroup">GID-2</_this><operationId>` + testOperationID + `</operationId>`
		if !strings.Contains(f.bodies[m], want) {
			t.Errorf("%s lacks %s:\n%s", m, want, f.bodies[m])
		}
	}
	// The checkpoint calls read the replica instances from the peer.
	if f.bodies["HmsSessionManagerLoginHmsServerByToken"] == "" {
		t.Error("no peer login")
	}

	disk, err := c.RecoveredDisk(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if *disk != (DatastorePath{Datastore: MoRef{Type: "Datastore", Value: "datastore-34"},
		Path: "hbr-pe-gid-GID-2", FileName: "cnsVolume.vmdk"}) ||
		disk.RelativePath() != "hbr-pe-gid-GID-2/cnsVolume.vmdk" {
		t.Fatalf("disk %+v", disk)
	}
	if !strings.Contains(f.bodies["HmsGetRecoveredCnsDisk"], `<_this type="HmsSecondaryGroup">GID-2</_this>`) {
		t.Fatalf("request %s", f.bodies["HmsGetRecoveredCnsDisk"])
	}
	f.resetTaskPolls()
	if err := c.BindRecoveredVolume(ctx, g, "vol1", disk.Datastore); err != nil {
		t.Fatal(err)
	}
	if want := `<volumeUuid>vol1</volumeUuid><datastore type="Datastore">datastore-34</datastore>`; !strings.Contains(
		f.bodies["HmsBindRecoveredCnsVolume_Task"], want) {
		t.Fatalf("bind request lacks %s:\n%s", want, f.bodies["HmsBindRecoveredCnsVolume_Task"])
	}
}

func TestReprotectCalls(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	g := &Group{Ref: MoRef{Type: "HmsGroup", Value: "GID-2"}, Name: "cns-vol1",
		Peer: &MoRef{Type: "HmsRemoteHmsServer", Value: "peer-1"}}
	err := c.StartReprotect(ctx, g, ReprotectSpec{OperationID: testOperationID, VolumeID: "vol1",
		Datastore: MoRef{Type: "Datastore", Value: "datastore-34"}})
	if err != nil {
		t.Fatal(err)
	}
	req := f.bodies["HmsReverseConfigurePeReplication_Task"]
	want := `<_this type="HmsSecondaryGroup">GID-2</_this><spec xsi:type="HmsCnsPeReprotectSpec">` +
		`<operationId>` + testOperationID + `</operationId><recoveredVolumeUuid>vol1</recoveredVolumeUuid>` +
		`<recoveredDatastore type="Datastore">datastore-34</recoveredDatastore>` +
		`<forceFullSync>false</forceFullSync></spec>`
	if !strings.Contains(req, want) {
		t.Fatalf("request lacks %s:\n%s", want, req)
	}
	if f.taskPolls != 0 {
		t.Fatal("waited for the reprotect task")
	}

	op, err := c.ReprotectOperation(ctx, testOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase != ReprotectPhaseComplete || op.SynchronizationMode != "optimized" || op.OriginalGroupID != "GID-2" ||
		op.ReverseGroup == nil || op.ReverseGroup.Value != "GID-9" {
		t.Fatalf("operation %+v", op)
	}
	if _, err := c.ReprotectOperation(ctx, "11111111-2222-4333-8444-555555555555"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown operation: %v", err)
	}
	if err := c.ResumeReprotect(ctx, nil, testOperationID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.bodies["HmsResumePeReprotectOperation_Task"],
		`<_this type="HmsReplicationManager">replication-manager</_this><operationId>`) {
		t.Fatalf("resume request %s", f.bodies["HmsResumePeReprotectOperation_Task"])
	}
}

func TestPeerClientSwapsCredentials(t *testing.T) {
	c, _ := newTestClient(t)
	if _, err := c.Peer(context.Background()); !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("peer without endpoint: %v", err)
	}
	peerHMS := &fakeHMS{t: t, bodies: map[string]string{}}
	server := httptest.NewServer(peerHMS)
	t.Cleanup(server.Close)
	c.cfg.PeerEndpoint = server.URL + "/vrms"
	p, err := c.Peer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := &Group{Ref: MoRef{Value: "GID-2"}, Name: "cns-vol1", Peer: &MoRef{Type: "HmsRemoteHmsServer", Value: "peer-1"}}
	if err := p.StartReprotect(context.Background(), g, ReprotectSpec{OperationID: testOperationID}); err != nil {
		t.Fatal(err)
	}
	// The peer HMS is logged in to with a token from the peer vCenter, and
	// reaches back to this site with a token from the local one.
	if !strings.Contains(peerHMS.bodies["HmsSessionManagerLogin"], `ID="vc-b"`) {
		t.Errorf("peer login: %s", peerHMS.bodies["HmsSessionManagerLogin"])
	}
	if !strings.Contains(peerHMS.bodies["HmsSessionManagerLoginHmsServerByToken"], `ID=&#34;vc-a&#34;`) {
		t.Errorf("peer's login back: %s", peerHMS.bodies["HmsSessionManagerLoginHmsServerByToken"])
	}
	if again, _ := c.Peer(context.Background()); again != p {
		t.Error("peer client is not cached")
	}
}

func TestMissingMethodIsNotSupported(t *testing.T) {
	err := error(&Fault{Type: "InvalidRequest", Message: "Invalid managed method 'HmsGetRecoveredCnsDisk' " +
		"requested for Managed Object Type 'HmsSecondaryGroup' version 'hmssrv/9.2.1'"})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatal("missing method is not ErrNotSupported")
	}
	if errors.Is(&Fault{Type: "InvalidRequest", Message: "Request missing value"}, ErrNotSupported) {
		t.Fatal("other invalid requests are ErrNotSupported")
	}
}
