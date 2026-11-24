<img src="assets/icon.svg" width="64" height="64" alt="">

# NetBird Route Sync for Zoraxy

A Zoraxy plugin that publishes reverse-proxy hostnames to NetBird. Tag a route `netbird` and its hostname and aliases become NetBird **domain resources**. Optionally, the plugin also creates private DNS records that point those names at Zoraxy.

Users keep their URLs, certificates and Zoraxy sign-in. Backend addresses are never published.

```text
Zoraxy route: grafana.example.com -> 192.168.1.20:3000   [tag: netbird]
                      |
                      |  read once, or every 60 s
                      v
NetBird Route Sync ---------> domain resource  grafana.example.com
                      |
                      +-----> DNS record (optional)  grafana.example.com -> Zoraxy
```

That's all it does. It doesn't create Networks, routing peers, access policies or DNS zones; you set those up in NetBird. It also doesn't close public listeners, handle URL paths or stream proxies, or map tags to different groups.

## Requirements

- Zoraxy with the plugin REST API. Tested with 3.3.4.
- NetBird with Networks and domain resources. Private DNS also needs the DNS Zones API. Tested with self-hosted 0.82.0.
- A NetBird personal access token that can list Networks and Groups and manage resources. DNS sync also needs to read the zone and manage its records.
- In NetBird: an existing Network with a routing peer, one or more resource groups, and access policies.

## Install

