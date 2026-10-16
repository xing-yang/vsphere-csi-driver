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

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultPollInterval = 2 * time.Second

// RESTConfig configures a RESTClient.
type RESTConfig struct {
	// Endpoint is the SnapService base URL, without the /api/snapservice suffix.
	Endpoint string
	// Site is the SnapService site name of the endpoint.
	Site string
	// Cluster is the vSphere cluster that owns the protection groups.
	Cluster string
	// Username and Password create a session. Both empty means no session.
	Username string
	Password string
	// InsecureSkipVerify disables TLS verification.
	InsecureSkipVerify bool
	// PollInterval is the task polling interval.
	PollInterval time.Duration
	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client
}

// RESTClient implements Client with the SnapService REST API.
type RESTClient struct {
	cfg  RESTConfig
	http *http.Client

	mu      sync.Mutex
	session string
}

var _ Client = &RESTClient{}

// NewRESTClient returns a REST client.
func NewRESTClient(cfg RESTConfig) (*RESTClient, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", cfg.Endpoint, err)
	}
	if cfg.Site == "" {
		return nil, errors.New("site is required")
	}
	if cfg.Cluster == "" {
		return nil, errors.New("cluster is required")
	}
	cfg.Endpoint = strings.TrimSuffix(cfg.Endpoint, "/")
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultPollInterval
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.InsecureSkipVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Opt-in for labs.
		}
		httpClient = &http.Client{Transport: transport, Timeout: time.Minute}
	}
	return &RESTClient{cfg: cfg, http: httpClient}, nil
}

// LocalSite returns the configured site name.
func (c *RESTClient) LocalSite() string {
	return c.cfg.Site
}

// IsSitePaired returns whether the site is in the list of paired sites.
func (c *RESTClient) IsSitePaired(ctx context.Context, site string) (bool, error) {
	var sites []wireSite
	if err := c.do(ctx, http.MethodGet, "/sites", nil, nil, &sites); err != nil {
		return false, err
	}
	for _, s := range sites {
		if s.Site == site || s.Name == site {
			return true, nil
		}
	}
	return false, nil
}

// GetProtectionGroup finds a protection group by name.
// TODO: Use a server-side name filter once the 9.2 FilterSpec is known.
func (c *RESTClient) GetProtectionGroup(ctx context.Context, name string) (*ProtectionGroup, error) {
	var pgs []wireProtectionGroup
	if err := c.do(ctx, http.MethodGet, c.pgPath(""), nil, nil, &pgs); err != nil {
		return nil, err
	}
	for i := range pgs {
		if pgs[i].Name == name {
			return fromWirePG(&pgs[i]), nil
		}
	}
	return nil, ErrNotFound
}

// CreateProtectionGroup creates a protection group and returns it.
func (c *RESTClient) CreateProtectionGroup(ctx context.Context, spec ProtectionGroupSpec) (*ProtectionGroup, error) {
	if _, err := c.runTask(ctx, http.MethodPost, c.pgPath(""), nil, toWireSpec(spec)); err != nil {
		return nil, err
	}
	return c.GetProtectionGroup(ctx, spec.Name)
}

// DeleteProtectionGroup deletes a protection group.
func (c *RESTClient) DeleteProtectionGroup(ctx context.Context, id string) error {
	_, err := c.runTask(ctx, http.MethodDelete, c.pgPath(id), nil, nil)
	return err
}

// Promote promotes a protection group.
func (c *RESTClient) Promote(ctx context.Context, id string, force bool) (*PromoteResult, error) {
	t, err := c.runTask(ctx, http.MethodPost, c.pgPath(id), url.Values{"action": {"promote"}},
		wirePromoteSpec{Force: force})
	if err != nil {
		return nil, err
	}
	res := &PromoteResult{}
	if t.Result != nil {
		for _, d := range t.Result.RecoveredDisks {
			res.RecoveredDisks = append(res.RecoveredDisks, RecoveredDisk(d))
		}
	}
	return res, nil
}

// Demote demotes a protection group.
func (c *RESTClient) Demote(ctx context.Context, id string) error {
	_, err := c.runTask(ctx, http.MethodPost, c.pgPath(id), url.Values{"action": {"demote"}}, nil)
	return err
}

// Activate re-protects a demoted protection group.
func (c *RESTClient) Activate(ctx context.Context, id string) error {
	_, err := c.runTask(ctx, http.MethodPost, c.pgPath(id), url.Values{"action": {"activate"}}, nil)
	return err
}

func (c *RESTClient) pgPath(id string) string {
	p := "/clusters/" + url.PathEscape(c.cfg.Cluster) + "/protection-groups"
	if id != "" {
		p += "/" + url.PathEscape(id)
	}
	return p
}

// runTask starts an operation as a task and waits for it to finish.
func (c *RESTClient) runTask(ctx context.Context, method, path string, query url.Values,
	body interface{}) (*wireTask, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("vmw-task", "true")
	var taskID string
	if err := c.do(ctx, method, path, query, body, &taskID); err != nil {
		return nil, err
	}
	for {
		var t wireTask
		if err := c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(taskID), nil, nil, &t); err != nil {
			return nil, err
		}
		switch t.Status {
		case taskStatusSucceeded:
			return &t, nil
		case taskStatusFailed:
			if t.Error == nil {
				return nil, fmt.Errorf("snapservice: task %s failed", taskID)
			}
			return nil, fromWireError(t.Error)
		case taskStatusPending, taskStatusRunning:
		default:
			return nil, fmt.Errorf("snapservice: task %s has unknown status %q", taskID, t.Status)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for task %s: %w", taskID, ctx.Err())
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// do sends a request and decodes the JSON response into out. It logs in when
// credentials are configured and retries once if the session expired.
func (c *RESTClient) do(ctx context.Context, method, path string, query url.Values,
	body, out interface{}) error {
	for attempt := 0; ; attempt++ {
		session, err := c.getSession(ctx)
		if err != nil {
			return err
		}
		status, data, err := c.send(ctx, method, path, query, body, session)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && session != "" && attempt == 0 {
			c.mu.Lock()
			c.session = ""
			c.mu.Unlock()
			continue
		}
		if status >= 300 {
			var we wireError
			if json.Unmarshal(data, &we) == nil && we.ErrorType != "" {
				return fromWireError(&we)
			}
			return fmt.Errorf("snapservice: %s %s returned %d: %s", method, path, status, bytes.TrimSpace(data))
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("snapservice: decoding %s %s response: %w", method, path, err)
		}
		return nil
	}
}

func (c *RESTClient) send(ctx context.Context, method, path string, query url.Values, body interface{},
	session string) (int, []byte, error) {
	u := c.cfg.Endpoint + apiPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("snapservice: %s %s: %v: %w", method, path, err, ErrSiteUnavailable)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// getSession returns the current session, logging in if needed.
func (c *RESTClient) getSession(ctx context.Context) (string, error) {
	if c.cfg.Username == "" && c.cfg.Password == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		return c.session, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+apiPrefix+"/sessions", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("snapservice: login: %v: %w", err, ErrSiteUnavailable)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("snapservice: login returned %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	var session string
	if err := json.Unmarshal(data, &session); err != nil || session == "" {
		return "", fmt.Errorf("snapservice: unexpected login response %q", bytes.TrimSpace(data))
	}
	c.session = session
	return session, nil
}
