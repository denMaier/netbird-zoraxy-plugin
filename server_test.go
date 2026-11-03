package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSettingsAndUI(t *testing.T) {
	f := newNetbirdFixture(t)
	a := testApp(t, testConfig(f), &routeFixture{routes: []proxyRoute{}})
	h := a.handler()
	request := func(method, path string, value any, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		if value != nil {
			if err := json.NewEncoder(&body).Encode(value); err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest(method, path, &body)
		if csrf {
			r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("GET", "/ui/", nil, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), a.csrf) || strings.Contains(w.Body.String(), "test-secret") {
		t.Fatalf("page render: %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "nonce-"+a.csrf) {
		t.Fatal("missing CSP nonce")
	}
	for _, path := range []string{"status", "catalog", "config", "preview", "sync"} {
		method := "POST"
		if path == "status" {
			method = "GET"
		}
		if w := request(method, "/ui/api/"+path, a.config, false); w.Code != 403 {
			t.Fatalf("unguarded endpoint %s: %d", path, w.Code)
		}
	}
	w = request("GET", "/ui/api/status", nil, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "test-secret") || !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatal("status exposed or lost token")
	}
	c := a.snapshot()
	c.Token = ""
	c.Tag = "publish"
	w = request("POST", "/ui/api/config", c, true)
	if w.Code != 200 || a.snapshot().Token != "test-secret" || a.snapshot().Tag != "publish" {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	c.GroupIDs = nil
	if w := request("POST", "/ui/api/config", c, true); w.Code != 400 {
		t.Fatal("continuous sync accepted incomplete settings")
	}
	if len(a.snapshot().GroupIDs) == 0 {
		t.Fatal("invalid settings replaced saved settings")
	}
	c = a.snapshot()
	c.Mode = "sometimes"
	if w := request("POST", "/ui/api/config", c, true); w.Code != 400 {
		t.Fatal("accepted an unknown sync mode")
	}
	c = a.snapshot()
	c.TokenStorage = "keychain"
	if w := request("POST", "/ui/api/config", c, true); w.Code != 400 {
		t.Fatal("accepted an unknown token storage")
	}
	c = a.snapshot()
	c.NetBirdURL = "https://another.example.com"
	c.Token = ""
	if w := request("POST", "/ui/api/config", c, true); w.Code != 400 {
		t.Fatal("reused stored token with a different API origin")
	}
	if w := request("POST", "/ui/api/catalog", c, true); w.Code != 400 {
		t.Fatal("sent stored token to a different catalog origin")
	}
	w = request("POST", "/ui/api/catalog", a.snapshot(), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Home") || strings.Contains(w.Body.String(), "test-secret") {
		t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
	}
	// A manual preview remains read-only even when live writes are configured.
	c = a.snapshot()
	c.DryRun = false
	if w := request("POST", "/ui/api/config", c, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request("POST", "/ui/api/preview", struct{}{}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"dry_run":true`) || len(f.writes) != 0 {
		t.Fatal("preview wrote changes")
	}
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]os.FileMode{"": 0700, "settings.json": 0600} {
			info, err := os.Stat(filepath.Join(a.dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode {
				t.Fatalf("%s permissions %o", name, info.Mode().Perm())
			}
		}
	}
	// Config changes cannot race an in-flight reconciliation.
	a.gate.Lock()
	w = request("POST", "/ui/api/config", c, true)
	a.gate.Unlock()
	if w.Code != 409 {
		t.Fatal("settings changed during a sync")
	}
}

func TestHTTPClientDoesNotFollowRedirectsOrExposeSecrets(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	err := callJSON(context.Background(), newHTTPClient(), "GET", redirect.URL, "Token sensitive", nil, new(any))
	if err == nil || followed {
		t.Fatal("followed credential-bearing redirect")
	}
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, "sensitive token: "+r.Header.Get("Authorization"))
	}))
	defer failure.Close()
	err = callJSON(context.Background(), newHTTPClient(), "GET", failure.URL, "Token sensitive", nil, nil)
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("upstream error exposed a credential")
	}
	reason := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		fmt.Fprint(w, `{"message":"bad domain; sent `+r.Header.Get("Authorization")+`","internal":"hidden"}`)
	}))
	defer reason.Close()
	err = callJSON(context.Background(), newHTTPClient(), "GET", reason.URL, "Token sensitive", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "bad domain") || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "hidden") {
		t.Fatalf("upstream reason: %v", err)
	}
}

func TestConfigurationValidation(t *testing.T) {
	for _, raw := range []string{"https://api.netbird.io", "https://netbird.example.com/api", "http://127.0.0.1:33073", "http://[::1]:33073", "http://localhost:33073"} {
		if err := validateBaseURL(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://netbird.example.com", "https://user:password@netbird.example.com", "https://example.com?token=secret", "file:///tmp/x", "https://example.com/api/networks", "https://example.com/api%2f..", "//example.com"} {
		if err := validateBaseURL(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	c := defaultConfig()
	if c.Mode != modeOnce || c.TokenStorage != storageMemory || c.storesToken() || !c.DryRun {
		t.Fatal("unsafe defaults")
	}
	if c.validate(false) != nil || c.validate(true) == nil {
		t.Fatal("partial settings validation failed")
	}
	if validID("../other") || validID("net1/query?delete=true") {
		t.Fatal("accepted unsafe API ID")
	}
}

func TestSchedulerStopsOnCancellation(t *testing.T) {
	a, err := newApp(t.TempDir(), "instance1", &routeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler ignored cancellation")
	}
}

type signalingSource struct{ called chan struct{} }

func (s signalingSource) Routes(context.Context) ([]proxyRoute, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return []proxyRoute{}, nil
}

func TestSettingsWakeScheduler(t *testing.T) {
	f := newNetbirdFixture(t)
	c := testConfig(f)
	c.IntervalSeconds = 86400
	source := signalingSource{called: make(chan struct{}, 1)}
	a := testApp(t, c, source)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.run(ctx) }()
	defer func() { cancel(); <-done }()
	// Zoraxy may still be loading routes at startup, so no sync may run yet.
	select {
	case <-source.called:
		t.Fatal("synced immediately on startup")
	case <-time.After(100 * time.Millisecond):
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/ui/api/config", bytes.NewReader(body))
	r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-source.called:
	case <-time.After(2 * time.Second):
		t.Fatal("settings update did not wake the scheduler")
	}
}

// Zoraxy's iframe sandbox only has allow-scripts and allow-same-origin, so these fail silently.
func TestUIAvoidsFeaturesBlockedByZoraxySandbox(t *testing.T) {
	for _, blocked := range []string{"<form", "confirm(", "alert(", "prompt(", "navigator.clipboard", "method=\"dialog\""} {
		if strings.Contains(uiHTML, blocked) {
			t.Errorf("UI uses %s", blocked)
		}
	}
}

func TestUIElementIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, match := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(uiHTML, -1) {
		if seen[match[1]] {
			t.Errorf("duplicate element ID %q", match[1])
		}
		seen[match[1]] = true
	}
}

func TestMalformedSettingsAreRejected(t *testing.T) {
	a, err := newApp(t.TempDir(), "instance1", &routeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{`, `{} {}`, `{"unknown":"field"}`, strings.Repeat("x", 70<<10)} {
		r := httptest.NewRequest("POST", "/ui/api/config", strings.NewReader(body))
		r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed body accepted: %d", w.Code)
		}
	}
}

func TestCancelledRequestsStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := callJSON(ctx, newHTTPClient(), "GET", "http://127.0.0.1:1/api", "Token test", nil, nil)
	if err != context.Canceled {
		t.Fatalf("%v", err)
	}
}

func TestUIJSONContainsNoStoredToken(t *testing.T) {
	f := newNetbirdFixture(t)
	a := testApp(t, testConfig(f), &routeFixture{routes: []proxyRoute{}})
	server := httptest.NewServer(a.handler())
	defer server.Close()
	r, _ := http.NewRequest("GET", server.URL+"/ui/api/status", nil)
	r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Config   config `json:"config"`
		HasToken bool   `json:"has_token"`
	}
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatal(err)
	}
	if status.Config.Token != "" || !status.HasToken {
		t.Fatal("token redaction failed")
	}
}

