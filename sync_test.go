package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type routeFixture struct {
	mu     sync.Mutex
	routes []proxyRoute
	err    error
}

func (f *routeFixture) Routes(context.Context) ([]proxyRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routes), f.err
}

func (f *routeFixture) set(routes []proxyRoute) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = routes
}

type netbirdFixture struct {
	mu        sync.Mutex
	resources map[string]resource
	records   map[string]dnsRecord
	writes    []string
	next      int
	server    *httptest.Server
}

func newNetbirdFixture(t *testing.T) *netbirdFixture {
	t.Helper()
	f := &netbirdFixture{resources: map[string]resource{}, records: map[string]dnsRecord{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *netbirdFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Token test-secret" {
		http.Error(w, "bad authentication", 401)
		return
	}
	if r.Method != "GET" {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		if r.Method != "DELETE" && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "expected JSON", 415)
			return
		}
	}
	resourcePath := "/api/networks/net1/resources"
	recordPath := "/api/dns/zones/zone1/records"
	switch {
	case r.URL.Path == "/api/networks" && r.Method == "GET":
		sendJSON(w, 200, []group{{ID: "net1", Name: "Home"}})
	case r.URL.Path == "/api/groups" && r.Method == "GET":
		sendJSON(w, 200, []group{{ID: "group1", Name: "Apps"}})
	case r.URL.Path == "/api/dns/zones" && r.Method == "GET":
		sendJSON(w, 200, []zone{testZone()})
	case r.URL.Path == "/api/dns/zones/zone1" && r.Method == "GET":
		sendJSON(w, 200, testZone())
	case r.URL.Path == resourcePath && r.Method == "GET":
		var values []resource // Encoded as null when empty, like the real API.
		for _, res := range f.resources {
			values = append(values, res)
		}
		sendJSON(w, 200, values)
	case r.URL.Path == recordPath && r.Method == "GET":
		values := []dnsRecord{}
		for _, record := range f.records {
			values = append(values, record)
		}
		sendJSON(w, 200, values)
	case r.URL.Path == resourcePath && r.Method == "POST" || strings.HasPrefix(r.URL.Path, resourcePath+"/") && r.Method == "PUT":
		var body resourceRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, resourcePath+"/")
		if r.Method == "POST" {
			f.next++
			id = fmt.Sprintf("res%d", f.next)
		}
		res := resource{ID: id, Name: body.Name, Description: body.Description, Address: body.Address, Enabled: body.Enabled, Groups: []group{}}
		for _, g := range body.Groups {
			res.Groups = append(res.Groups, group{ID: g})
		}
		f.resources[id] = res
		sendJSON(w, 200, res)
	case strings.HasPrefix(r.URL.Path, resourcePath+"/") && r.Method == "DELETE":
		delete(f.resources, strings.TrimPrefix(r.URL.Path, resourcePath+"/"))
		sendJSON(w, 200, struct{}{})
	case r.URL.Path == recordPath && r.Method == "POST" || strings.HasPrefix(r.URL.Path, recordPath+"/") && r.Method == "PUT":
		var body dnsRecord
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		if body.ID != "" {
			http.Error(w, "ID is response-only", 400)
			return
		}
		if r.Method == "POST" {
			f.next++
			body.ID = fmt.Sprintf("dns%d", f.next)
		} else {
			body.ID = strings.TrimPrefix(r.URL.Path, recordPath+"/")
		}
		f.records[body.ID] = body
		sendJSON(w, 200, body)
	case strings.HasPrefix(r.URL.Path, recordPath+"/") && r.Method == "DELETE":
		delete(f.records, strings.TrimPrefix(r.URL.Path, recordPath+"/"))
		sendJSON(w, 200, struct{}{})
	default:
		http.Error(w, "unexpected endpoint", 404)
	}
}

func testZone() zone {
	return zone{ID: "zone1", Domain: "example.com", Enabled: true, DistributionGroups: []string{"clients", "routers"}}
}

func testConfig(f *netbirdFixture) config {
	c := defaultConfig()
	c.NetBirdURL, c.Token, c.NetworkID, c.Mode, c.TokenStorage = f.server.URL, "test-secret", "net1", modeContinuous, storageFile
	c.GroupIDs = []string{"group1"}
	c.DNSZoneID, c.DNSTarget = "zone1", "192.168.1.10"
	return c
}

