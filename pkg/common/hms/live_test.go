//go:build hmslive

package hms

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// dumpTransport prints response bodies only; requests carry SAML tokens.
type dumpTransport struct {
	rt http.RoundTripper
	t  *testing.T
}

func (d dumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := d.rt.RoundTrip(req)
	if err != nil || req.URL.Path != DefaultPath {
		return res, err
	}
	body, _ := io.ReadAll(res.Body)
	res.Body = io.NopCloser(bytes.NewReader(body))
	d.t.Logf("RESPONSE %d: %s", res.StatusCode, truncate(string(body), 3000))
	return res, nil
}

// Run: HMS_LIVE_ENDPOINT=... HMS_LIVE_VC=... HMS_LIVE_PEER_VC=... HMS_LIVE_USER=...
// HMS_LIVE_PWD=... HMS_LIVE_PEER_PWD=... go test -tags hmslive -run TestLive -v ./pkg/common/hms/
func TestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user := os.Getenv("HMS_LIVE_USER")
	c, err := NewSOAPClient(Config{
		Endpoint: os.Getenv("HMS_LIVE_ENDPOINT"),
		Insecure: true,
		Local:    Credentials{VCHost: os.Getenv("HMS_LIVE_VC"), User: user, Password: os.Getenv("HMS_LIVE_PWD")},
		Peer:     Credentials{VCHost: os.Getenv("HMS_LIVE_PEER_VC"), User: user, Password: os.Getenv("HMS_LIVE_PEER_PWD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HMS_LIVE_DUMP") != "" {
		c.httpClient.Transport = dumpTransport{rt: c.httpClient.Transport, t: t}
	}

	c.mu.Lock()
	err = c.ensureLogin(ctx)
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Logf("login OK")

	servers, err := getHmsServers(ctx, c.transport)
	if err != nil {
		t.Fatalf("GetHmsServersInternal: %v", err)
	}
	t.Logf("paired servers: %+v", servers)
	for _, s := range servers {
		info, err := getServerInfo(ctx, c.transport, s)
		if err != nil {
			t.Fatalf("GetInfoInternal %v: %v", s, err)
		}
		t.Logf("server %v: %+v", s, *info)
	}

	if site := os.Getenv("HMS_LIVE_PEER_SITE"); site != "" {
		peer, err := c.findPeer(ctx, site)
		if err != nil {
			t.Fatalf("findPeer: %v", err)
		}
		c.mu.Lock()
		err = c.ensurePeer(ctx, *peer)
		c.mu.Unlock()
		if err != nil {
			t.Fatalf("peer login: %v", err)
		}
		t.Logf("peer login to %v OK", *peer)
	}

	for _, outgoing := range []bool{true, false} {
		page, err := listReplications(ctx, c.transport, outgoing, 0, pageSize)
		if err != nil {
			t.Fatalf("list outgoing=%v: %v", outgoing, err)
		}
		t.Logf("outgoing=%v total=%d", outgoing, page.Returnval.TotalCount)
		for _, i := range page.Returnval.Info {
			t.Logf("  %+v", i)
			if os.Getenv("HMS_LIVE_PROGRESS") != "" {
				if p, err := getPolicyProgress(ctx, c.transport, i.Replication); err != nil {
					t.Logf("    progress: %v", err)
				} else if p != nil {
					t.Logf("    progress: gen=%v lastSyncMs=%v lastBytes=%v live=%v/%v health=%s", deref(p.CurrentGeneration),
						deref(p.LastSyncTimeMs), deref(p.LastSyncBytesTransferred), deref(p.LiveBytesTransferred),
						deref(p.LiveBytesToTransfer), p.Health)
				}
				if res, err := currentSpec(ctx, c.transport, i.Replication); err != nil {
					t.Logf("    currentSpec: %v", err)
				} else if res.Returnval.TargetDatastoreRef != nil {
					t.Logf("    currentSpec target datastore: %v", *res.Returnval.TargetDatastoreRef)
				}
			}
		}
	}
}

func deref(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}
