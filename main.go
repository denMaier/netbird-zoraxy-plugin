package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	pluginID = "io.github.denmaier.zoraxy.netbird"
	// Zoraxy starts plugins with their own folder as the working directory.
	dataDir = "data"
	// Zoraxy force-stops a plugin about five seconds after SIGTERM.
	shutdownTimeout = 4 * time.Second
)

// These wire types implement Zoraxy's public plugin protocol; no SDK is required.
// https://github.com/tobychui/zoraxy/tree/main/src/mod/plugins/zoraxy_plugin
type runtimeConfig struct {
	Port       int    `json:"port"`
	APIKey     string `json:"api_key"`
	ZoraxyPort int    `json:"zoraxy_port"`
	Constants  struct {
		UUID string `json:"zoraxy_uuid"`
	} `json:"runtime_const"`
}

func introspection() any {
	return map[string]any{
		"id": pluginID, "name": "NetBird Route Sync", "author": "Denis Maier",
		"author_contact": "https://github.com/denMaier", "url": "https://github.com/denMaier/netbird-zoraxy-plugin",
		"description":   "Publish tagged Zoraxy hostnames to NetBird Networks and private DNS.",
		"type":          1,
		"version_major": 0, // x-release-please-major
		"version_minor": 1, // x-release-please-minor
		"version_patch": 0, // x-release-please-patch
		"ui_path":       "/ui",
		"permitted_api_endpoints": []map[string]string{{
			"method": "GET", "endpoint": "/plugin/api/proxy/list",
			"reason": "Read tagged reverse-proxy routes and aliases to synchronize NetBird resources.",
		}},
	}
}

func main() {
	inspect := flag.Bool("introspect", false, "print the Zoraxy plugin manifest")
	configure := flag.String("configure", "", "runtime configuration supplied by Zoraxy")
	flag.Parse()
	if *inspect {
		if err := json.NewEncoder(os.Stdout).Encode(introspection()); err != nil {
			log.Fatal("cannot write plugin manifest")
		}
		return
	}
	var runtime runtimeConfig
	if json.Unmarshal([]byte(*configure), &runtime) != nil || runtime.Port < 1 || runtime.Port > 65535 ||
		runtime.ZoraxyPort < 1 || runtime.ZoraxyPort > 65535 || runtime.APIKey == "" || runtime.Constants.UUID == "" {
		log.Fatal("start this utility plugin from Zoraxy; a valid -configure payload with API access is required")
	}
	app, err := newApp(dataDir, runtime.Constants.UUID, &zoraxyClient{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", runtime.ZoraxyPort), token: runtime.APIKey, http: newHTTPClient(),
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.ctx = ctx
	server := &http.Server{
		Addr: net.JoinHostPort("127.0.0.1", fmt.Sprint(runtime.Port)), Handler: app.handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 3 * time.Minute,
		IdleTimeout: 60 * time.Second,
	}
	scheduler := make(chan struct{})
	go func() { defer close(scheduler); app.run(ctx) }()
	go func() {
		log.Printf("NetBird Route Sync listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	// A stopping sync finishes its current write, so the ledger stays accurate.
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = server.Shutdown(shutdown)
	select {
	case <-scheduler:
	case <-shutdown.Done():
	}
}
