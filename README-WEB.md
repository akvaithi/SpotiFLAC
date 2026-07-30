# SpotiFLAC Web (self-hosted, Docker)

A headless, browser-based rebuild of SpotiFLAC for running on a home server
(ZimaOS, CasaOS, any Docker host). The Wails desktop GUI is replaced by an HTTP
server + web UI. Its only download source is the **flacit-gateway** sidecar,
which drives Telegram's `@deezload2bot` over MTProto (ported from
[FlacIt](https://github.com/BunnY-exe/FlacIt)) to fetch lossless FLAC sourced
from Deezer.

> Personal / private use only. Downloading tracks you don't have rights to may
> violate the source services' Terms of Service and your local law.

---

## Quick start

```bash
docker compose up -d --build
# then open http://<host>:8080
```

This needs **both** services in `docker-compose.yml` — `spotiflac` and
`flacit-gateway` — and the gateway needs an authenticated Telegram session
before downloads will work. See [SETUP.md](SETUP.md) for the full walkthrough,
including how to bootstrap that session.

The `spotiflac` image is a small pure-Go static binary (the web UI is
embedded) plus `ffmpeg`. No cgo, no C libraries — taglib runs as WASM.

## ZimaOS

ZimaOS runs Docker/compose apps. Use the compose file (adjust the host paths on
the left of each volume to your shares), or import it via the ZimaOS custom-app
UI. Then browse to `http://<zimaos-ip>:8080`.

```yaml
volumes:
  - /DATA/Media/Music:/downloads          # where FLACs land
  - /DATA/AppData/spotiflac:/config       # history, settings, library index (persist!)
  - /DATA/AppData/flacit-gateway:/config  # (flacit-gateway service) Telegram session (persist!)
```

## Configuration (env vars)

| Var | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `DOWNLOAD_DIR` | `/downloads` | Output directory (mount a volume) |
| `CONFIG_DIR` | `/config` | App data: history, settings, library index |
| `FLACIT_GATEWAY_URL` | `http://172.17.0.1:8082` | flacit-gateway address — must be docker0's host gateway + published port, never a container IP (those shift on recreate) |

No authentication is built in — run it on a trusted LAN, or put it behind your
own reverse-proxy auth / VPN if you expose it.

## What the UI covers

- Paste a Spotify **track / album / playlist** URL, or search.
- Pick a filename format; download single tracks or select-and-download whole
  albums/playlists.
- Live **Queue** with progress and speed, fed by the gateway's own job status.
- **Files** browser over `/downloads` with per-file download-to-browser.
- **History**, **Convert/Resample** tools, **Settings** (including the
  flacit-gateway URL and its live login status).

Every backend method is exposed over a generic RPC endpoint
(`POST /api/rpc/{Method}`), so any feature not yet surfaced in the UI can be
added client-side without server changes.

## Architecture

```
web/ (embedded SPA) ──HTTP/SSE──► main.go ── rpc.go (reflection RPC over *App)
                                           ├─ events.go  (SSE, replaces Wails events)
                                           ├─ app.go + backend/  (Spotify catalog,
                                           │   tagging, queue, library, enrichment)
                                           └─ backend/flacit.go ──HTTP──► flacit-gateway
                                                                          (Telethon/Flask,
                                                                           separate container)
```

## Limitations / notes

- **Single active download queue** — fine for one user; the gateway also
  processes fetches one at a time for the same reason (the bot chat is one
  stateful conversation).
- **16-bit/44.1kHz FLAC only** — this is what Deezer serves via the bot;
  there's no hi-res tier and no fallback source if a track isn't on Deezer.
- Depends on the flacit-gateway's Telegram session staying authenticated and
  `@deezload2bot`'s channel membership staying valid.
- Native file/folder pickers are removed; use the Files browser instead.
- `/api/file` only serves files inside `/downloads`.
