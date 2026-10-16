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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestREST(t *testing.T, handler http.Handler, site string) *RESTClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := NewRESTClient(RESTConfig{
		Endpoint:     server.URL + "/" + site,
		Site:         site,
		Cluster:      "domain-c1",
		Username:     "user",
		Password:     "pass",
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRESTSessionAndRelogin(t *testing.T) {
	fabric := NewFakeFabric([]string{"a", "b"})
	inner := NewFakeServer(fabric).Handler()
	var mu sync.Mutex
	logins := 0
	expired := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sessions") {
			if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			logins++
			inner.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(sessionHeader) == "" || expired {
			expired = false
			writeJSON(w, http.StatusUnauthorized, &wireError{ErrorType: "UNAUTHENTICATED"})
			return
		}
		inner.ServeHTTP(w, r)
	})
	c := newTestREST(t, handler, "a")
	ctx := context.Background()

	if paired, err := c.IsSitePaired(ctx, "b"); err != nil || !paired {
		t.Fatalf("IsSitePaired = %t, %v", paired, err)
	}
	mu.Lock()
	expired = true
	mu.Unlock()
	if _, err := c.GetProtectionGroup(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after re-login, got %v", err)
	}
	if logins != 2 {
		t.Fatalf("logins = %d, want 2", logins)
	}
}

func TestRESTErrorsMapToSentinels(t *testing.T) {
	fabric := NewFakeFabric([]string{"a", "b"})
	handler := NewFakeServer(fabric).Handler()
	a := newTestREST(t, handler, "a")
	b := newTestREST(t, handler, "b")
	ctx := context.Background()

	spec := ProtectionGroupSpec{
		Name:           "cns-vol",
		TargetEntities: TargetEntities{FCDs: []string{"vol"}},
		Replication:    &ReplicationPolicy{TargetSite: "b", RPO: 15 * time.Minute},
	}
	pg, err := a.CreateProtectionGroup(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if pg.State != StateActive || pg.Replication.RPO != 15*time.Minute || len(pg.TargetEntities.FCDs) != 1 {
		t.Fatalf("unexpected protection group %+v", pg)
	}
	if _, err := a.CreateProtectionGroup(ctx, spec); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
	if _, err := a.CreateProtectionGroup(ctx, ProtectionGroupSpec{Name: "x",
		Replication: &ReplicationPolicy{TargetSite: "c"}}); !errors.Is(err, ErrNoSitePair) {
		t.Fatalf("expected ErrNoSitePair, got %v", err)
	}

	replica, err := b.GetProtectionGroup(ctx, "cns-vol")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Promote(ctx, replica.ID, true); !errors.Is(err, ErrPeerActive) {
		t.Fatalf("expected ErrPeerActive, got %v", err)
	}
	if err := b.Activate(ctx, replica.ID); err != nil {
		t.Fatalf("activate on a dormant group should be a no-op: %v", err)
	}

	fabric.SetSiteDown("a", true)
	if _, err := b.Promote(ctx, replica.ID, false); !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("expected ErrPeerUnreachable, got %v", err)
	}
	if _, err := a.GetProtectionGroup(ctx, "cns-vol"); !errors.Is(err, ErrSiteUnavailable) {
		t.Fatalf("expected ErrSiteUnavailable, got %v", err)
	}
	res, err := b.Promote(ctx, replica.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RecoveredDisks) != 1 || res.RecoveredDisks[0].SourceID != "vol" ||
		res.RecoveredDisks[0].DiskURLPath == "" {
		t.Fatalf("unexpected promote result %+v", res)
	}
}

func TestRESTUnreachableEndpoint(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL + "/a"
	server.Close()
	c, err := NewRESTClient(RESTConfig{Endpoint: endpoint, Site: "a", Cluster: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetProtectionGroup(context.Background(), "x"); !errors.Is(err, ErrSiteUnavailable) {
		t.Fatalf("expected ErrSiteUnavailable, got %v", err)
	}
}

func TestRESTConfigValidation(t *testing.T) {
	for _, cfg := range []RESTConfig{
		{Site: "a", Cluster: "c"},
		{Endpoint: "http://x", Cluster: "c"},
		{Endpoint: "http://x", Site: "a"},
	} {
		if _, err := NewRESTClient(cfg); err == nil {
			t.Errorf("NewRESTClient(%+v) should fail", cfg)
		}
	}
}
