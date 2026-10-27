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
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/sts"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

const (
	pageSize         = 500
	taskPollInterval = 2 * time.Second
	tokenLifetime    = 30 * time.Minute
)

// Credentials are SSO credentials for one vCenter. HMS accepts a delegable
// bearer SAML token issued by the vCenter STS of its own site.
type Credentials struct {
	// VCHost is the vCenter host (and optional port) whose STS issues tokens.
	VCHost   string
	User     string
	Password string
}

// Config configures a SOAPClient.
type Config struct {
	// Endpoint is the HMS URL, for example "https://dpva.example.com/vrms".
	// A bare host gets the https scheme and DefaultPath.
	Endpoint string
	// Namespace and Version default to DefaultNamespace and DefaultVersion.
	Namespace string
	Version   string
	Insecure  bool
	// Local logs in to the local HMS.
	Local Credentials
	// Peer logs in to the paired HMS through the local one. It is needed to
	// configure and unconfigure replication.
	Peer Credentials
}

// TokenIssuer returns a delegable bearer SAML assertion from the STS of a vCenter.
type TokenIssuer func(ctx context.Context, creds Credentials, insecure bool) (string, error)

// SOAPClient is a Client for a real HMS.
type SOAPClient struct {
	cfg         Config
	issueToken  TokenIssuer
	httpClient  *http.Client
	transport   *soapTransport
	mu          sync.Mutex
	loggedIn    bool
	peerSession map[string]bool
}

var _ Client = &SOAPClient{}

// NewSOAPClient returns an HMS client. It logs in on first use.
func NewSOAPClient(cfg Config) (*SOAPClient, error) {
	u, err := endpointURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.Version == "" {
		cfg.Version = DefaultVersion
	}
	if cfg.Local.User == "" || cfg.Local.Password == "" || cfg.Local.VCHost == "" {
		return nil, errors.New("local vCenter host and SSO credentials are required")
	}
	c := &SOAPClient{
		cfg:         cfg,
		issueToken:  IssueBearerToken,
		peerSession: map[string]bool{},
	}
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// #nosec G402 -- POC: DPVA certificates are often self-signed.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure},
		},
		Timeout: 5 * time.Minute,
	}
	c.resetSession()
	c.transport = &soapTransport{httpClient: c.httpClient, url: u, namespace: cfg.Namespace, version: cfg.Version}
	return c, nil
}

// WithTokenIssuer replaces the STS token issuer, for tests.
func (c *SOAPClient) WithTokenIssuer(issuer TokenIssuer) *SOAPClient {
	c.issueToken = issuer
	return c
}

func endpointURL(endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("HMS endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid HMS endpoint %q: %w", endpoint, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = DefaultPath
	}
	return u.String(), nil
}

// resetSession drops the session cookie and peer sessions. Callers hold mu,
// except during construction.
func (c *SOAPClient) resetSession() {
	jar, _ := cookiejar.New(nil)
	c.httpClient.Jar = jar
	c.loggedIn = false
	c.peerSession = map[string]bool{}
}

// LocalSite returns the HMS endpoint.
func (c *SOAPClient) LocalSite() string { return c.cfg.Endpoint }

// IssueBearerToken gets a delegable bearer token from the vCenter STS.
func IssueBearerToken(ctx context.Context, creds Credentials, insecure bool) (string, error) {
	u, err := soap.ParseURL(creds.VCHost)
	if err != nil {
		return "", err
	}
	vc, err := vim25.NewClient(ctx, soap.NewClient(u, insecure))
	if err != nil {
		return "", fmt.Errorf("connect to vCenter %s: %w", creds.VCHost, err)
	}
	stsClient, err := sts.NewClient(ctx, vc)
	if err != nil {
		return "", fmt.Errorf("create STS client for %s: %w", creds.VCHost, err)
	}
	signer, err := stsClient.Issue(ctx, sts.TokenRequest{
		Userinfo:    url.UserPassword(creds.User, creds.Password),
		Lifetime:    tokenLifetime,
		Delegatable: true,
	})
	if err != nil {
		return "", fmt.Errorf("issue token from %s for %s: %w", creds.VCHost, creds.User, err)
	}
	return signer.Token, nil
}

// ensureLogin logs in to the local HMS if needed. Callers hold mu.
func (c *SOAPClient) ensureLogin(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	token, err := c.issueToken(ctx, c.cfg.Local, c.cfg.Insecure)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	header := bearerSecurityHeader(token, now.Format(time.RFC3339), now.Add(5*time.Minute).Format(time.RFC3339))
	if err := login(ctx, c.transport, header); err != nil && !isFault(err, "AlreadyLoggedIn") {
		return fmt.Errorf("log in to HMS %s: %w", c.cfg.Endpoint, err)
	}
	c.loggedIn = true
	return nil
}

