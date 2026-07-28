# SpotiFLAC Web (self-hosted, Docker)

A headless, browser-based rebuild of SpotiFLAC for running on a home server
(ZimaOS, CasaOS, any Docker host). The Wails desktop GUI is replaced by an HTTP
server + web UI; the entire download engine (`backend/`) is reused unchanged and
still uses the same **community servers** as upstream.

> Personal / private use only. Downloading tracks you don't have rights to may
> violate the source services' Terms of Service and your local law.

---

## Quick start

```bash
docker compose up -d --build
# then open http://<host>:8080
```

Or without compose:

```bash
docker build -t spotiflac-web .
docker run -d --name spotiflac -p 8080:8080 \
  -v /path/to/music:/downloads \
  -v /path/to/appdata:/config \
  spotiflac-web
```

The image is a small pure-Go static binary (the web UI is embedded) plus
`ffmpeg`. No cgo, no C libraries — taglib runs as WASM.

## ZimaOS

ZimaOS runs Docker/compose apps. Use the compose file (adjust the host paths on
the left of each volume to your shares), or import it via the ZimaOS custom-app
UI. Then browse to `http://<zimaos-ip>:8080`.

```yaml
volumes:
  - /DATA/Media/Music:/downloads      # where FLACs land
  - /DATA/AppData/spotiflac:/config   # sessions, history, settings (persist!)
```

Keep `/config` persistent — it stores the community verification session so you
don't have to re-verify on every restart.

## Configuration (env vars)

| Var | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `DOWNLOAD_DIR` | `/downloads` | Output directory (mount a volume) |
| `CONFIG_DIR` | `/config` | App data: session, history, settings |
| `PUBLIC_URL` | *(auto)* | Externally-visible URL; set only if behind a reverse proxy so the Cloudflare verification callback resolves |

No authentication is built in — run it on a trusted LAN, or put it behind your
own reverse-proxy auth / VPN if you expose it.

## How the Cloudflare verification works

The community servers occasionally require a Cloudflare check. Its callback is
strictly validated to be a `127.0.0.1/session-grant` loopback address, so a
remote/headless deployment can't have it redirect straight back to the app.
Instead the web UI walks you through a quick capture step:

1. When a download needs verification, the UI pops a **"Verification required"**
   modal (pushed over Server-Sent Events).
2. Click **Open verification page** — the real Cloudflare challenge opens in a new
   tab and you complete it.
3. Your browser then tries to redirect to `http://127.0.0.1:…/session-grant?…&grant=…`
   and shows a **"can't connect"** page — that's expected. **Copy that page's full
   address** and paste it into the modal, then **Submit**.
4. The server extracts the grant and relays it to the engine's loopback listener
   *inside the container*, which exchanges it for an HMAC session (cached in
   `/config`). The download continues, and subsequent downloads reuse the session
   until it expires — so you only do this occasionally.

> Tip: if you run the container locally and browse it at `http://localhost:PORT`,
> the redirect can reach the app directly; the paste step is mainly for remote
> (LAN/ZimaOS) access where `127.0.0.1` in your browser isn't the container.

## What the UI covers

- Paste a Spotify **track / album / playlist** URL, or search.
- Pick service (Tidal / Qobuz / Amazon), format, filename format.
- Download single tracks or select-and-download whole albums/playlists.
- Live **Queue** with progress, speed, rate-limit/cooldown status.
- **Files** browser over `/downloads` with per-file download-to-browser.
- **History**, **Convert/Resample** tools, **Settings**.

Every backend method is exposed over a generic RPC endpoint
(`POST /api/rpc/{Method}`), so any desktop feature not yet surfaced in the UI can
be added client-side without server changes.

## Architecture

```
web/ (embedded SPA) ──HTTP/SSE──► main.go ── rpc.go (reflection RPC over *App)
                                           ├─ events.go  (SSE, replaces Wails events)
                                           ├─ verify.go  (Cloudflare callback proxy)
                                           └─ app.go + backend/  (unchanged engine)
```

## Limitations / notes

- **Single active download queue** (same as upstream) — fine for one user.
- Depends on the upstream community servers; if they're down or "on break"
  (they run scheduled cooldowns) downloads pause until they return.
- Native file/folder pickers are removed; use the Files browser instead.
- `/api/file` only serves files inside `/downloads`.