func testApp(t *testing.T, c config, source routeSource) *app {
	t.Helper()
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "settings.json"), c); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(dir, "instance1", source)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSyncLifecycle(t *testing.T) {
	f := newNetbirdFixture(t)
	source := &routeFixture{routes: []proxyRoute{
		{Hostname: "Grafana.Example.com.", Aliases: []string{"metrics.example.com", "grafana.example.com"}, Tags: []string{"netbird", "monitoring"}},
		{Hostname: "disabled.example.com", Disabled: true, Tags: []string{"netbird"}},
		{Hostname: "private.example.com", Tags: []string{"other"}},
	}}
	f.resources["manual"] = resource{ID: "manual", Name: "Manual resource", Address: "manual.example.com", Enabled: true}
	f.records["manual"] = dnsRecord{ID: "manual", Name: "manual.example.com", Type: "A", Content: "192.168.1.50", TTL: 60}
	a := testApp(t, testConfig(f), source)
	ctx := context.Background()
	result, err := a.synchronize(ctx, false)
	if err != nil || len(result.Domains) != 2 || len(result.Changes) != 4 || result.Applied != 0 || len(f.writes) != 0 {
		t.Fatalf("dry run: result=%+v err=%v writes=%v", result, err, f.writes)
	}
	if len(a.state.DNS) != 0 {
		t.Fatal("dry run changed ownership state")
	}
	a.config.DryRun = false
	if err := writeJSON(filepath.Join(a.dir, "settings.json"), a.config); err != nil {
		t.Fatal(err)
	}
	result, err = a.synchronize(ctx, false)
	if err != nil || result.Applied != 4 || len(f.resources) != 3 || len(f.records) != 3 || len(a.state.DNS) != 2 {
		t.Fatalf("publish: result=%+v err=%v", result, err)
	}
	// Restart from the on-disk ledger. A second live sync must be idempotent.
	a, err = newApp(a.dir, "instance1", source)
	if err != nil {
		t.Fatal(err)
	}
	result, err = a.synchronize(ctx, false)
	if err != nil || len(result.Changes) != 0 {
		t.Fatalf("repeat: result=%+v err=%v", result, err)
	}
	// Settings changes update existing IDs rather than creating duplicate objects.
	a.config.DNSTarget = "zoraxy.netbird.cloud"
	a.config.GroupIDs = []string{"group2"}
	result, err = a.synchronize(ctx, false)
	if err != nil || result.Applied != 4 || len(f.resources) != 3 || len(f.records) != 3 {
		t.Fatalf("update: result=%+v err=%v", result, err)
	}
	// Withdrawing all tags removes only owned resources and records.
	source.set([]proxyRoute{{Hostname: "grafana.example.com", Tags: []string{"other"}}})
	result, err = a.synchronize(ctx, false)
	if err != nil || result.Applied != 4 || len(f.resources) != 1 || len(f.records) != 1 || len(a.state.DNS) != 0 {
		t.Fatalf("withdraw: result=%+v err=%v resources=%v records=%v", result, err, f.resources, f.records)
	}
	if _, ok := f.resources["manual"]; !ok {
		t.Fatal("deleted an unowned resource")
	}
	if _, ok := f.records["manual"]; !ok {
		t.Fatal("deleted an unowned DNS record")
	}
	if result.Changes[0].Kind != "resource" || result.Changes[0].Verb != "delete" {
		t.Fatal("withdrawal did not revoke resources first")
	}
}

func TestDNSConflictStopsAllWrites(t *testing.T) {
	for _, scenario := range []string{"unowned", "outside-zone", "wildcard", "edited-owned", "edited-stale", "missing-zone"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNetbirdFixture(t)
			c := testConfig(f)
			c.DryRun = false
			source := &routeFixture{routes: []proxyRoute{{Hostname: "app.example.com", Tags: []string{"netbird"}}}}
			a := testApp(t, c, source)
			switch scenario {
			case "unowned":
				f.records["manual"] = dnsRecord{ID: "manual", Name: "app.example.com", Type: "A", Content: "192.168.1.10", TTL: 300}
			case "outside-zone":
				source.routes[0].Aliases = []string{"app.other.com"}
			case "wildcard":
				source.routes[0].Hostname = "*.example.com"
			case "missing-zone":
				a.config.DNSZoneID = "missing"
			case "edited-owned", "edited-stale":
				if _, err := a.synchronize(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				for id, record := range f.records {
					record.Content = "192.168.1.99"
					f.records[id] = record
				}
				if scenario == "edited-stale" {
					source.set([]proxyRoute{})
				}
			}
			before := len(f.writes)
			if _, err := a.synchronize(context.Background(), false); err == nil {
				t.Fatal("expected preflight conflict")
			}
			if len(f.writes) != before {
				t.Fatalf("wrote despite a preflight error: %v", f.writes[before:])
			}
		})
	}
}

