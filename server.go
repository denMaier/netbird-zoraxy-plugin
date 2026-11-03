package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/ui.html
var uiHTML string

type app struct {
	ctx      context.Context // Ends at shutdown. A closed browser tab must not interrupt a sync.
	gate     sync.Mutex      // Serialize sync and configuration changes, including ledger writes.
	mu       sync.Mutex      // Short-lived snapshot lock; the UI stays responsive during a sync.
	config   config
	state    ownershipState
	last     *syncResult
	dir      string
	instance string
	source   routeSource
	csrf     string
	wake     chan struct{}
}

func newApp(dir, instance string, source routeSource) (*app, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	a := &app{ctx: context.Background(), dir: dir, instance: instance, source: source, config: defaultConfig(), state: newState(), wake: make(chan struct{}, 1)}
	if err := readJSON(filepath.Join(dir, "settings.json"), &a.config); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	a.config.normalize()
	// A missing token is not an error here: Zoraxy disables a plugin that exits.
	if err := a.config.validate(false); err != nil {
		return nil, err
	}
	// Remove a token this mode must not keep, such as one saved by an older version.
	if a.config.Token != "" && !a.config.storesToken() {
		a.config.Token = ""
		if err := saveSettings(dir, a.config); err != nil {
			return nil, errors.New("cannot remove the stored token from settings.json; check data directory permissions")
		}
	}
	var persisted ownershipState
	if err := readJSON(filepath.Join(dir, "state.json"), &persisted); err == nil {
		a.state = persisted
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if a.state.Version != 1 || a.state.DNS == nil {
		return nil, errors.New("unsupported or incomplete state.json; restore the ownership file before starting")
	}
	for _, record := range a.state.DNS {
		if !validID(record.ID) || record.Name == "" || record.Type == "" || record.Content == "" {
			return nil, errors.New("invalid DNS ownership entry in state.json")
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	a.csrf = hex.EncodeToString(b)
	return a, nil
}

func (a *app) snapshot() config {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.config
	c.GroupIDs = append([]string{}, c.GroupIDs...)
	return c
}

func (a *app) synchronize(ctx context.Context, preview bool) (syncResult, error) {
	if !a.gate.TryLock() {
		return syncResult{}, errors.New("a sync or settings update is already running")
	}
	defer a.gate.Unlock()
	c := a.snapshot()
	if preview {
		c.DryRun = true
	}
	result := syncResult{Time: time.Now().UTC(), DryRun: c.DryRun, Domains: []string{}, Changes: []change{}}
	err := c.validate(true)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		n := newNetBird(c)
		result, err = buildPlan(ctx, a.source, n, c, a.instance, a.state)
		if err == nil && !result.DryRun {
			save := func() error { return writeJSON(filepath.Join(a.dir, "state.json"), a.state) }
			// Also retry any ownership persistence that failed in the previous run.
			if err = save(); err == nil {
				err = applyPlan(ctx, n, c, &result, &a.state, save)
			}
		}
		// Only a completed live sync ends a one-time run; otherwise the token is
		// still needed to retry.
		if err == nil && !result.DryRun && c.Mode == modeOnce {
			a.mu.Lock()
			a.config.Token = ""
			a.mu.Unlock()
			result.TokenDeleted = true
			log.Print("one-time sync complete; removed the NetBird token from memory")
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	a.mu.Lock()
	a.last = &result
	a.mu.Unlock()
	return result, err
}

func (a *app) run(ctx context.Context) {
	// Wait before the first sync, not only between syncs. Zoraxy starts plugins and
	// its API while it is still loading route files, and a partial route list would
	// withdraw live entries. All remote calls have timeouts; a failed discovery
	// never produces a deletion plan.
	if a.snapshot().paused() {
		log.Print("continuous sync is paused: enter the NetBird token on the settings page")
	}
	for {
		timer := time.NewTimer(time.Duration(a.snapshot().IntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.wake:
			timer.Stop()
		case <-timer.C:
		}
		if c := a.snapshot(); c.Mode != modeContinuous || c.paused() {
			continue
		}
		result, err := a.synchronize(ctx, false)
		if err != nil {
			log.Printf("sync failed: %s", err)
		} else if len(result.Changes) > 0 {
			log.Printf("sync complete: dry_run=%t domains=%d changes=%d applied=%d", result.DryRun, len(result.Domains), len(result.Changes), result.Applied)
		}
	}
}

func sendJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func sendError(w http.ResponseWriter, status int, err error) {
	sendJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeConfig(w http.ResponseWriter, r *http.Request) (config, error) {
	var c config
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, errors.New("invalid settings JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return c, errors.New("expected one settings object")
	}
	c.normalize()
	return c, nil
}

func (a *app) keepToken(c *config) error {
	previous := a.snapshot()
	if c.Token == "" {
		if strings.TrimSuffix(c.NetBirdURL, "/api") != strings.TrimSuffix(previous.NetBirdURL, "/api") && previous.Token != "" {
			return errors.New("enter a token for the new NetBird API URL")
		}
		c.Token = previous.Token
	}
	return nil
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	page := template.Must(template.New("ui").Parse(uiHTML))
	mux.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+a.csrf+"'; style-src 'nonce-"+a.csrf+"'; connect-src 'self'; base-uri 'none'; form-action 'none'")
		_ = page.Execute(w, struct{ CSRF, HostCSRF string }{a.csrf, r.Header.Get("X-Zoraxy-Csrf")})
	})
	mux.HandleFunc("GET /ui/api/status", func(w http.ResponseWriter, r *http.Request) {
		c := a.snapshot()
		hasToken := c.Token != ""
		c.Token = ""
		a.mu.Lock()
		last := a.last
		a.mu.Unlock()
		sendJSON(w, 200, map[string]any{"config": c, "has_token": hasToken, "paused": c.Mode == modeContinuous && !hasToken, "last": last})
	})
	mux.HandleFunc("POST /ui/api/config", func(w http.ResponseWriter, r *http.Request) {
		c, err := decodeConfig(w, r)
		if err != nil {
			sendError(w, 400, err)
			return
		}
		if !a.gate.TryLock() {
			sendError(w, 409, errors.New("wait for the current sync before changing settings"))
			return
		}
		saved := false
		defer func() {
			a.gate.Unlock()
			if saved {
				select {
				case a.wake <- struct{}{}:
				default:
				}
			}
		}()
		if err := a.keepToken(&c); err != nil {
			sendError(w, 400, err)
			return
		}
		if err := c.validate(c.Mode == modeContinuous); err != nil {
			sendError(w, 400, err)
			return
		}
		if err := saveSettings(a.dir, c); err != nil {
			sendError(w, 500, errors.New("cannot save settings; check data directory permissions"))
			return
		}
		a.mu.Lock()
		a.config = c
		a.mu.Unlock()
		saved = true
		sendJSON(w, 200, map[string]bool{"saved": true, "has_token": c.Token != ""})
	})
	mux.HandleFunc("POST /ui/api/catalog", func(w http.ResponseWriter, r *http.Request) {
		c, err := decodeConfig(w, r)
		if err != nil {
			sendError(w, 400, err)
			return
		}
		if err := a.keepToken(&c); err != nil {
			sendError(w, 400, err)
			return
		}
		if err := validateBaseURL(c.NetBirdURL); err != nil {
			sendError(w, 400, err)
			return
		}
		if c.Token == "" || c.validate(false) != nil {
			sendError(w, 400, errors.New("enter a NetBird URL, token, and valid settings first"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		catalog, err := newNetBird(c).catalog(ctx)
		if err != nil {
			sendError(w, 502, err)
			return
		}
		sendJSON(w, 200, catalog)
	})
	for _, endpoint := range []string{"preview", "sync"} {
		mux.HandleFunc("POST /ui/api/"+endpoint, func(w http.ResponseWriter, r *http.Request) {
			result, err := a.synchronize(a.ctx, endpoint == "preview")
			if err != nil && result.Time.IsZero() {
				sendError(w, 409, err)
				return
			}
			status := 200
			if err != nil {
				status = 502
			}
			sendJSON(w, status, result)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Protect read endpoints too: they reveal internal domains and settings.
		if strings.HasPrefix(r.URL.Path, "/ui/api/") && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Netbird-Zoraxy-Token")), []byte(a.csrf)) != 1 {
			sendError(w, 403, errors.New("reload the plugin page before using its API"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}
