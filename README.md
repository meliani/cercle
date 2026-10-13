# Cercle

A self-hosted social layer for [Navidrome](https://www.navidrome.org/): see what the people in
your circles are listening to, share stats and a yearly Wrapped, react and comment, and listen
together in synchronized rooms. Everything stays on your server, and every bit of sharing can be
switched off per circle.

| Part | What it does |
|---|---|
| [`hub/`](hub/) | Go service + SQLite. Circles, privacy, feed, stats, rooms, WebSocket live updates, and a privacy-filtered Subsonic `getNowPlaying`. |
| [`plugin/`](plugin/) | Navidrome plugin (`.ndp`). Forwards now-playing, playback state and scrobbles to the hub. |
| [meliani/feishin `cercle` branch](https://github.com/meliani/feishin/tree/cercle) | The [Feishin](https://github.com/jeffvli/feishin) player with a **Cercle** section (feed, live, stats, circles, rooms, privacy). |

How the pieces talk to each other, every endpoint and the privacy model:
[docs/CONTRACT.md](docs/CONTRACT.md) (OpenAPI: [docs/openapi.yaml](docs/openapi.yaml)).

## Privacy model in one paragraph

Users are in circles; the built-in `everyone` circle contains every user from the start and can
be left and rejoined. For each circle a user decides whether it sees their live listening,
history, stats and discoveries. On top of that: pause sharing for a while, delay history, and
hide artists, albums or tracks. Navidrome's own per-user scrobble filter also applies. Leaving a
circle hides your past from it immediately.

## Quick start

See [deploy/docker-compose.example.yml](deploy/docker-compose.example.yml): run the hub next to
Navidrome, install `cercle.ndp` into Navidrome's plugins folder, enable it for all users with the
hub URL and the shared ingest secret, and route `/hub` (and optionally `/rest/getNowPlaying`) to
the hub. Then open the Cercle section in the Feishin web image `ghcr.io/meliani/feishin-cercle`.

## License

MIT. The Feishin fork stays GPL-3.0 like upstream.
