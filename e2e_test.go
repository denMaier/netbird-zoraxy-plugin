package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestRealNetBird runs the sync against a real server. `make e2e` starts one in
// Docker and sets these variables; the server is thrown away afterwards.
func TestRealNetBird(t *testing.T) {
	url, token := os.Getenv("NETBIRD_E2E_URL"), os.Getenv("NETBIRD_E2E_TOKEN")
	if url == "" || token == "" {
		t.Skip("set NETBIRD_E2E_URL and NETBIRD_E2E_TOKEN, or run make e2e")
	}
	ctx := context.Background()
	c := defaultConfig()
	c.NetBirdURL, c.Token, c.Mode, c.TokenStorage = url, token, modeContinuous, storageFile
	n := newNetBird(c)
	create := func(path string, body any) string {
		t.Helper()
		var out struct {
			ID string `json:"id"`
		}
		if err := n.call(ctx, "POST", path, body, &out); err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	groupA := create("/groups", map[string]any{"name": "e2e-a"})
	groupB := create("/groups", map[string]any{"name": "e2e-b"})
	c.NetworkID = create("/networks", map[string]any{"name": "e2e"})
	c.GroupIDs = []string{groupA}
	c.DNSTarget = "192.168.1.10"
	c.DNSZoneID = create("/dns/zones", map[string]any{"name": "e2e", "domain": "e2e.example", "enable_search_domain": false, "distribution_groups": []string{groupA}})
	c.normalize()

	choices, err := n.catalog(ctx)
	if err != nil || choices.DNSNote != "" || len(choices.Networks) != 1 || len(choices.Zones) != 1 {
		t.Fatalf("catalog: %+v %v", choices, err)
	}
	// The first preview meets an empty Network, which the API reports as null.
	first := testApp(t, c, &routeFixture{routes: []proxyRoute{{Hostname: "app.e2e.example", Tags: []string{"netbird"}}}})
	if result, err := first.synchronize(ctx, false); err != nil || len(result.Changes) != 2 {
		t.Fatalf("empty Network: %+v %v", result, err)
	}

	// Entries made by a person must survive every step below.
	manualResource := create("/networks/"+c.NetworkID+"/resources", resourceRequest{Name: "manual", Address: "manual.e2e.example", Enabled: true, Groups: []string{groupA}})
	manualRecord := create("/dns/zones/"+c.DNSZoneID+"/records", dnsRecord{Name: "manual.e2e.example", Type: "A", Content: "192.168.1.50", TTL: 60})

	source := &routeFixture{routes: []proxyRoute{{Hostname: "App.E2E.example.", Aliases: []string{"alias.e2e.example"}, Tags: []string{"netbird"}}}}
	dir := t.TempDir()
	save := func(c config) {
		t.Helper()
		if err := writeJSON(dir+"/settings.json", c); err != nil {
			t.Fatal(err)
		}
	}
	start := func() *app {
		t.Helper()
		a, err := newApp(dir, "instance1", source)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	sync := func(a *app, wantChanges int) syncResult {
		t.Helper()
		result, err := a.synchronize(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Changes) != wantChanges {
			t.Fatalf("want %d changes, got %+v", wantChanges, result.Changes)
		}
		return result
	}
	counts := func(wantResources, wantRecords int) ([]resource, []dnsRecord) {
		t.Helper()
		resources, err := n.resources(ctx, c.NetworkID)
		if err != nil {
			t.Fatal(err)
		}
		var records []dnsRecord
		if err := n.call(ctx, "GET", "/dns/zones/"+c.DNSZoneID+"/records", nil, &records); err != nil {
			t.Fatal(err)
		}
		if len(resources) != wantResources || len(records) != wantRecords {
			t.Fatalf("want %d resources and %d records, got %+v %+v", wantResources, wantRecords, resources, records)
		}
		return resources, records
	}

	// Dry run plans everything and writes nothing.
	save(c)
	a := start()
	sync(a, 4)
	counts(1, 1)

	// Publish. The real API must accept the requests and echo them unchanged.
	c.DryRun = false
	save(c)
	a = start()
	if result := sync(a, 4); result.Applied != 4 {
		t.Fatalf("applied %d", result.Applied)
	}
	resources, _ := counts(3, 3)
	for _, r := range resources {
		if r.Address == "app.e2e.example" && (r.Description != ownerMarker("instance1") || !r.Enabled || !slices.Equal(groupIDs(r.Groups), c.GroupIDs)) {
			t.Fatalf("published resource: %+v", r)
		}
	}

	// A restart from disk finds nothing to do.
	a = start()
	sync(a, 0)

	// Changed settings update in place.
	c.DNSTarget = "zoraxy.peers.example"
	c.GroupIDs = []string{groupB}
	c.normalize()
	save(c)
	a = start()
	sync(a, 4)
	counts(3, 3)
	sync(a, 0)

	// Records edited by someone else stop the whole run before any write.
	var edited dnsRecord
	for _, record := range a.state.DNS {
		edited = record
		break
	}
	id := edited.ID
	edited.ID, edited.TTL = "", 60
	path := "/dns/zones/" + c.DNSZoneID + "/records/" + id
	if err := n.call(ctx, "PUT", path, edited, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.synchronize(ctx, false); err == nil || !strings.Contains(err.Error(), "edited outside") {
		t.Fatalf("edited record: %v", err)
	}
	edited.TTL = 300
	if err := n.call(ctx, "PUT", path, edited, nil); err != nil {
		t.Fatal(err)
	}

	// A hostname someone else already publishes is not adopted.
	source.set([]proxyRoute{{Hostname: "manual.e2e.example", Tags: []string{"netbird"}}})
	if _, err := a.synchronize(ctx, false); err == nil || !strings.Contains(err.Error(), "will not be adopted") {
		t.Fatalf("unowned resource: %v", err)
	}

	// NetBird's own reason reaches the operator.
	err = n.call(ctx, "POST", "/dns/zones/"+c.DNSZoneID+"/records", dnsRecord{Name: "app.outside.example", Type: "A", Content: "192.168.1.10", TTL: 60}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not belong to zone") {
		t.Fatalf("API reason: %v", err)
	}

	// Removing the tag withdraws only what the plugin owns.
	source.set([]proxyRoute{{Hostname: "app.e2e.example", Tags: []string{"other"}}})
	sync(a, 4)
	resources, records := counts(1, 1)
	if resources[0].ID != manualResource || records[0].ID != manualRecord {
		t.Fatalf("deleted someone else's entry: %+v %+v", resources, records)
	}

	// Wildcards work as resources when DNS sync is off.
	c.DNSZoneID, c.DNSTarget = "", ""
	save(c)
	a = start()
	source.set([]proxyRoute{{Hostname: "*.wild.example", Tags: []string{"netbird"}}})
	sync(a, 1)
	sync(a, 0)
	source.set([]proxyRoute{})
	sync(a, 1)

	// A one-time sync publishes, drops the token, and leaves the entry. The token
	// comes from the settings page and only ever lives in memory.
	c.Mode = modeOnce
	save(c)
	a = start()
	a.config.Token = token
	source.set([]proxyRoute{{Hostname: "once.e2e.example", Tags: []string{"netbird"}}})
	if result := sync(a, 1); !result.TokenDeleted || a.snapshot().Token != "" {
		t.Fatalf("one-time sync: %+v", result)
	}
	resources, err = n.resources(ctx, c.NetworkID)
	if err != nil || !slices.ContainsFunc(resources, func(r resource) bool { return r.Address == "once.e2e.example" }) {
		t.Fatalf("one-time entry missing: %+v %v", resources, err)
	}
}