func TestDiscoveryFailureNeverDeletes(t *testing.T) {
	for _, response := range []string{"null", `{"error":"cannot list routes"}`, `{`, `[{"RootOrMatchingDomain":"/","Tags":["netbird"]}]`} {
		t.Run(response, func(t *testing.T) {
			f := newNetbirdFixture(t)
			c := testConfig(f)
			c.DryRun = false
			c.DNSZoneID = ""
			f.resources["owned"] = resource{ID: "owned", Name: "Owned", Address: "app.example.com", Description: ownerMarker("instance1"), Enabled: true}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/plugin/api/proxy/list" || r.URL.Query().Get("type") != "host" || r.Header.Get("Authorization") != "Bearer zoraxy-secret" {
					t.Error("incorrect Zoraxy route API request")
				}
				fmt.Fprint(w, response)
			}))
			defer server.Close()
			a := testApp(t, c, &zoraxyClient{baseURL: server.URL, token: "zoraxy-secret", http: newHTTPClient()})
			if _, err := a.synchronize(context.Background(), false); err == nil {
				t.Fatal("expected discovery error")
			}
			if len(f.writes) != 0 || len(f.resources) != 1 {
				t.Fatal("discovery failure deleted resources")
			}
		})
	}
}

func TestOwnershipBoundariesAndResourceUpdates(t *testing.T) {
	c := defaultConfig()
	c.NetworkID = "net1"
	c.GroupIDs = []string{"a", "b"}
	current := resource{ID: "owned", Name: resourceName("instance1", "net1", "app.example.com"), Description: ownerMarker("instance1"), Address: "app.example.com", Enabled: true, Groups: []group{{ID: "b"}, {ID: "a"}}}
	changes, err := resourceChanges(c, "instance1", []string{"app.example.com"}, []resource{current})
	if err != nil || len(changes) != 0 {
		t.Fatalf("group order should not matter: %v %v", changes, err)
	}
	current.Enabled = false
	changes, err = resourceChanges(c, "instance1", []string{"app.example.com"}, []resource{current})
	if err != nil || len(changes) != 1 || changes[0].Verb != "update" || !changes[0].Resource.Enabled {
		t.Fatal("failed to re-enable desired resource")
	}
	current.Description = ownerMarker("another-instance")
	if _, err := resourceChanges(c, "instance1", []string{"app.example.com"}, []resource{current}); err == nil {
		t.Fatal("adopted another instance's resource")
	}
	changes, err = resourceChanges(c, "instance1", nil, []resource{current})
	if err != nil || len(changes) != 0 {
		t.Fatal("deleted another instance's resource")
	}
}

func TestUnrelatedDuplicateResourceNamesDoNotBlockSync(t *testing.T) {
	c := defaultConfig()
	c.NetworkID = "net1"
	c.GroupIDs = []string{"a"}
	manual := []resource{{ID: "m1", Name: "Same", Address: "m1.example.com"}, {ID: "m2", Name: "Same", Address: "m2.example.com"}}
	changes, err := resourceChanges(c, "instance1", []string{"app.example.com"}, manual)
	if err != nil || len(changes) != 1 {
		t.Fatalf("%v %v", changes, err)
	}
}

func TestStoppingSyncFinishesCurrentWrite(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.DryRun = false
	routes := []proxyRoute{{Hostname: "a.example.com", Tags: []string{"netbird"}}, {Hostname: "b.example.com", Tags: []string{"netbird"}}}
	state := newState()
	result, err := buildPlan(context.Background(), &routeFixture{routes: routes}, newNetBird(c), c, "instance1", state)
	if err != nil {
		t.Fatal(err)
	}
	// Shutdown arrives during the first write; its record must still reach the ledger.
	ctx, cancel := context.WithCancel(context.Background())
	save := func() error { cancel(); return nil }
	err = applyPlan(ctx, newNetBird(c), c, &result, &state, save)
	if !errors.Is(err, context.Canceled) || len(f.writes) != 1 || len(state.DNS) != 1 || result.Applied != 1 {
		t.Fatalf("err=%v writes=%v ledger=%v applied=%d", err, f.writes, state.DNS, result.Applied)
	}
}

func storedSettings(t *testing.T, a *app) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func saveThroughUI(t *testing.T, a *app, c config) {
	t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/ui/api/config", bytes.NewReader(body))
	r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
}