func TestZoraxyUIProxyAndHostCSRF(t *testing.T) {
	f := newNetbirdFixture(t)
	a := testApp(t, testConfig(f), &routeFixture{routes: []proxyRoute{}})
	upstream := httptest.NewServer(a.handler())
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Zoraxy protects /plugin.ui/ with its own CSRF middleware, strips
		// the plugin prefix, and supplies the host token in this header.
		if r.Method == "POST" && r.Header.Get("X-CSRF-Token") != "host-csrf" {
			http.Error(w, "host CSRF missing", 403)
			return
		}
		r.Header.Set("X-Zoraxy-Csrf", "host-csrf")
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/plugin.ui/"+pluginID)
		proxy.ServeHTTP(w, r)
	}))
	defer host.Close()
	pageURL := host.URL + "/plugin.ui/" + pluginID + "/ui/"
	resp, err := http.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(page), `name="zoraxy-csrf-token" content="host-csrf"`) {
		t.Fatal("host CSRF token was not embedded")
	}
	postURL, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	postURL = postURL.ResolveReference(&url.URL{Path: "api/preview"})
	r, _ := http.NewRequest("POST", postURL.String(), strings.NewReader("{}"))
	r.Header.Set("X-CSRF-Token", "host-csrf")
	r.Header.Set("X-Netbird-Zoraxy-Token", a.csrf)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxied settings request failed: %d", resp.StatusCode)
	}
}