// ensurePeer extends the session to the peer HMS. Callers hold mu.
func (c *SOAPClient) ensurePeer(ctx context.Context, peer MoRef) error {
	if c.peerSession[peer.Value] {
		return nil
	}
	if c.cfg.Peer.User == "" || c.cfg.Peer.Password == "" || c.cfg.Peer.VCHost == "" {
		return fmt.Errorf("%w: peer vCenter host and SSO credentials are not configured", ErrPeerUnreachable)
	}
	// The peer validates the token against its own SSO, so it must come from
	// the peer vCenter's STS.
	token, err := c.issueToken(ctx, c.cfg.Peer, c.cfg.Insecure)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPeerUnreachable, err)
	}
	if err := loginHmsServerByToken(ctx, c.transport, peer, token); err != nil && !isFault(err, "AlreadyLoggedIn") {
		return fmt.Errorf("log in to peer HMS %s: %w", peer.Value, err)
	}
	c.peerSession[peer.Value] = true
	return nil
}

// do runs fn with a logged-in session and retries once after a re-login if
// the session expired. The lock only covers logging in, so that long tasks
// for different volumes run in parallel on the shared session.
func (c *SOAPClient) do(ctx context.Context, peer *MoRef, fn func() error) error {
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		err := c.ensureLogin(ctx)
		if err == nil && peer != nil {
			err = c.ensurePeer(ctx, *peer)
		}
		c.mu.Unlock()
		if err == nil {
			err = fn()
		}
		if attempt == 0 && errors.Is(err, ErrNotAuthenticated) {
			c.mu.Lock()
			c.resetSession()
			c.mu.Unlock()
			continue
		}
		return err
	}
}

func isFault(err error, name string) bool {
	var f *Fault
	return errors.As(err, &f) && strings.Contains(f.Type, name)
}

// waitForTask polls an HMS task until it completes.
func (c *SOAPClient) waitForTask(ctx context.Context, task MoRef, op string) (*MoRef, error) {
	for {
		info, err := getTaskInfo(ctx, c.transport, task)
		if err != nil {
			return nil, fmt.Errorf("%s: get task %s: %w", op, task.Value, err)
		}
		switch info.State {
		case "success":
			return info.Result, nil
		case "error":
			f := &Fault{Message: "task failed"}
			if info.Error != nil {
				f.Type = info.Error.Fault.Type
				if i := strings.IndexByte(f.Type, ':'); i >= 0 {
					f.Type = f.Type[i+1:]
				}
				f.Message = info.Error.LocalizedMessage
			}
			return nil, fmt.Errorf("%s: %w", op, f)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: task %s still running: %w", op, task.Value, ctx.Err())
		case <-time.After(taskPollInterval):
		}
	}
}

