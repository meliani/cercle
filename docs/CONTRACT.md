# Cercle — component contract (v1)

Cercle is a self-hosted social layer for [Navidrome](https://www.navidrome.org/): people see what
their circles are listening to, share stats, react and comment, and listen together.
Everything is opt-out-able per circle. This file is the contract between the three components;
change it first, then the code.

```
 Navidrome ──(cercle plugin: scrobbler)──► POST /ingest/v1/events ─┐
                                                                   ▼
 Player (Feishin fork, "Cercle" tab) ──REST /api/v1 + WS /api/v1/ws──► cercle-hub ──► SQLite
        │                                                          │
        └──── its own Subsonic/Navidrome calls (unchanged) ────────┴──► Navidrome /rest/ping (auth check)
```

| Component | Path | Tech |
|---|---|---|
| hub | `hub/` | Go 1.24, `net/http` (1.22 patterns), `modernc.org/sqlite` (no cgo), `github.com/coder/websocket` |
| plugin | `plugin/` | Navidrome plugin (Go PDK → `.ndp`) |
| player module | fork `meliani/feishin`, `src/renderer/features/cercle/` | React/TS inside Feishin |

Public repos: **no deployment-specific names, hosts, IPs or people in code, docs or tests.** Use
`navidrome.example.com`, users `alice`, `bob`, `carol`.

---

## 1. Concepts

- **User** — a Navidrome user, keyed by Navidrome **username** (stable, unique). Hub creates the
  record on first sight (first ingest event or first login). `display_name` defaults to the
  Navidrome name if the plugin provides it, else the username; user can change it.
- **Circle** — a group of users. Anyone can create one, invite, join with an invite, leave.
  Roles: `owner` (one; can delete, transfer), `admin` (invite, remove members, rename), `member`.
  The built-in circle **`everyone`** (`kind = "everyone"`, id `everyone`) contains every user
  automatically on creation ("visible from the start" is the concept). Users may leave it and
  re-join it at any time (`POST /api/v1/circles/everyone/join`, no invite needed). It has no owner;
  only server admins (Navidrome admins, see §3) may rename it. It cannot be deleted.
- **User sync** (optional, §6): with Navidrome admin credentials configured, the hub mirrors
  Navidrome's user list (`getUsers`) at startup and periodically, so new users are in
  `everyone` before their first login or play. Users deleted from Navidrome are **disabled**:
  invisible everywhere (lists, member counts, live, feed, stats), sessions revoked, data kept;
  they are re-enabled if they reappear (or log in / play again).
- **Excluded users** (`CERCLE_EXCLUDED_USERS`): service accounts that never exist in Cercle —
  never created or auto-joined, their ingest events are accepted and dropped, session login →
  `403 excluded_user`; an existing user newly excluded is removed from all circles at startup.
- **Sharing flags** — per *(user, circle)* membership, all default **true**:
  - `share_live` — what I'm playing right now (and paused/position)
  - `share_history` — my individual plays / feed items
  - `share_stats` — my stats pages, and inclusion in circle-wide stats
  - `share_discoveries` — "discovered a new artist/album" feed items
- **Global privacy controls** — per user:
  - `paused_until` (timestamp|null): while in the future, *nothing new* is shared (live hidden,
    plays recorded but flagged `private=1` forever — they never become visible; stats include them
    only for the user themself).
  - `history_delay_minutes` (int, default 0): others see a play only once it is older than this.
    Live is unaffected (turn `share_live` off for that).
  - **hidden items**: list of `{kind: artist|album|track, id, label}`. Plays matching a hidden item
    are recorded `private=1` (same semantics as paused). Hiding applies to *future and past* plays
    at query time as well (filter by `artist_id/album_id/track_id` when computing visibility).
- **Visibility rule** (the only one; implement once in the hub, e.g. `visibility.go`):
  viewer **V** may see item **X** of subject **S** iff `V == S`, **or** there exists a circle
  where both V and S are members and S's membership has the flag for X's category, **and**
  X is not private (`private=0`, not hidden), **and** (for history) `played_at <= now - S.delay`,
  **and** (for live) S is not paused.
  Leaving a circle immediately removes visibility through that circle, including the past.

## 2. Ingest API (plugin → hub)

`POST /ingest/v1/events` — header `Authorization: Bearer <INGEST_SECRET>` (constant-time
compare; 401 otherwise). Body: one event object, or `{"events":[…]}` (≤ 100). Response
`202 {"accepted": n}`. Unknown fields ignored; unknown `type` → counted but ignored.

```json
{
  "type": "now_playing" | "scrobble" | "playback_state",
  "user": { "username": "alice", "name": "Alice", "id": "<navidrome user id>" },
  "track": {
    "id": "<navidrome mediafile id>", "title": "…", "artist": "…", "artist_id": "…",
    "album": "…", "album_id": "…", "album_artist": "…", "duration_ms": 245000,
    "track_number": 3, "year": 1973, "genre": "Rock",
    "mbz_track_id": "…", "mbz_album_id": "…", "mbz_artist_id": "…"
  },
  "player": { "id": "…", "name": "Feishin" },
  "timestamp": "2026-01-01T12:00:00Z",          // event time (scrobble: play start time)
  "position_ms": 12000,                          // optional (now_playing / playback_state)
  "state": "playing" | "paused" | "stopped"      // optional, playback_state only
}
```

Plugin mapping (Navidrome 0.64 scrobbler capability): `NowPlaying` → `now_playing`
(`position_ms = position*1000`, `timestamp = now`); `Scrobble` → `scrobble` (`timestamp` from
Navidrome, may be the stop time for clients using `reportPlayback`); `PlaybackReport` →
`playback_state` with `state` ∈ `starting|playing|paused|stopped|expired` (hub treats
`starting`≈`playing`, `expired`≈`stopped`), plus `player`. Navidrome gives the plugin only the
**username** and no album/artist ids: the plugin enriches `track` via the `subsonicapi` host call
`getSong` (cached per track id) to fill `album_id`, `artist_id`, `year`, `genre`. `user.id`/`name`
may be absent — hub must not require them. Hub must accept events whose `track` lacks ids
(store empty string; such plays never produce discoveries).

Semantics:
- `scrobble` → insert into `plays` (dedupe: same user+track+timestamp within ±5 s is ignored).
  Then evaluate discovery: if this is the user's **first ever** play of this `artist_id` →
  feed item `discovery_artist`; else if first play of `album_id` → `discovery_album`.
- `now_playing` → set the user's live state `{track, started_at=timestamp, position_ms,
  state:"playing", updated_at=now}` in memory; expires after `duration_ms + 60 s` without update.
- `playback_state` → update live state (state/position). `stopped` clears it.
- Every live change is broadcast over WS (§4) to viewers allowed to see it.

## 3. Client authentication (player → hub)

`POST /api/v1/auth/session` body `{"username":"alice","token":"<subsonic t>","salt":"<subsonic s>"}`
**or** `{"username":"alice","password":"…"}` **or** `{"username":"alice","api_key":"…"}`.
Hub validates by calling `${NAVIDROME_URL}/rest/ping.view?u=&t=&s=&v=1.16.1&c=cercle-hub&f=json`
(or `p=` / `apiKey=`) and requires `subsonic-response.status == "ok"`. It also reads
`getUser.view?username=<u>` with the same creds to learn `adminRole` → stored as `is_admin`.
On success: `200 {"token":"<opaque 32-byte base64url>","user":{…User}}`; store only
`sha256(token)` with `expires_at = now + 30 d` (sliding: refreshed on use, at most once a day).
Rate-limit failures per IP (10/min). Navidrome down → 502.

All `/api/v1/*` (except `/auth/session`, `/health`) require `Authorization: Bearer <token>`;
WS takes `?token=<token>` (browsers can't set WS headers). 401 → client re-runs session login
with the credentials it already holds.

Optional deployment mode `TRUSTED_HEADER` (+ `TRUSTED_PROXIES` CIDRs): if a request comes from a
trusted proxy with that header, the header value is the username (no session needed).

## 4. REST API v1 (JSON, `snake_case`, times RFC 3339 UTC, ids are strings)

Errors: `{"error":{"code":"not_found","message":"…"}}` with proper HTTP status.
Pagination: `?limit=` (default 50, max 200) & `?before=<cursor>` → response `{"items":[…],"next":"<cursor>|null"}`.

### Types
```ts
User        { username, display_name, is_admin, created_at, last_seen_at }
Me          User & { settings: Settings }
Settings    { paused_until: string|null, history_delay_minutes: number, hidden: Hidden[] }
Hidden      { kind: "artist"|"album"|"track", id: string, label: string }
Circle      { id, name, description, kind: "everyone"|"custom", member_count, my_role: "owner"|"admin"|"member"|null, created_at }
Member      { user: User, role, joined_at, sharing: Sharing }   // sharing visible to circle admins and self only; else omitted
Sharing     { share_live, share_history, share_stats, share_discoveries: boolean }
Invite      { code, circle_id, created_by, expires_at, max_uses, uses, url_path: "/cercle/join/<code>" }
Track       { id, title, artist, artist_id, album, album_id, duration_ms }
Live        { user: User, track: Track, state: "playing"|"paused", position_ms, started_at, updated_at, player_name }
FeedItem    { id, kind: "listened"|"discovery_artist"|"discovery_album", user: User,
              track?: Track,                      // listened: last track of the group
              album?: {id,name,artist}, artist?: {id,name},
              play_count: number,                 // plays grouped into this item
              started_at, ended_at,
              reactions: {emoji:string,count:number,mine:boolean}[], comment_count }
Comment     { id, user: User, body, created_at, edited_at|null }
StatEntry   { id, name, artist?, album_id?, plays, ms }
Stats       { range, totals:{plays,ms,active_days,distinct_artists,distinct_albums},
              top_artists:StatEntry[], top_albums:StatEntry[], top_tracks:StatEntry[],
              by_hour:number[24], by_weekday:number[7] /* 0=Mon */, by_day:{date,plays}[] }