func TestOneTimeTokenNeverReachesDiskAndEndsAfterLiveSuccess(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.Mode = modeOnce
	source := &routeFixture{routes: []proxyRoute{{Hostname: "app.example.com", Tags: []string{"netbird"}}}}
	a := testApp(t, c, source)
	ctx := context.Background()
	saveThroughUI(t, a, c)
	keeps := func(step string) {
		t.Helper()
		if a.snapshot().Token != "test-secret" {
			t.Fatalf("%s removed the token", step)
		}
		if strings.Contains(storedSettings(t, a), "test-secret") {
			t.Fatalf("token on disk after %s", step)
		}
	}
	keeps("save")
	// A dry run writes nothing, so the one-time sync has not happened yet.
	if result, err := a.synchronize(ctx, false); err != nil || result.TokenDeleted {
		t.Fatalf("dry run: %+v %v", result, err)
	}
	keeps("dry run")
	c.DryRun = false
	saveThroughUI(t, a, c)
	keeps("second save")
	if result, err := a.synchronize(ctx, true); err != nil || result.TokenDeleted {
		t.Fatalf("preview: %+v %v", result, err)
	}
	keeps("preview")
	source.err = errors.New("Zoraxy is down")
	if result, err := a.synchronize(ctx, false); err == nil || result.TokenDeleted {
		t.Fatalf("failed sync: %+v %v", result, err)
	}
	keeps("failed sync")
	source.err = nil

	result, err := a.synchronize(ctx, false)
	if err != nil || !result.TokenDeleted || result.Applied != 2 || len(f.resources) != 1 {
		t.Fatalf("one-time sync: %+v %v", result, err)
	}
	if a.snapshot().Token != "" {
		t.Fatal("token survived a one-time sync")
	}
	// Without a token nothing more can run, and published entries stay.
	if _, err := a.synchronize(ctx, false); err == nil {
		t.Fatal("synced without a token")
	}
	if len(f.resources) != 1 || len(f.records) != 1 {
		t.Fatal("published entries were removed")
	}
	// A new token resumes from the same ledger.
	c.Token = "test-secret"
	saveThroughUI(t, a, c)
	if result, err := a.synchronize(ctx, false); err != nil || len(result.Changes) != 0 || !result.TokenDeleted {
		t.Fatalf("second one-time sync: %+v %v", result, err)
	}
}

