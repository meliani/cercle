# Cercle plugin for Navidrome

A [Navidrome](https://www.navidrome.org/) plugin (scrobbler capability) that forwards listening
events to a Cercle hub, so the people in your circles can see what you are playing. It needs
**Navidrome 0.64 or later**.

| Navidrome event  | Sent to the hub as | Notes |
|------------------|--------------------|-------|
| `NowPlaying`     | `now_playing`      | position (s) → `position_ms`, `timestamp` = now |
| `Scrobble`       | `scrobble`         | `timestamp` from Navidrome; retried by Navidrome while the hub is down |
| `PlaybackReport` | `playback_state`   | `state` (`starting`/`playing`/`paused`/`stopped`/`expired`), `position_ms`, `player` |

Events go to `POST <hub_url>/ingest/v1/events` with `Authorization: Bearer <ingest_secret>`
(see [`docs/CONTRACT.md`](../docs/CONTRACT.md) §2). Navidrome only gives scrobbler plugins the
username and track metadata, so the plugin looks up the album id, artist id, year and genre with
the internal Subsonic `getSong` call (cached per track) and the display name with the users
service. If a lookup fails the event is still sent, without those fields.

## Install

1. Enable plugins in Navidrome (`navidrome.toml` or the matching `ND_PLUGINS_*` env vars):

   ```toml
   [Plugins]
   Enabled = true
   Folder = "/data/plugins"   # default: <DataFolder>/plugins
   ```

2. Download `cercle.ndp` from the
   [releases](https://github.com/meliani/cercle/releases) (tags `plugin-v*`) or build it (below),
   and copy it into the plugins folder. The file name (without `.ndp`) is the plugin id.
3. Rescan: run `navidrome plugin rescan` or restart Navidrome (with `AutoReload = true` Navidrome
   watches the folder and picks the file up by itself). It then shows up on the **Plugins** page.
4. Open the **Cercle** plugin and configure it:
   - **Hub URL** (`hub_url`): base URL of the hub *as Navidrome reaches it*, e.g.
     `http://cercle-hub:8080/hub` or `https://navidrome.example.com/hub`.
   - **Ingest secret** (`ingest_secret`): the hub's `INGEST_SECRET` (≥ 32 characters).
5. Under **Users**, turn on **Allow all users**. Navidrome only forwards events for users assigned to
   the plugin, and Cercle is visible-by-default: everyone starts in the `everyone` circle and each
   person controls sharing from the Cercle privacy page. Pick individual users instead if you want an
   opt-in rollout.
6. Review the permissions and enable the plugin.

From the command line, steps 4–6 are:

```sh
navidrome plugin edit cercle --all-users \
  --config '{"hub_url":"http://cercle-hub:8080/hub","ingest_secret":"<secret>"}'
navidrome plugin enable cercle
```

### Permissions it asks for

- `users` – required by Navidrome for scrobbler plugins; also used to read display names.
- `http` with `requiredHosts: ["*"]` – the hub usually sits on a private network, and since 0.64
  Navidrome blocks private addresses for host-name-only entries. You can narrow this to your hub's
  address (an IP or CIDR is needed for private addresses).
- `subsonicapi` – `getSong` for album/artist ids, year and genre.
- `cache` – caches those lookups and display names in memory.

### What gets shared

- Navidrome's own filters apply before events reach the plugin: the per-user scrobble filter keeps
  matching songs out, and a player with scrobbling turned off sends no now-playing or scrobbles.
  Anything Navidrome would not scrobble is never sent to the hub.
- Everything else (which circles see what, pausing, history delay, hidden artists) is enforced by
  the hub, not the plugin.

### Failure behaviour

- `now_playing` / `playback_state`: one attempt (5 s timeout); failures are logged and dropped. They
  are superseded within seconds and never block playback.
- `scrobble`: on a network error, timeout, 408, 429 or 5xx the plugin answers `retry_later`
  (honouring `Retry-After`), so Navidrome keeps the play in its scrobble buffer and retries with
  backoff until the hub is back. Any other 4xx (for example 401 from a wrong secret) is
  `unrecoverable`: it is logged and the play is dropped.
- If `hub_url` or `ingest_secret` is missing, the plugin reports users as not authorized and logs a
  warning (at most once an hour); Navidrome then sends it nothing.

## Build

Requires Go ≥ 1.25 (the Navidrome PDK's minimum).

```sh
make test          # unit tests for the event → payload mapping (native)
make package       # dist/cercle.ndp
make docker-build  # same, inside golang:1.25-alpine
```

The `.ndp` is a zip containing exactly `manifest.json` and `plugin.wasm`
(`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`). Validate it with
`navidrome plugin validate dist/cercle.ndp`.

Layout: `main.go` is the wasm entry point (PDK wiring, host calls); `internal/ingest` holds the
mapping, `getSong` parsing and error classification in plain Go so it tests without wasm.