Room        { id, circle_id, name, host: User, listeners: User[], state: RoomState, created_at }
RoomState   { track_id|null, position_ms, playing, queue: string[] /* track ids */, updated_at, seq }
RoomRequest { id, user: User, track: Track, created_at }
```

### Endpoints
```
GET    /health                                   -> {"ok":true,"version":"…"}   (no auth)
POST   /api/v1/auth/session                      -> {token,user}
DELETE /api/v1/auth/session                      -> 204 (logout this token)

GET    /api/v1/me                                -> Me
PATCH  /api/v1/me                                -> Me      body {display_name?}
PUT    /api/v1/me/settings                       -> Settings body {paused_until?, history_delay_minutes?}
POST   /api/v1/me/pause                          -> Settings body {minutes:number}   (0 = resume)
POST   /api/v1/me/hidden                         -> Settings body Hidden
DELETE /api/v1/me/hidden/{kind}/{id}             -> Settings

GET    /api/v1/users                             -> {items: User[]}   users sharing ≥1 circle with me
GET    /api/v1/users/{username}                  -> User & {common_circles: Circle[]}   (404 if no common circle and not me)

GET    /api/v1/circles                           -> {items: Circle[]}   my circles (+ `everyone` always listed; my_role null if left)
POST   /api/v1/circles                           -> Circle  body {name, description?}   (creator = owner)
GET    /api/v1/circles/{id}                      -> Circle & {members: Member[]}   (members only)
PATCH  /api/v1/circles/{id}                      -> Circle  body {name?, description?}  (owner/admin)
DELETE /api/v1/circles/{id}                      -> 204     (owner; not `everyone`)
POST   /api/v1/circles/{id}/join                 -> Circle  body {code?}  (code required except `everyone`)
POST   /api/v1/circles/{id}/leave                -> 204     (owner must transfer first unless last member → circle deleted)
PUT    /api/v1/circles/{id}/sharing              -> Sharing body Sharing (partial ok) — my flags in this circle
PUT    /api/v1/circles/{id}/members/{username}   -> Member  body {role}  (owner: admin/member; "owner" = transfer)
DELETE /api/v1/circles/{id}/members/{username}   -> 204     (owner/admin remove; not the owner)
POST   /api/v1/circles/{id}/invites              -> Invite  body {expires_in_hours?=168, max_uses?=0 (unlimited)}
GET    /api/v1/circles/{id}/invites              -> {items: Invite[]}  (owner/admin)
DELETE /api/v1/circles/{id}/invites/{code}       -> 204
GET    /api/v1/invites/{code}                    -> {circle:{id,name,member_count}, created_by: User, valid: boolean}   (any logged-in user, for the join screen)
POST   /api/v1/invites/{code}/accept             -> Circle

