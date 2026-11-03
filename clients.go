package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // Never forward a credential to a redirect target.
	}}
}

func callJSON(ctx context.Context, client *http.Client, method, endpoint, authorization string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return errors.New("cannot build API request")
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("API connection failed; check the URL, TLS certificate, and connectivity")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned HTTP %d for %s %s%s", resp.StatusCode, method, req.URL.Path, apiMessage(resp.Body, authorization))
	}
	if out == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil || len(b) > 8<<20 {
		return errors.New("cannot read API response (maximum 8 MiB)")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("API returned an unexpected JSON response")
	}
	return nil
}

// apiMessage returns NetBird's short error reason. Nothing else from an error
// body is used, because it may hold internals, and the credential is removed.
func apiMessage(body io.Reader, authorization string) string {
	var failure struct {
		Message string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(body, 4<<10)).Decode(&failure) != nil || failure.Message == "" {
		return ""
	}
	_, secret, _ := strings.Cut(authorization, " ")
	message := strings.ReplaceAll(failure.Message, secret, "[redacted]")
	if len(message) > 200 {
		message = message[:200]
	}
	return ": " + message
}

type proxyRoute struct {
	Hostname string   `json:"RootOrMatchingDomain"`
	Aliases  []string `json:"MatchingDomainAlias"`
	Disabled bool     `json:"Disabled"`
	Tags     []string `json:"Tags"`
}

type routeSource interface {
	Routes(context.Context) ([]proxyRoute, error)
}

type zoraxyClient struct {
	baseURL, token string
	http           *http.Client
}

func (z *zoraxyClient) Routes(ctx context.Context) ([]proxyRoute, error) {
	var routes []proxyRoute
	if err := callJSON(ctx, z.http, "GET", z.baseURL+"/plugin/api/proxy/list?type=host", "Bearer "+z.token, nil, &routes); err != nil {
		return nil, fmt.Errorf("Zoraxy route discovery: %w", err)
	}
	if routes == nil {
		return nil, errors.New("Zoraxy returned null instead of a route list; sync stopped")
	}
	return routes, nil
}

type group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type resource struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Address     string  `json:"address"`
	Enabled     bool    `json:"enabled"`
	Groups      []group `json:"groups"`
}

type resourceRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Address     string   `json:"address"`
	Enabled     bool     `json:"enabled"`
	Groups      []string `json:"groups"`
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

type zone struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Domain             string   `json:"domain"`
	Enabled            bool     `json:"enabled"`
	DistributionGroups []string `json:"distribution_groups"`
}

type netbirdClient struct {
	baseURL, token string
	http           *http.Client
}

func newNetBird(c config) *netbirdClient {
	return &netbirdClient{baseURL: strings.TrimSuffix(c.NetBirdURL, "/api"), token: c.Token, http: newHTTPClient()}
}

func (n *netbirdClient) call(ctx context.Context, method, path string, body, out any) error {
	return callJSON(ctx, n.http, method, n.baseURL+"/api"+path, "Token "+n.token, body, out)
}

func (n *netbirdClient) resources(ctx context.Context, networkID string) ([]resource, error) {
	// NetBird answers null, not [], for a Network without resources. Without any
	// owned resource to find, a null can only lead to creates, never to deletes.
	var resources []resource
	if err := n.call(ctx, "GET", "/networks/"+networkID+"/resources", nil, &resources); err != nil {
		return nil, err
	}
	for _, r := range resources {
		if !validID(r.ID) || r.Address == "" || r.Name == "" {
			return nil, errors.New("NetBird returned an incomplete resource")
		}
	}
	return resources, nil
}

type catalog struct {
	Networks []group `json:"networks"`
	Groups   []group `json:"groups"`
	Zones    []zone  `json:"zones"`
	DNSNote  string  `json:"dns_note,omitempty"`
}

func (n *netbirdClient) catalog(ctx context.Context) (catalog, error) {
	c := catalog{Networks: []group{}, Groups: []group{}, Zones: []zone{}}
	if err := n.call(ctx, "GET", "/networks", nil, &c.Networks); err != nil {
		return c, err
	}
	if err := n.call(ctx, "GET", "/groups", nil, &c.Groups); err != nil {
		return c, err
	}
	// DNS is optional: older NetBird installations may not have the zones API.
	if err := n.call(ctx, "GET", "/dns/zones", nil, &c.Zones); err != nil {
		c.DNSNote = "DNS zones could not be loaded: " + err.Error()
	}
	return c, nil
}