Download the binary for your platform from the [latest release](https://github.com/denMaier/netbird-zoraxy-plugin/releases/latest), check it against `SHA256SUMS`, and put it in a folder with the same name as the binary:

```sh
mkdir -p /opt/zoraxy/plugins/netbird
curl -L -o /opt/zoraxy/plugins/netbird/netbird \
  https://github.com/denMaier/netbird-zoraxy-plugin/releases/latest/download/netbird_linux_amd64
chmod +x /opt/zoraxy/plugins/netbird/netbird
```

Restart Zoraxy, because it only discovers new plugins at startup. Then enable **NetBird Route Sync** under **Plugins** and accept its one permission: `GET /plugin/api/proxy/list`, which reads the routes.

To update, replace the binary and restart the plugin. Keep the `data/` folder next to it.

## Set up

1. Open the plugin page. Enter the NetBird API URL (`https://api.netbird.io` for Cloud) and your token. Self-hosted URLs must use HTTPS.
2. Click **Load Networks, groups & DNS zones**. Select the Network and resource groups.
3. Optional: select a DNS zone and enter Zoraxy's reachable IP or fully qualified peer hostname.
4. Click **Save settings**, add the tag to a route in Zoraxy, then click **Preview saved settings**.
5. Choose a sync mode.
6. When the preview looks right, turn off **Dry run**, save, and click **Sync saved settings now**.

**Dry run is on by default.** Preview never writes, whatever the settings say.

## Sync modes

**One time** is the default. The plugin syncs when you click the button, and the token is only ever held in memory. A successful live sync removes it. Previews and dry runs keep it, and so do failed syncs so you can retry. If the plugin restarts before the sync, enter the token again.

- Published entries stay in NetBird.
- Later route changes, including removed tags, reach NetBird only when you enter a token and sync again.

**Continuous** syncs by itself. It waits one interval after startup (default 60 s, minimum 30), then runs at that interval. Saving settings starts a sync straight away. You choose where the token is kept:

| | In memory only (default) | In a plain-text file |
|---|---|---|
| **After a restart** | Sync pauses until you enter the token again. The settings page and log say so. | Sync resumes by itself |
| **On disk** | Never | Unencrypted in `data/settings.json` |
| **In backups and snapshots** | No | Yes |

While sync is paused, published entries stay in NetBird, but route changes wait.

**The token stays valid in NetBird** until it expires or you revoke it there. The plugin can't revoke it, so give it a short expiry. It can't be limited to one Network, so use a service user with the lowest role that works.

## DNS and routing

**Zoraxy behind a routing peer.** Use its LAN address as the DNS target. Give client groups access to the resource group on Zoraxy's ports, usually TCP 443, plus UDP 443 for HTTP/3. Distribute the DNS zone to the client groups and to the routing peer's group.

**Zoraxy on a NetBird peer.** Use a CNAME to the peer's full NetBird hostname. Access to the peer itself needs a peer-to-peer policy; a Network resource policy doesn't grant it.

All tagged hostnames must fit inside the selected DNS zone, and a CNAME target must be outside it. Private DNS only takes exact hostnames. Wildcards such as `*.example.com` work as resources with DNS sync off. A private zone shadows public DNS for its whole suffix, so choose its scope with care.

## Security

**NetBird does not isolate sites from each other.** Its policies control IPs, protocols and ports. They don't look at hostnames or URL paths. If several Zoraxy sites share an IP and port, access to that listener reaches all of them. Keep Zoraxy's authentication on.

**Tagging doesn't close anything.** A site that's public stays public until you change your firewall or Zoraxy.

The token only reaches the disk if you choose the plain-text file in continuous mode. It's then unencrypted in `data/settings.json`, and backups of that folder contain it. Encrypting it next to its key wouldn't help, so the plugin doesn't pretend to. Leaving file storage removes the token from the file, but backups taken earlier keep their copy. Root on the Zoraxy host can read a token in memory in any mode. The folder has mode `0700` and the files `0600`. The token is never shown in the UI, logged or sent to a different API URL: changing the URL requires entering a token again. Redirects aren't followed, so the token can't leak through one.

The settings page is served through Zoraxy and uses Zoraxy's sign-in. Its local port listens on `127.0.0.1` only, so anyone with a shell on the Zoraxy host can reach it. They can change settings, but the settings page never returns the token.

## What it will and won't touch

The plugin only changes entries it created:

- **Resources** carry an ownership description with Zoraxy's instance UUID.
- **DNS records** have no such field, so their IDs and values are kept in `data/state.json`. **Back up the whole `data/` folder.**

An existing resource or record with the same hostname is never adopted, even if its values match. A DNS record edited outside the plugin stops the sync until it's restored.

Removing the tag, disabling a route or deleting it removes the plugin's entries on the next live sync. In one-time mode that sync needs a token again.

## When things go wrong

The plugin checks everything before its first write. A failed route read, an invalid tagged hostname, a conflict or an unavailable zone stops the run with nothing changed.

Writes across two NetBird objects aren't a transaction. If a run fails halfway, the next run finishes the job, with one exception: if a DNS create succeeds but its response is lost, the new record looks like someone else's. The sync reports a conflict for that hostname. Delete that record in NetBird and the next sync recreates it.

If `data/state.json` is lost, every published DNS record becomes a conflict. Restore the backup, or delete those records in NetBird and sync again.

Only the **currently selected** API, Network and zone are synced. Changing them, or uninstalling, doesn't clean up the old ones. To move or uninstall, first remove the tags and run a live sync with the old settings.

Run one plugin instance per `data/` folder.

## Development

```sh
make test     # gofmt, go vet, go test -race
make e2e      # the sync against a real NetBird server in Docker
make build    # dist/netbird/netbird
make release  # static linux binaries and SHA256SUMS
```

There are no Go dependencies or frontend build steps.

| File | What it does |
|---|---|
| `main.go` | Zoraxy startup protocol and shutdown |
| `clients.go` | HTTP clients for Zoraxy and NetBird |
| `sync.go` | Plans and applies changes |
| `server.go` | Settings API and scheduler |
| `config.go` | Settings, validation and atomic file writes |
| `web/ui.html` | Settings page, embedded in the binary |

The settings page runs in a Zoraxy iframe that blocks forms and dialogs, so it only uses plain buttons.

Releases are automatic; see [CONTRIBUTING.md](CONTRIBUTING.md).

References: [Zoraxy plugin protocol](https://github.com/tobychui/zoraxy/blob/main/src/mod/plugins/zoraxy_plugin/zoraxy_plugin.go), [NetBird API](https://github.com/netbirdio/netbird/blob/main/shared/management/http/api/openapi.yml), [Networks](https://docs.netbird.io/manage/networks), [private DNS zones](https://docs.netbird.io/manage/dns/custom-zones).

## License

[AGPL-3.0](LICENSE)