GET    /api/v1/live                              -> {items: Live[]}   everyone I may see, incl. me
GET    /api/v1/feed?circle=&user=&limit=&before= -> {items: FeedItem[], next}
GET    /api/v1/users/{username}/plays?limit=&before= -> {items: {id, track: Track, played_at, player_name}[], next}

GET    /api/v1/feed/{item_id}/comments           -> {items: Comment[]}
POST   /api/v1/feed/{item_id}/comments           -> Comment body {body} (1..2000 chars)
PATCH  /api/v1/comments/{id}                     -> Comment body {body}   (author)
DELETE /api/v1/comments/{id}                     -> 204                   (author, or admin of a circle that makes the item visible)
PUT    /api/v1/feed/{item_id}/reactions/{emoji}  -> FeedItem   (idempotent; emoji = one grapheme, url-encoded)
DELETE /api/v1/feed/{item_id}/reactions/{emoji}  -> FeedItem

GET    /api/v1/stats/me?range=7d|30d|year|all|YYYY        -> Stats
GET    /api/v1/stats/users/{username}?range=…             -> Stats   (needs share_stats via a common circle)
GET    /api/v1/stats/circles/{id}?range=…                 -> Stats & {members: {user:User, plays, ms}[]}   (members with share_stats only)
GET    /api/v1/stats/compare/{username}?range=…           -> {common_artists: StatEntry[], common_albums: StatEntry[], score: number /*0..1 jaccard on artists*/}
GET    /api/v1/wrapped/{year}?user=<username>             -> Stats & {first_play, top_month, longest_day:{date,ms}, new_artists:number}

