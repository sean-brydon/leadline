package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
)

// The attach API as `leadline attach` and the app use it: a worktree
// becomes a source with connections it owns, every answer says who owns
// what, and none of them holds a password.
func TestAttachAPI(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	pg.AddDatabase("acme_fix_orders")
	c := newIntegrationsClient(t)
	url := strings.Replace(pg.URL(), "/acme_dev", "/acme_fix_orders", 1)

	// Nothing attached, and the list is a list.
	var listed struct {
		Attached []integrations.AttachedView `json:"attached"`
	}
	var raw map[string]json.RawMessage
	c.ok("GET", "/api/attach", "", &raw)
	if string(raw["attached"]) != "[]" {
		t.Errorf("attached = %s, want []", raw["attached"])
	}
	c.ok("GET", "/api/sources", "", &raw)
	if string(raw["attached"]) != "[]" {
		t.Errorf("sources.attached = %s, want []", raw["attached"])
	}
	c.fails("GET", "/api/attach?service=acme/fix-orders", "", http.StatusNotFound, "not_found")

	// A connection a person added, to the same server.
	var mine integrations.ConnectionView
	c.ok("POST", "/api/connections", jsonBody(map[string]any{"url": pg.URL(), "name": "Acme dev"}), &mine)
	if mine.Owner.Kind != "user" || mine.Owner.Label != "added by hand" || mine.Env == nil {
		t.Errorf("a person's connection: owner %+v, env %v", mine.Owner, mine.Env)
	}

	body := jsonBody(map[string]any{
		"service": "acme/fix-orders", "via": "shipyard", "project": "acme", "worktree": "fix-orders",
		"branch": "fix-orders", "box": "devl", "path": "/home/dev/work/acme-fix-orders",
		"connections": []map[string]any{
			{"env": []string{"DATABASE_URL"}, "url": url},
			{"env": []string{"REDIS_URL"}, "url": rd.URL()},
		},
	})
	var got integrations.AttachResult
	c.ok("POST", "/api/attach", body, &got)
	if !got.Created || !got.Changed || got.Added != 2 || got.Service != "acme/fix-orders" || len(got.Connections) != 2 {
		t.Fatalf("attach: %+v", got)
	}
	if o := got.Connections[0].Owner; o.Kind != "worktree" || o.Via != "shipyard" || o.Project != "acme" || o.Worktree != "fix-orders" ||
		o.Branch != "fix-orders" || o.Box != "devl" || o.Label != "from Shipyard, worktree fix-orders" {
		t.Errorf("owner: %+v", o)
	}
	if p := got.Connections[0].Proxy; p == nil || p.State != "listening" || p.Owner.Kind != "worktree" || p.Label != "acme/fix-orders" {
		t.Errorf("the worktree's proxy: %+v", p)
	}
	var again integrations.AttachResult
	c.ok("POST", "/api/attach", body, &again)
	if again.Created || again.Changed || again.Unchanged != 2 || again.Connections[0].ID != got.Connections[0].ID {
		t.Errorf("attaching twice: %+v", again)
	}

	// One, and all.
	var one struct {
		Source      integrations.AttachedView     `json:"source"`
		Connections []integrations.ConnectionView `json:"connections"`
	}
	c.ok("GET", "/api/attach?service=acme%2Ffix-orders", "", &one)
	if one.Source.Service != "acme/fix-orders" || len(one.Connections) != 2 || len(one.Source.ConnectionIDs) != 2 {
		t.Errorf("one attached source: %+v", one)
	}
	c.ok("GET", "/api/attach", "", &listed)
	if len(listed.Attached) != 1 || listed.Attached[0].Owner.Worktree != "fix-orders" {
		t.Errorf("attached: %+v", listed.Attached)
	}
	// It is a source at once, before it has sent anything, and says whose.
	var src integrations.Sources
	c.ok("GET", "/api/sources", "", &src)
	if len(src.Services) != 1 || src.Services[0].Name != "acme/fix-orders" || src.Services[0].Owner == nil || src.Services[0].Owner.Worktree != "fix-orders" ||
		len(src.Attached) != 1 || len(src.Connections) != 3 {
		t.Errorf("sources: services %+v, attached %+v, %d connections", src.Services, src.Attached, len(src.Connections))
	}
	c.ok("GET", "/api/connections", "", nil)
	c.ok("GET", "/api/proxies", "", nil)

	c.fails("POST", "/api/attach", `{"service": ""}`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/attach", `{"service": "acme/fix:orders"}`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/attach", `not json`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/detach", `{}`, http.StatusBadRequest, "bad_request")

	var det integrations.DetachResult
	c.ok("POST", "/api/detach", `{"service": "acme/fix-orders"}`, &det)
	if !det.Found || det.Connections != 2 || det.Proxies != 1 || det.KeptData {
		t.Errorf("detach: %+v", det)
	}
	c.ok("POST", "/api/detach", `{"service": "acme/fix-orders"}`, &det)
	if det.Found || det.Connections != 0 {
		t.Errorf("detach again: %+v", det)
	}
	c.ok("POST", "/api/detach", `{"service": "acme/never-seen", "keep_data": true}`, &det)
	if det.Found || !det.KeptData {
		t.Errorf("detach of a worktree never seen: %+v", det)
	}
	// The person's own connection is the one thing left.
	c.ok("GET", "/api/sources", "", &src)
	if len(src.Connections) != 1 || src.Connections[0].ID != mine.ID || len(src.Attached) != 0 || len(src.Services) != 0 {
		t.Errorf("after detach: %d connections, attached %+v, services %+v", len(src.Connections), src.Attached, src.Services)
	}

	for _, body := range c.bodies {
		for _, secret := range []string{pg.Password, rd.Password} {
			if strings.Contains(body, secret) {
				t.Errorf("a response holds a password: %s", body)
			}
		}
	}
	if len(c.bodies) < 18 {
		t.Errorf("only %d responses were checked", len(c.bodies))
	}
}

// Attach and detach are for this machine alone, like every other route.
func TestAttachIsBehindTheGuard(t *testing.T) {
	c := newIntegrationsClient(t)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/attach", `{"service": "acme/fix-orders"}`},
		{"POST", "/api/detach", `{"service": "acme/fix-orders"}`},
		{"GET", "/api/attach", ""},
	} {
		req, err := http.NewRequest(tc.method, c.base+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "leadline.acme.example"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s from another host: %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		req, _ = http.NewRequest(tc.method, c.base+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Origin", "https://acme.example")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s from a website: %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
	var listed struct {
		Attached []integrations.AttachedView `json:"attached"`
	}
	c.ok("GET", "/api/attach", "", &listed)
	if len(listed.Attached) != 0 {
		t.Errorf("a refused request attached something: %+v", listed.Attached)
	}
}
