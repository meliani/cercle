# cercle-hub

The server side of **Cercle**, a self-hosted social layer for [Navidrome](https://www.navidrome.org/):
see what your circles are listening to, share stats, react and comment, and listen together.

The hub receives listening events from the Cercle Navidrome plugin, stores plays in SQLite,
groups them into a feed, and serves a REST + WebSocket API to the player. Every piece of data it
returns goes through one visibility rule (circle membership × per-circle sharing flags × pause,
history delay and hidden items) — see [`docs/CONTRACT.md`](../docs/CONTRACT.md) §1.

- Single static Go binary, no cgo (`modernc.org/sqlite`), ~11 MB.
- Users are Navidrome users; the hub never stores passwords. Logins are checked against
  Navidrome's Subsonic API (`ping`, `getUser`, `tokenInfo`) and exchanged for an opaque hub token
  (only its SHA-256 is stored).
- Also serves a privacy-filtered Subsonic `getNowPlaying` so third-party Subsonic apps don't see
  what everyone is playing (contract §5b).

## API

The contract is [`docs/CONTRACT.md`](../docs/CONTRACT.md) (authoritative, including the
"Decisions (hub)" section) and the machine-readable spec is
[`docs/openapi.yaml`](../docs/openapi.yaml).

| Surface | Path (under `CERCLE_BASE_PATH`) |
|---|---|
| Health | `GET /health` (also at `/health` when a base path is set) |
| Ingest (plugin → hub) | `POST /ingest/v1/events` with `Authorization: Bearer $INGEST_SECRET` |
| REST (player → hub) | `/api/v1/…` with `Authorization: Bearer <token>` from `POST /api/v1/auth/session` |
| WebSocket | `GET /api/v1/ws?token=<token>` |
| Subsonic shim | `GET\|POST /rest/getNowPlaying[.view]` (also at the root `/rest/…`) |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NAVIDROME_URL` | — (required) | Base URL the hub uses to validate users, e.g. `http://navidrome:4533` |
| `INGEST_SECRET` | — (required, ≥ 32 chars) | Shared secret with the Navidrome plugin |
| `CERCLE_LISTEN` | `:8080` | Listen address |
| `CERCLE_DB` | `/data/cercle.db` | SQLite path (WAL, `busy_timeout=5000`, foreign keys on) |
| `CERCLE_BASE_PATH` | `` | Serve everything under this prefix, e.g. `/hub` |
| `CERCLE_CORS_ORIGINS` | `` | Comma-separated browser origins allowed to call the API (`*` allowed) |
| `TZ` | `UTC` | Time zone for stats buckets (hour, weekday, day, year) |
| `TRUSTED_HEADER` | `` | Optional: header carrying the username set by a trusted reverse proxy |
| `TRUSTED_PROXIES` | `` | CIDRs/IPs of proxies trusted for `TRUSTED_HEADER` and `X-Forwarded-For` |
| `CERCLE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` (JSON logs on stdout) |
| `NAVIDROME_ADMIN_USER` | `` | Optional Navidrome admin account for user sync (set both or neither) |
| `NAVIDROME_ADMIN_PASSWORD` | `` | Its password; only a salted token (`t`/`s`) is ever sent, never logged |
| `CERCLE_USER_SYNC_INTERVAL` | `15m` | How often to sync users from Navidrome (≥ `1m`; also once at startup) |
| `CERCLE_EXCLUDED_USERS` | `` | Comma-separated service accounts (case-insensitive), e.g. `admin` |
| `CERCLE_EVERYONE_NAME` | `` | If set, applied to the `everyone` circle at startup (default name "Everyone") |
| `CERCLE_EVERYONE_DESCRIPTION` | `` | If set, applied likewise (default "Everyone on this server") |

### Users: sync and exclusions

Without sync, a user appears in Cercle (and in `everyone`) at their first login or first play.
With `NAVIDROME_ADMIN_USER`/`NAVIDROME_ADMIN_PASSWORD` set, the hub calls Subsonic `getUsers` at
startup and every `CERCLE_USER_SYNC_INTERVAL`: new Navidrome users are created and join
`everyone` right away, `is_admin` follows `adminRole`, and users deleted from Navidrome are
*disabled* — hidden from lists, member counts, live and feed, sessions revoked, data kept — and
re-enabled if they come back. A dedicated admin account is fine; list it in
`CERCLE_EXCLUDED_USERS` if it should not appear in Cercle.

Excluded users are never created or auto-joined, their plugin events are accepted and dropped,
and they cannot log in (`403 excluded_user`). Adding an existing user to the list removes them
from every circle at the next start (ownership passes to the senior admin, else the senior
member; a circle they were alone in is deleted).

Subsonic `getUsers` carries no display name, so display names still come from the plugin
(when it sends `user.name`) or from the user's own edit; until then the username is shown.

## Run with Docker

```sh
docker run -d --name cercle-hub \
  -e NAVIDROME_URL=http://navidrome:4533 \
  -e INGEST_SECRET="$(openssl rand -hex 32)" \
  -e CERCLE_BASE_PATH=/hub \
  -e CERCLE_CORS_ORIGINS=https://navidrome.example.com \
  -v cercle-data:/data -p 8080:8080 \
  ghcr.io/meliani/cercle-hub:latest
```

Compose, next to Navidrome:

```yaml
services:
  cercle-hub:
    image: ghcr.io/meliani/cercle-hub:latest
    environment:
      NAVIDROME_URL: http://navidrome:4533
      INGEST_SECRET: ${CERCLE_INGEST_SECRET}
      CERCLE_BASE_PATH: /hub
      CERCLE_CORS_ORIGINS: https://navidrome.example.com
      TZ: Europe/Paris
    volumes:
      - cercle-data:/data
    healthcheck:
      test: ["CMD", "/usr/local/bin/cercle-hub", "healthcheck"]
      interval: 30s
    restart: unless-stopped
volumes:
  cercle-data:
```

Point the plugin's `hub_url` at `http://cercle-hub:8080/hub` with the same `ingest_secret`.
To keep third-party apps from seeing everyone's now-playing, have your reverse proxy route
`/rest/getNowPlaying` and `/rest/getNowPlaying.view` to the hub instead of Navidrome.

The image is distroless and runs as `nonroot` (uid 65532); the `/data` volume holds the database.
`cercle-hub healthcheck` exits 0 when `/health` answers 200. `SIGTERM` shuts down gracefully.

## Development

```sh
cd hub
go vet ./...
go test ./...          # uses a fake Navidrome (httptest) and temp SQLite files
go run ./cmd/cercle-hub
```

Layout: `cmd/cercle-hub` (entry point), `internal/visibility` (the rule — the only place that
decides who sees what), `internal/ingest` (plays, feed grouping, discoveries),
`internal/feed`, `internal/stats`, `internal/live`, `internal/rooms`, `internal/ws`,
`internal/auth`, `internal/subsonic`, `internal/api` (HTTP wiring), `internal/migrations`
(embedded numbered SQL).