GET    /api/v1/circles/{id}/rooms                -> {items: Room[]}
POST   /api/v1/circles/{id}/rooms                -> Room body {name}   (creator = host; any member)
GET    /api/v1/rooms/{id}                        -> Room & {requests: RoomRequest[]}
POST   /api/v1/rooms/{id}/join                   -> Room
POST   /api/v1/rooms/{id}/leave                  -> 204   (host leaving → host passes to longest listener; empty → closed)
PUT    /api/v1/rooms/{id}/state                  -> RoomState  body RoomState minus seq/updated_at (host only)
PUT    /api/v1/rooms/{id}/host                   -> Room  body {username}   (host only)
POST   /api/v1/rooms/{id}/requests               -> RoomRequest body {track: Track}  (any listener)
DELETE /api/v1/rooms/{id}/requests/{rid}         -> 204   (host, or requester)
DELETE /api/v1/rooms/{id}                        -> 204   (host, or circle owner/admin)
```

**Feed grouping:** consecutive `scrobble`s of one user from the same album with < 30 min gaps form
one `listened` item (stable id = first play id). Discoveries are separate items. Store groups in a
`feed_items` table updated at ingest (not computed per request).

**Stats ranges:** `7d`, `30d` rolling; `year` = current calendar year; `all`; `YYYY` = that year.
Time zone for hour/weekday/day buckets: hub config `TZ` (default UTC). `ms` sums `duration_ms`.

## 5. WebSocket `GET /api/v1/ws?token=…`

Server → client JSON messages `{"type": "...", "data": {...}}`:
```
hello            {user: Me}
live             Live                     (start/update)
live_end         {username}
feed_item        FeedItem                 (new or updated group)
reaction         {item_id, reactions}
comment          {item_id, comment: Comment}
comment_deleted  {item_id, comment_id}
room_state       {room_id, state: RoomState}
room_update      Room                     (join/leave/host change)
room_request     {room_id, request: RoomRequest}
room_closed      {room_id}
```
Client → server:
```
ping             {}                        -> pong {}
subscribe_room   {room_id}                 (must be a listener)
unsubscribe_room {room_id}
```
Server pings every 30 s; drop after 75 s silence. Every message is filtered per recipient through
the visibility rule (live/feed/reaction/comment) or room membership (room_*).

**Listen-together sync (client rule):** listeners apply `room_state` as: load `track_id` if
different; target position = `position_ms + (playing ? now - updated_at : 0)`; seek if drift
> 2000 ms; match play/pause. The host's client PUTs state on every track change, play/pause,
seek, queue change, and every 15 s while playing. Ignore states with `seq` ≤ last applied.

## 5b. Subsonic compatibility endpoint (privacy for third-party apps)

Navidrome's own `getNowPlaying` returns every user's session to every user, with no opt-out
(`EnableNowPlaying=false` does not disable it in 0.64). Deployments route
`/rest/getNowPlaying(.view)` to the hub instead, which answers it **filtered by the visibility
rule** for the calling user:

`GET|POST {base}/rest/getNowPlaying[.view]?u=&t=&s=|p=|apiKey=&v=&c=&f=json|xml`
- Validate credentials against Navidrome `ping` like §3; cache a success for 10 min keyed by
  `sha256(u|t|s|p|apiKey)`; failures → Subsonic error 40 (HTTP 200).
- Response: standard Subsonic envelope (`status:"ok"`, `version:"1.16.1"`, `type:"cercle"`,
  `serverVersion`, `openSubsonic:true`) with `nowPlaying.entry[]`: `id` (track id), `title`,
  `artist`, `album`, `albumId`, `artistId`, `duration` (s), `coverArt` (= track id),
  `username`, `minutesAgo`, `playerId` (int, 1..n), `playerName`, plus OpenSubsonic
  `state` / `positionMs`. JSON when `f=json`, else XML (`xmlns="http://subsonic.org/restapi"`).
  `f=jsonp` → treat as json.
- Mounted at `{CERCLE_BASE_PATH}/rest/...` **and** at root `/rest/...` (the proxy may route the
  original path unchanged).

## 6. Hub configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `CERCLE_LISTEN` | `:8080` | listen address |
| `CERCLE_DB` | `/data/cercle.db` | SQLite path (WAL mode, `busy_timeout=5000`, foreign keys on) |
| `NAVIDROME_URL` | — (required) | base URL the hub uses to validate users |
| `INGEST_SECRET` | — (required, ≥ 32 chars) | shared with the plugin |
| `CERCLE_BASE_PATH` | `` | serve everything under this prefix, e.g. `/hub` |
| `CERCLE_CORS_ORIGINS` | `` | comma list of allowed browser origins (desktop app origins too) |
| `TZ` | `UTC` | stats bucketing |
| `TRUSTED_HEADER`, `TRUSTED_PROXIES` | `` | optional reverse-proxy auth |
| `CERCLE_LOG_LEVEL` | `info` | |
| `NAVIDROME_ADMIN_USER`, `NAVIDROME_ADMIN_PASSWORD` | `` | optional admin creds enabling user sync (both or neither; token auth only, never logged) |
| `CERCLE_USER_SYNC_INTERVAL` | `15m` | user sync period (≥ 1m), plus once at startup |
| `CERCLE_EXCLUDED_USERS` | `` | comma list of service accounts, case-insensitive (e.g. `admin`) |
| `CERCLE_EVERYONE_NAME`, `CERCLE_EVERYONE_DESCRIPTION` | `` | applied to the `everyone` circle at startup only when set (else an API rename is kept) |

Schema migrations: embedded numbered SQL files, applied at start in a transaction, tracked in
`schema_migrations`. Graceful shutdown on SIGTERM.

## 7. Plugin configuration (Navidrome plugin settings UI)

`hub_url` (e.g. `http://cercle-hub:8080/hub`), `ingest_secret`. Manifest HTTP permission `"*"`
(the hub is usually on a private network; Navidrome ≥ 0.64 blocks private IPs for host names).
User-scoped: enable for **all users** (visible-by-default concept); Navidrome's per-user scrobble
filter keeps songs out before they reach the plugin. On hub errors: `Scrobble` returns
`scrobbler(retry_later[:N])` for network errors / 408 / 429 / 5xx (Navidrome buffers and retries,
so plays survive a hub outage) and `unrecoverable` for other 4xx (e.g. wrong secret → plays are
dropped until fixed). `NowPlaying`/`PlaybackReport` are tried once (5 s timeout) and dropped.
Never blocks playback. The plugin also needs the `cache` permission (Navidrome reloads the wasm
module per call, so metadata is cached in Navidrome's cache service, 12 h).

## 8. Player module (Feishin fork)

All new code under `src/renderer/features/cercle/`; upstream files touched only to register
routes, the sidebar entry, i18n keys and settings. Hub URL: setting `cercle.hubUrl`; default
from `window.CERCLE_HUB_URL` (injected by the web image's `settings.js`), else
`<server origin>/hub`. Pages:
- **Cercle** (feed): circle filter chips, live strip on top ("now playing" cards with progress),
  feed items with cover art (from the Navidrome server via Feishin's existing image helpers),
  click → play the album/track in my player, reactions (quick emoji set + picker), comments thread.
- **Live**: grid of everyone visible playing now; "listen along" = join their room if any, else
  play the same track at their position.
- **Stats**: me / a user / a circle; range switch; top artists/albums/tracks, hour clock,
  weekday bars, daily chart; compare with a user; Wrapped page per year.
- **Circles**: list, create, detail (members, roles, invites with copy link, my sharing toggles,
  leave), join screen for `/cercle/join/:code`.
- **Rooms**: per circle list/create; room page = host's now playing, listeners, requests
  (add from search), host controls; listeners' players follow (§5).
- **Privacy** (settings page): pause sharing (30 min / 2 h / 8 h / until tomorrow / resume),
  history delay, hidden items list (also "Hide from Cercle" in track/album/artist context menus).
- Auth: on first use, derive Subsonic credentials of the current server and POST
  `/auth/session`; keep the hub token in Feishin's persisted store; re-login on 401.
- Optional client reporting: none in v1 — live comes from the plugin.

## Decisions (hub)

Gaps the contract left open, as implemented in `hub/` (machine-readable: `docs/openapi.yaml`).

**Ingest**
- Skipped and *not* counted in `accepted`: events without `user.username`; `scrobble`/`now_playing`
  without `track.id`; `playback_state` with an unknown `state`. Deduplicated scrobbles *are* counted.
- `playback_state` with a track and no current live state starts one; a different `track.id`
  replaces it. Future-dated timestamps are stored as given (visible once that time has passed).
- Feed grouping key: `album_id`, else lowercased `album` + `album_artist`; plays with neither never
  group. The gap is measured from the group's `ended_at` (= last `played_at + duration_ms`) to the
  new play's timestamp. Private plays (paused/hidden) only group with private plays. An
  out-of-order (older) scrobble starts a new group.
- Feed item ids: `listened` = first play id; discoveries = `<play id>-artist` / `<play id>-album`.
  Discovery items also carry `track` (the discovering play); `listened` carries `album` and `artist`.

**Visibility details**
- A `listened` group is visible to others once its *last* play is older than the delay, and is
  hidden entirely if any of its plays matches a hidden item. Discoveries need only
  `share_discoveries` (not `share_history`) and obey private/hidden/delay too.
- Hidden items also hide the matching *live* track. Unhiding restores past plays except those
  recorded `private=1` while hidden.
- Stats of someone else (user stats, compare, `wrapped?user=`) use only their public plays
  (not private, not hidden, past the delay). Circle stats use the public plays of members with
  `share_stats` *in that circle* — the viewer included — so every member sees the same numbers.
- Strangers (no common circle) get 404; a known user who doesn't share the category gets 403
  (plays, stats, compare, wrapped). Non-members get 404 for a custom circle, 403 for `everyone`.
- `GET /users` excludes me.

**Circles**
- Auto-join of `everyone` happens only when the user record is created; logging in again does not
  re-add someone who left. Re-joining any circle resets the sharing flags to `true`.
- Join/accept when already a member returns the circle without consuming an invite use. Leaving a
  circle you're not in → 204.
- Only the owner changes roles; `"owner"` transfers and the previous owner becomes `admin`.
  Admins may remove members (not admins); the owner may remove anyone but themself; removing
  yourself → 400 (use `/leave`). Leaving or being removed also removes you from the circle's rooms.
- `everyone`: no roles, invites → 400, removing members → 403, rename only by server admins.
- Invites: `expires_in_hours` 1..8760, `max_uses` ≥ 0. Expired / used up → **410**
  `invite_invalid`; code missing on join → 400 `code_required`; owner leaving with others → 409
  `owner_must_transfer`; login rate limit → 429 `rate_limited`.
- Create endpoints (circles, invites, comments, rooms, room requests) return **201**.

**Settings / social**
- `history_delay_minutes` 0..43200; `pause.minutes` 0..525600; a `paused_until` in the past is
  stored and returned as `null`. Hidden `id` ≤ 256 bytes, `label` ≤ 512 chars.
- Comments: body trimmed, 1..2000 chars; edit by author only; delete by author or an owner/admin
  of a circle through which the item is visible (the item's owner has no extra right).
- Reaction emoji: 1..32 bytes of UTF-8, ≤ 10 code points, no whitespace, not plain ASCII
  (approximation of "one grapheme").

**Stats**
- Top lists: 10 entries; compare lists: 25, `plays`/`ms` summed over both users. Entries without
  a Navidrome id use `id = "name:<lowercased name>"`. Default `range` = `30d`. `by_day` lists every
  day of the range up to today (for `all`: only days with plays).
- Wrapped: `first_play = {track, played_at}|null`, `top_month = "YYYY-MM"|null`,
  `longest_day = {date, ms}|null`, `new_artists` = artists whose first ever (visible) play falls in
  that year.

**WebSocket**
- Extra server messages: `pong {}`, `error {code, message}`, `room_request_deleted {room_id,
  request_id}`. `subscribe_room` is answered with the current `room_state`.
- `room_update` / `room_closed` go to every member of the room's circle (keeps room lists fresh);
  `room_state` / `room_request*` only to subscribed listeners.
- `live_end` is sent to users sharing any circle with the subject who may not (or no longer) see
  the live state — e.g. after pause, `share_live` off, hiding, leaving a circle.
- Feed items still inside the subject's delay are not pushed to others (they appear over REST).
- Slow consumer (64 queued messages) → the connection is closed (1008); the client reconnects and
  refetches. Origin must be in `CERCLE_CORS_ORIGINS` or the same host; no Origin (native apps) is
  allowed. `Authorization: Bearer` is accepted as well as `?token=`.

**Auth / Subsonic shim / HTTP**
- `api_key` login resolves the key's owner with OpenSubsonic `tokenInfo` and requires it to match
  `username`. A failing `getUser` (non-admin) means `is_admin=false`. Navidrome 5xx, network errors,
  non-Subsonic bodies and Subsonic error codes other than 10/40–44/50 → 502.
- `X-Forwarded-For` is only honoured from `TRUSTED_PROXIES` (right-most untrusted address);
  `TRUSTED_HEADER` requires `TRUSTED_PROXIES`.
- Subsonic shim: missing credentials → error 10; Navidrome unavailable → error 0 (HTTP 200);
  failures share the login rate limiter (blocked → error 40); first sight creates the user.
  Entries also carry `parent`, `isDir:false`, `type:"music"`.
- With a base path set, `/health` and `/rest/getNowPlaying*` are also served at the root; every
  other root path is 404. CORS accepts `*`; preflights are answered 204 on any path.
- Rooms: the creator auto-joins; joining needs circle membership; host hand-over goes to the
  earliest `joined_at`; ≤ 200 pending requests, queue ≤ 1000; empty `track_id` → `null`; closed
  rooms are deleted.

**User sync / exclusions**
- Disabling is a `users.disabled_at` timestamp; every visibility/list/count read goes through an
  `active_memberships` view, so memberships, plays and comments are kept untouched. Any proof of
  existence (login, plugin event, sync) re-enables. A sync that fails or returns an empty list
  changes nothing.
- Subsonic `getUsers` has no display-name field, so sync only sets `display_name` if a server
  sends `name`; otherwise names come from the plugin's `user.name` or the user's own edit.
- An excluded user with a valid session or trusted-header identity gets `403 excluded_user` on
  every authenticated endpoint; the Subsonic shim answers them with an empty `nowPlaying`.
- Purging a newly excluded owner hands the circle to the earliest-joined admin, else member; a
  circle they were alone in is deleted. Their sessions are revoked and live state dropped.