func (c *SOAPClient) findGroup(ctx context.Context, outgoing bool, name string) (*Group, error) {
	var found *Group
	err := c.do(ctx, nil, func() error {
		for start := 0; ; start += pageSize {
			page, err := listReplications(ctx, c.transport, outgoing, start, pageSize)
			if err != nil {
				return err
			}
			for _, info := range page.Returnval.Info {
				if info.Name == name {
					found = groupFromTabular(&info, outgoing)
					return nil
				}
			}
			if len(page.Returnval.Info) < pageSize || start+pageSize >= page.Returnval.TotalCount {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("group %q: %w", name, ErrNotFound)
	}
	return found, nil
}

func groupFromTabular(info *tabularInfo, outgoing bool) *Group {
	g := &Group{
		Ref:         info.Replication,
		Name:        info.Name,
		Status:      info.Status,
		RPO:         time.Duration(info.RPO) * time.Minute,
		PeerHealthy: info.PeerHealthy,
	}
	peerSite := info.TargetSite
	if !outgoing {
		peerSite = info.SourceSite
	}
	g.Peer = peerSite.HmsServer
	return g
}

// FindOutgoingGroup implements Client.
func (c *SOAPClient) FindOutgoingGroup(ctx context.Context, name string) (*Group, error) {
	return c.findGroup(ctx, true, name)
}

// FindIncomingGroup implements Client.
func (c *SOAPClient) FindIncomingGroup(ctx context.Context, name string) (*Group, error) {
	return c.findGroup(ctx, false, name)
}

// findPeer returns the paired HMS server whose vCenter site name or server
// name is site.
func (c *SOAPClient) findPeer(ctx context.Context, site string) (*MoRef, error) {
	var peer *MoRef
	err := c.do(ctx, nil, func() error {
		servers, err := getHmsServers(ctx, c.transport)
		if err != nil {
			return err
		}
		var names []string
		for i := range servers {
			info, err := getServerInfo(ctx, c.transport, servers[i])
			if err != nil {
				return err
			}
			if info.PairedVcSiteName == site || info.Name == site {
				peer = &servers[i]
				return nil
			}
			names = append(names, info.PairedVcSiteName)
		}
		return fmt.Errorf("site %q is not paired with HMS %s (paired: %v): %w",
			site, c.cfg.Endpoint, names, ErrNoPeer)
	})
	return peer, err
}

// ConfigureCnsVolumeReplication implements Client.
func (c *SOAPClient) ConfigureCnsVolumeReplication(ctx context.Context,
	spec CnsVolumeReplicationSpec) (*Group, error) {
	peer, err := c.findPeer(ctx, spec.TargetSite)
	if err != nil {
		return nil, err
	}
	rpoMinutes := int64((spec.RPO + time.Minute - 1) / time.Minute)
	wire := cnsVolumeReplicationSpec{
		Settings:               groupSettings{Name: spec.Name, RPO: rpoMinutes},
		VolumeUUID:             spec.VolumeID,
		SourceDatastoreRef:     spec.SourceDatastore,
		TargetDatastoreRef:     spec.TargetDatastore,
		TargetStorageProfileID: spec.TargetStorageProfileID,
	}
	var group *Group
	err = c.do(ctx, peer, func() error {
		task, err := configureCnsVolumeReplication(ctx, c.transport, *peer, wire)
		if err != nil {
			return err
		}
		res, err := c.waitForTask(ctx, task, "configure replication")
		if err != nil {
			return err
		}
		group = &Group{Name: spec.Name, Status: StatusConfiguring, RPO: time.Duration(rpoMinutes) * time.Minute,
			Peer: peer}
		if res != nil {
			group.Ref = *res
		}
		return nil
	})
	return group, err
}

// GetPolicyProgress implements Client.
func (c *SOAPClient) GetPolicyProgress(ctx context.Context, group *Group) (*PolicyProgress, error) {
	var p *policyProgress
	err := c.do(ctx, nil, func() error {
		var err error
		p, err = getPolicyProgress(ctx, c.transport, group.Ref)
		return err
	})
	if err != nil || p == nil {
		return nil, err
	}
	out := &PolicyProgress{
		CurrentGeneration:    p.CurrentGeneration,
		LastSyncBytes:        p.LastSyncBytesTransferred,
		LiveBytesTransferred: p.LiveBytesTransferred,
		LiveBytesToTransfer:  p.LiveBytesToTransfer,
		Health:               p.Health,
	}
	if p.LastSyncTimeMs != nil {
		d := time.Duration(*p.LastSyncTimeMs) * time.Millisecond
		out.LastSyncDuration = &d
	}
	return out, nil
}

// OnlineSync implements Client.
func (c *SOAPClient) OnlineSync(ctx context.Context, group *Group) error {
	return c.do(ctx, nil, func() error {
		task, err := onlineSync(ctx, c.transport, group.Ref)
		if err != nil {
			return err
		}
		_, err = c.waitForTask(ctx, task, "online sync")
		return err
	})
}

// Unconfigure implements Client. It needs a session to the peer HMS.
func (c *SOAPClient) Unconfigure(ctx context.Context, group *Group, preserveReplicaDisks bool) error {
	return c.do(ctx, group.Peer, func() error {
		task, err := unconfigure(ctx, c.transport, group.Ref, preserveReplicaDisks)
		if err != nil {
			return err
		}
		_, err = c.waitForTask(ctx, task, "unconfigure replication")
		return err
	})
}

// RecoverGroup implements Client. It runs on the local (secondary) HMS only.
func (c *SOAPClient) RecoverGroup(ctx context.Context, group *Group, spec RecoverSpec) error {
	return c.do(ctx, nil, func() error {
		task, err := recoverGroup(ctx, c.transport, group.Ref, spec)
		if err != nil {
			return err
		}
		_, err = c.waitForTask(ctx, task, "recover group")
		return err
	})
}

// GroupDatastore implements Client.
func (c *SOAPClient) GroupDatastore(ctx context.Context, group *Group) (MoRef, error) {
	var ds MoRef
	err := c.do(ctx, nil, func() error {
		res, err := currentSpec(ctx, c.transport, group.Ref)
		if err != nil {
			return err
		}
		if res.Returnval.TargetDatastoreRef == nil {
			return fmt.Errorf("group %s has no target datastore in its spec", group.Name)
		}
		ds = *res.Returnval.TargetDatastoreRef
		return nil
	})
	return ds, err
}
