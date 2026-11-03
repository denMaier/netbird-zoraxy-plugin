package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"
)

func domainName(raw string, wildcard bool) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	check := name
	if wildcard {
		check = strings.TrimPrefix(check, "*.")
	}
	if len(name) > 253 || !strings.Contains(check, ".") {
		return "", fmt.Errorf("%q must be a fully qualified domain name", raw)
	}
	if _, err := netip.ParseAddr(check); err == nil {
		return "", fmt.Errorf("%q is an IP address; tag hostname routes instead", raw)
	}
	for _, label := range strings.Split(check, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid hostname %q", raw)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid hostname %q; use ASCII / punycode hostnames", raw)
			}
		}
	}
	return name, nil
}

func dnsTarget(raw string) (string, string, error) {
	if addr, err := netip.ParseAddr(raw); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
			return "", "", errors.New("DNS target must be an address reachable by NetBird clients or routing peers")
		}
		if addr.Is4() {
			return "A", addr.String(), nil
		}
		return "AAAA", addr.String(), nil
	}
	name, err := domainName(raw, false)
	if err != nil {
		return "", "", errors.New("DNS target must be Zoraxy's reachable IP or fully qualified peer hostname, without a port")
	}
	return "CNAME", name, nil
}

func desiredDomains(routes []proxyRoute, tag string) ([]string, error) {
	domains := map[string]bool{}
	for _, route := range routes {
		if route.Disabled || !slices.Contains(route.Tags, tag) {
			continue
		}
		for _, raw := range append([]string{route.Hostname}, route.Aliases...) {
			name, err := domainName(raw, true)
			if err != nil {
				return nil, fmt.Errorf("tagged Zoraxy route: %w", err)
			}
			domains[name] = true
		}
	}
	result := make([]string, 0, len(domains))
	for name := range domains {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

// DNS records lack a description/ownership field. Only IDs saved here can ever
// be changed or removed. Preserve this file when updating/moving the plugin.
type ownershipState struct {
	Version int                  `json:"version"`
	DNS     map[string]dnsRecord `json:"dns"`
}

func newState() ownershipState {
	return ownershipState{Version: 1, DNS: map[string]dnsRecord{}}
}

type change struct {
	Kind     string           `json:"kind"`
	Verb     string           `json:"verb"`
	Domain   string           `json:"domain"`
	ID       string           `json:"id,omitempty"`
	Resource *resourceRequest `json:"resource,omitempty"`
	Record   *dnsRecord       `json:"record,omitempty"`
	stateKey string
}

type syncResult struct {
	Time    time.Time `json:"time"`
	DryRun  bool      `json:"dry_run"`
	Domains []string  `json:"domains"`
	Changes []change  `json:"changes"`
	Applied int       `json:"applied"`
	// TokenDeleted reports that a one-time sync removed the token from memory.
	TokenDeleted bool   `json:"token_deleted,omitempty"`
	Error        string `json:"error,omitempty"`
}

func ownerMarker(instance string) string {
	return "Managed by " + pluginID + "; Zoraxy instance " + instance + "."
}

func resourceName(instance, network, domain string) string {
	h := sha256.Sum256([]byte(instance + "\x00" + network + "\x00" + domain))
	label := strings.ReplaceAll(domain, "*", "wildcard")
	if len(label) > 70 {
		label = label[:70]
	}
	return "zoraxy-" + label + "-" + hex.EncodeToString(h[:6])
}

func groupIDs(groups []group) []string {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func resourceChanges(c config, instance string, domains []string, existing []resource) ([]change, error) {
	wanted := map[string]bool{}
	for _, d := range domains {
		wanted[d] = true
	}
	owned := map[string]resource{}
	names := map[string]resource{}
	changes := []change{}
	for _, r := range existing {
		names[r.Name] = r
		if r.Description == ownerMarker(instance) {
			if _, ok := owned[r.Address]; ok {
				return nil, fmt.Errorf("multiple plugin-owned resources have address %s; resolve the duplicate before syncing", r.Address)
			}
			owned[r.Address] = r
		} else if wanted[strings.TrimSuffix(strings.ToLower(r.Address), ".")] {
			return nil, fmt.Errorf("resource %s exists outside this plugin and will not be adopted; delete it in NetBird to publish this hostname", r.Address)
		}
	}
	for _, domain := range domains {
		request := resourceRequest{
			Name: resourceName(instance, c.NetworkID, domain), Description: ownerMarker(instance),
			Address: domain, Enabled: true, Groups: slices.Clone(c.GroupIDs),
		}
		current, found := owned[domain]
		if r, ok := names[request.Name]; ok && (!found || r.ID != current.ID) {
			return nil, fmt.Errorf("resource name collision for %s; sync stopped", domain)
		}
		verb := "create"
		if found {
			if current.Name == request.Name && current.Enabled && slices.Equal(groupIDs(current.Groups), request.Groups) {
				continue
			}
			verb = "update"
		}
		changes = append(changes, change{Kind: "resource", Verb: verb, Domain: domain, ID: current.ID, Resource: &request})
	}
	for domain, r := range owned {
		if !wanted[domain] {
			changes = append(changes, change{Kind: "resource", Verb: "delete", Domain: domain, ID: r.ID})
		}
	}
	return changes, nil
}

func dnsScope(c config, instance string) string {
	return strings.TrimSuffix(c.NetBirdURL, "/api") + "\x00" + c.DNSZoneID + "\x00" + instance + "\x00"
}

func sameRecord(a, b dnsRecord) bool {
	return a.Name == b.Name && a.Type == b.Type && a.Content == b.Content && a.TTL == b.TTL
}

func dnsChanges(c config, instance string, domains []string, z zone, records []dnsRecord, state ownershipState) ([]change, error) {
	changes := []change{}
	zoneDomain, err := domainName(z.Domain, false)
	if err != nil || z.ID != c.DNSZoneID || !z.Enabled || len(z.DistributionGroups) == 0 {
		return nil, errors.New("select an enabled DNS zone with distribution groups and a valid domain")
	}
	typeName, target, err := dnsTarget(c.DNSTarget)
	if err != nil {
		return nil, err
	}
	if typeName == "CNAME" && (target == zoneDomain || strings.HasSuffix(target, "."+zoneDomain)) {
		return nil, errors.New("CNAME target must be outside the managed DNS zone to avoid loops or shadowed peer names")
	}
	byID := map[string]dnsRecord{}
	byName := map[string][]dnsRecord{}
	for _, record := range records {
		if !validID(record.ID) || record.Name == "" || record.Type == "" || record.Content == "" {
			return nil, errors.New("NetBird returned an incomplete DNS record")
		}
		if _, found := byID[record.ID]; found {
			return nil, errors.New("NetBird returned duplicate DNS record IDs")
		}
		byID[record.ID] = record
		name := strings.TrimSuffix(strings.ToLower(record.Name), ".")
		byName[name] = append(byName[name], record)
	}
	scope := dnsScope(c, instance)
	wanted := map[string]bool{}
	for _, domain := range domains {
		if strings.HasPrefix(domain, "*.") {
			return nil, errors.New("private DNS sync currently requires exact hostnames; wildcard resources work with DNS sync disabled")
		}
		if domain != zoneDomain && !strings.HasSuffix(domain, "."+zoneDomain) {
			return nil, fmt.Errorf("%s is outside DNS zone %s; choose a matching zone or disable DNS sync", domain, zoneDomain)
		}
		wanted[domain] = true
		key := scope + domain
		previous, known := state.DNS[key]
		current, exists := byID[previous.ID]
		if known && exists && !sameRecord(current, previous) {
			return nil, fmt.Errorf("DNS record %s was edited outside the plugin; restore it before syncing", domain)
		}
		for _, record := range byName[domain] {
			if !known || record.ID != previous.ID {
				return nil, fmt.Errorf("DNS record %s exists outside this plugin and will not be adopted; delete it in NetBird to publish this hostname", domain)
			}
		}
		request := dnsRecord{Name: domain, Type: typeName, Content: target, TTL: 300}
		if exists && sameRecord(current, request) {
			continue
		}
		verb := "create"
		if exists {
			verb = "update"
		}
		changes = append(changes, change{Kind: "dns", Verb: verb, Domain: domain, ID: current.ID, Record: &request, stateKey: key})
	}
	for key, previous := range state.DNS {
		if !strings.HasPrefix(key, scope) || wanted[strings.TrimPrefix(key, scope)] {
			continue
		}
		current, exists := byID[previous.ID]
		if exists && !sameRecord(current, previous) {
			return nil, fmt.Errorf("stale DNS record %s was edited outside the plugin; it will not be removed", previous.Name)
		}
		id := ""
		if exists {
			id = current.ID
		}
		// An already removed record still needs to be forgotten in the ledger.
		changes = append(changes, change{Kind: "dns", Verb: "delete", Domain: previous.Name, ID: id, stateKey: key})
	}
	return changes, nil
}

func buildPlan(ctx context.Context, source routeSource, n *netbirdClient, c config, instance string, state ownershipState) (syncResult, error) {
	result := syncResult{Time: time.Now().UTC(), DryRun: c.DryRun, Domains: []string{}, Changes: []change{}}
	routes, err := source.Routes(ctx)
	if err != nil {
		return result, err
	}
	result.Domains, err = desiredDomains(routes, c.Tag)
	if err != nil {
		return result, err
	}
	existing, err := n.resources(ctx, c.NetworkID)
	if err != nil {
		return result, fmt.Errorf("read NetBird resources: %w", err)
	}
	resources, err := resourceChanges(c, instance, result.Domains, existing)
	if err != nil {
		return result, err
	}
	result.Changes = append(result.Changes, resources...)
	if c.DNSZoneID != "" {
		var z zone
		path := "/dns/zones/" + c.DNSZoneID
		if err := n.call(ctx, "GET", path, nil, &z); err != nil {
			return result, fmt.Errorf("read DNS zone: %w", err)
		}
		var records []dnsRecord
		if err := n.call(ctx, "GET", path+"/records", nil, &records); err != nil {
			return result, fmt.Errorf("read DNS records: %w", err)
		}
		if records == nil {
			return result, errors.New("NetBird returned null instead of a DNS record list")
		}
		dns, err := dnsChanges(c, instance, result.Domains, z, records, state)
		if err != nil {
			return result, err
		}
		result.Changes = append(result.Changes, dns...)
	}
	// Revoke old resources first, then withdraw DNS, then publish DNS and resources.
	// Preflight above must finish entirely before any write is attempted.
	rank := func(ch change) int {
		if ch.Verb == "delete" {
			if ch.Kind == "resource" {
				return 0
			}
			return 1
		}
		if ch.Kind == "dns" {
			return 2
		}
		return 3
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		a, b := result.Changes[i], result.Changes[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		return a.Domain < b.Domain
	})
	return result, nil
}

func applyPlan(ctx context.Context, n *netbirdClient, c config, result *syncResult, state *ownershipState, save func() error) error {
	if result.DryRun {
		return nil
	}
	for _, ch := range result.Changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Stop between writes, never during one: a DNS create whose response is
		// lost leaves a record the ledger cannot claim. The HTTP client's timeout
		// still bounds the call.
		if err := applyChange(context.WithoutCancel(ctx), n, c, ch, state, save); err != nil {
			return fmt.Errorf("%s %s %s: %w", ch.Verb, ch.Kind, ch.Domain, err)
		}
		log.Printf("%sd %s %s", ch.Verb, ch.Kind, ch.Domain)
		result.Applied++
	}
	return nil
}

func applyChange(ctx context.Context, n *netbirdClient, c config, ch change, state *ownershipState, save func() error) error {
	method := map[string]string{"create": "POST", "update": "PUT", "delete": "DELETE"}[ch.Verb]
	path := "/networks/" + c.NetworkID + "/resources"
	if ch.Kind == "dns" {
		path = "/dns/zones/" + c.DNSZoneID + "/records"
	}
	if ch.ID != "" {
		path += "/" + ch.ID
	}
	switch {
	case ch.Kind == "resource" && ch.Verb == "delete":
		return n.call(ctx, method, path, nil, nil)
	case ch.Kind == "resource":
		return n.call(ctx, method, path, ch.Resource, nil)
	case ch.Verb == "delete":
		if ch.ID != "" {
			if err := n.call(ctx, method, path, nil, nil); err != nil {
				return err
			}
		}
		delete(state.DNS, ch.stateKey)
	default:
		var record dnsRecord
		if err := n.call(ctx, method, path, ch.Record, &record); err != nil {
			return err
		}
		if !validID(record.ID) || !sameRecord(record, *ch.Record) {
			return errors.New("NetBird returned an unexpected DNS record after a write; inspect it before retrying")
		}
		state.DNS[ch.stateKey] = record
	}
	if err := save(); err != nil {
		return fmt.Errorf("cannot persist DNS ownership; sync stopped: %w", err)
	}
	return nil
}