func TestContinuousMemoryTokenPausesAfterRestart(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.TokenStorage, c.DryRun = storageMemory, false
	source := signalingSource{called: make(chan struct{}, 1)}
	a := testApp(t, c, source)
	saveThroughUI(t, a, c)
	if strings.Contains(storedSettings(t, a), "test-secret") {
		t.Fatal("memory mode wrote the token to disk")
	}
	if result, err := a.synchronize(context.Background(), false); err != nil || result.TokenDeleted || a.snapshot().Token == "" {
		t.Fatalf("continuous sync: %+v %v", result, err)
	}
	<-source.called
	// The restart must not fail, or Zoraxy would disable the plugin.
	a, err := newApp(a.dir, "instance1", source)
	if err != nil {
		t.Fatal(err)
	}
	if !a.snapshot().paused() {
		t.Fatal("not paused without a token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.run(ctx) }()
	defer func() { cancel(); <-done }()
	a.wake <- struct{}{}
	select {
	case <-source.called:
		t.Fatal("scheduler synced while paused")
	case <-time.After(100 * time.Millisecond):
	}
	r := httptest.NewRequest("GET", "/ui/api/status", nil)
	r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"paused":true`) {
		t.Fatalf("status: %s", w.Body.String())
	}
	// Entering the token resumes the schedule.
	saveThroughUI(t, a, c)
	select {
	case <-source.called:
	case <-time.After(2 * time.Second):
		t.Fatal("did not resume after the token was entered")
	}
}

func TestContinuousFileTokenSurvivesRestart(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.DryRun = false
	a := testApp(t, c, &routeFixture{routes: []proxyRoute{}})
	saveThroughUI(t, a, c)
	if !strings.Contains(storedSettings(t, a), "test-secret") {
		t.Fatal("file mode did not store the token")
	}
	if result, err := a.synchronize(context.Background(), false); err != nil || result.TokenDeleted {
		t.Fatalf("%+v %v", result, err)
	}
	a, err := newApp(a.dir, "instance1", &routeFixture{routes: []proxyRoute{}})
	if err != nil || a.snapshot().Token != "test-secret" || a.snapshot().paused() {
		t.Fatalf("restart: %+v %v", a, err)
	}
}

func TestLeavingFileStorageRemovesToken(t *testing.T) {
	f := newNetbirdFixture(t)
	for name, change := range map[string]func(*config){
		"to memory":   func(c *config) { c.TokenStorage = storageMemory },
		"to one time": func(c *config) { c.Mode = modeOnce },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig(f)
			a := testApp(t, c, &routeFixture{routes: []proxyRoute{}})
			c.Token = ""
			change(&c)
			saveThroughUI(t, a, c)
			if strings.Contains(storedSettings(t, a), "test-secret") {
				t.Fatal("token left on disk")
			}
			if a.snapshot().Token != "test-secret" {
				t.Fatal("token lost from memory")
			}
		})
	}
}

func TestStartupRemovesTokenThisModeMustNotKeep(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.Mode = modeOnce // As an older version would have left it on disk.
	a := testApp(t, c, &routeFixture{routes: []proxyRoute{}})
	if a.snapshot().Token != "" || strings.Contains(storedSettings(t, a), "test-secret") {
		t.Fatal("startup kept a token on disk")
	}
	if strings.Contains(storedSettings(t, a), `"token"`) {
		t.Fatal("empty token field left behind")
	}
}

func TestMissingOwnedDNSCanBeRecreated(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.DryRun = false
	a := testApp(t, c, &routeFixture{routes: []proxyRoute{{Hostname: "app.example.com", Tags: []string{"netbird"}}}})
	if _, err := a.synchronize(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for id := range f.records {
		delete(f.records, id)
	}
	result, err := a.synchronize(context.Background(), false)
	if err != nil || result.Applied != 1 || len(f.records) != 1 {
		t.Fatalf("recreate: %v %v", result, err)
	}
	// A different zone or API origin never inherits this ledger's ownership.
	other := c
	other.DNSZoneID = "other"
	if dnsScope(c, "instance1") == dnsScope(other, "instance1") || dnsScope(c, "instance1") == dnsScope(c, "instance2") {
		t.Fatal("ownership scopes collided")
	}
}

func TestDNSOwnershipSaveFailureStopsPublication(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.DryRun = false
	state := newState()
	result, err := buildPlan(context.Background(), &routeFixture{routes: []proxyRoute{{Hostname: "app.example.com", Tags: []string{"netbird"}}}}, newNetBird(c), c, "instance1", state)
	if err != nil {
		t.Fatal(err)
	}
	err = applyPlan(context.Background(), newNetBird(c), c, &result, &state, func() error { return errors.New("disk full") })
	if err == nil || len(f.resources) != 0 || len(f.writes) != 1 {
		t.Fatalf("continued after ledger failure: %v %v", err, f.writes)
	}
}

func TestDomainsAndDNSTargets(t *testing.T) {
	domains, err := desiredDomains([]proxyRoute{
		{Hostname: "APP.EXAMPLE.COM.", Aliases: []string{"alias.example.com", "app.example.com"}, Tags: []string{"netbird"}},
		{Hostname: "*.example.com", Tags: []string{"netbird"}},
		{Hostname: "ignored.example.com", Tags: []string{"NetBird"}},
	}, "netbird")
	if err != nil || !slices.Equal(domains, []string{"*.example.com", "alias.example.com", "app.example.com"}) {
		t.Fatalf("%v %v", domains, err)
	}
	for _, invalid := range []string{"/", "*", "127.0.0.1", "example.com:443", "https://example.com", "-app.example.com", "app..example.com", "app.example.com/path"} {
		if _, err := domainName(invalid, true); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	for raw, expected := range map[string]string{"192.168.1.10": "A", "fd00::10": "AAAA", "zoraxy.netbird.cloud": "CNAME"} {
		typeName, _, err := dnsTarget(raw)
		if err != nil || typeName != expected {
			t.Errorf("%s: %s %v", raw, typeName, err)
		}
	}
	for _, raw := range []string{"127.0.0.1", "::1", "0.0.0.0", "224.0.0.1", "fe80::1%en0", "app.example.com:443"} {
		if _, _, err := dnsTarget(raw); err == nil {
			t.Errorf("accepted unreachable DNS target %q", raw)
		}
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, data := range []string{"{", "null", `{}`, `{"version":99,"dns":{}}`, `{"version":1,"dns":null}`, `{"version":1,"dns":{"entry":{"id":"../../unsafe"}}}`} {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newApp(dir, "instance1", &routeFixture{}); err == nil {
			t.Fatalf("accepted corrupt ledger: %s", data)
		}
	}
}
