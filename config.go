package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	modeOnce       = "once"       // Sync when asked; a successful live sync deletes the token.
	modeContinuous = "continuous" // Keep the token and sync at the interval.

	storageMemory = "memory" // Sync pauses after a restart until the token is entered again.
	storageFile   = "file"   // Plain text in settings.json, so also in backups of that file.
)

type config struct {
	NetBirdURL      string   `json:"netbird_url"`
	Token           string   `json:"token,omitempty"`
	NetworkID       string   `json:"network_id"`
	GroupIDs        []string `json:"group_ids"`
	Tag             string   `json:"tag"`
	IntervalSeconds int      `json:"interval_seconds"`
	Mode            string   `json:"mode"`
	TokenStorage    string   `json:"token_storage"`
	DryRun          bool     `json:"dry_run"`
	DNSZoneID       string   `json:"dns_zone_id"`
	DNSTarget       string   `json:"dns_target"`
}

func defaultConfig() config {
	return config{NetBirdURL: "https://api.netbird.io", Tag: "netbird", IntervalSeconds: 60, Mode: modeOnce, TokenStorage: storageMemory, DryRun: true, GroupIDs: []string{}}
}

// storesToken is the only case where the token may reach the disk.
func (c config) storesToken() bool {
	return c.Mode == modeContinuous && c.TokenStorage == storageFile
}

// paused means continuous sync is on but has no token, as after a restart in
// memory mode.
func (c config) paused() bool {
	return c.Mode == modeContinuous && c.Token == ""
}

func (c *config) normalize() {
	c.NetBirdURL = strings.TrimRight(strings.TrimSpace(c.NetBirdURL), "/")
	c.Token = strings.TrimSpace(c.Token)
	c.NetworkID = strings.TrimSpace(c.NetworkID)
	c.DNSZoneID = strings.TrimSpace(c.DNSZoneID)
	c.DNSTarget = strings.TrimSpace(c.DNSTarget)
	c.Tag = strings.TrimSpace(c.Tag)
	for i := range c.GroupIDs {
		c.GroupIDs[i] = strings.TrimSpace(c.GroupIDs[i])
	}
	slices.Sort(c.GroupIDs)
	c.GroupIDs = slices.Compact(c.GroupIDs)
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/api") || u.RawPath != "" {
		return errors.New("NetBird URL must be an HTTPS origin, optionally ending in /api")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("use HTTPS for NetBird; HTTP is supported only on loopback addresses")
		}
	}
	return nil
}

func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (c config) validate(requireReady bool) error {
	if err := validateBaseURL(c.NetBirdURL); err != nil {
		return err
	}
	if strings.ContainsAny(c.Token, "\r\n\t ") {
		return errors.New("enter the NetBird personal access token without the Token prefix")
	}
	if c.Tag == "" || len(c.Tag) > 100 {
		return errors.New("a publish tag of 1–100 characters is required")
	}
	if c.IntervalSeconds < 30 || c.IntervalSeconds > 86400 {
		return errors.New("sync interval must be between 30 and 86400 seconds")
	}
	if c.Mode != modeOnce && c.Mode != modeContinuous {
		return errors.New("sync mode must be once or continuous")
	}
	if c.TokenStorage != storageMemory && c.TokenStorage != storageFile {
		return errors.New("token storage must be memory or file")
	}
	if c.NetworkID != "" && !validID(c.NetworkID) {
		return errors.New("invalid Network ID")
	}
	for _, id := range c.GroupIDs {
		if !validID(id) {
			return errors.New("invalid resource group ID")
		}
	}
	if c.DNSZoneID != "" {
		if !validID(c.DNSZoneID) {
			return errors.New("invalid DNS zone ID")
		}
		if _, _, err := dnsTarget(c.DNSTarget); err != nil {
			return err
		}
	}
	if requireReady && (c.Token == "" || c.NetworkID == "" || len(c.GroupIDs) == 0) {
		return errors.New("set a NetBird token, Network, and at least one resource group before syncing")
	}
	return nil
}

func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("cannot decode %s; restore a valid file before starting", filepath.Base(path))
	}
	return nil
}

// saveSettings is the only way settings reach the disk, so the token cannot be
// written by accident.
func saveSettings(dir string, c config) error {
	if !c.storesToken() {
		c.Token = ""
	}
	return writeJSON(filepath.Join(dir, "settings.json"), c)
}

// Commit via a same-directory rename. Both settings (including the token) and
// the DNS ownership ledger must survive partial writes without being truncated.
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sync-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Directory fsync is not supported on every platform. The file itself has
	// already been synced and renamed atomically.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
