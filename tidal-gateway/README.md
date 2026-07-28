# Self-hosted Tidal gateway for SpotiFLAC

Runs alongside SpotiFLAC on your ZimaOS box. It logs into Tidal **once** with your
own subscription and exposes the endpoint SpotiFLAC's *Custom Tidal API URL*
expects — so downloads use **your** account instead of the shared community
servers (no Cloudflare check, no rate limits, no scheduled breaks).

> Personal use only. Downloading via this almost certainly breaks Tidal's Terms
> of Service even with a paid account. That's on you.

## The contract it implements

SpotiFLAC calls:
```
GET /track/?id=<trackId>&quality=<LOSSLESS|HI_RES_LOSSLESS|HIGH|LOW>
```
and expects:
```json
{ "data": { "trackId": 123, "assetPresentation": "FULL",
            "manifestMimeType": "application/dash+xml",
            "manifest": "<base64 playback manifest>" } }
```
That `manifest` is exactly what Tidal returns from
`/v1/tracks/{id}/playbackinfopostpaywall`, which needs a logged-in subscriber
token. The gateway does the OAuth device login, holds the token, and forwards
that manifest.

It also serves a search endpoint used to recover from bad Spotify→Tidal matches:
```
GET /search/?q=<artist title>&isrc=<ISRC>&limit=10
-> { "items": [ { "id", "title", "artist", "isrc", "duration",
                  "audioQuality", "streamReady" }, ... ] }
```
song.link frequently resolves a Spotify track to a Tidal ID this account/region
cannot stream (Tidal answers `playbackinfopostpaywall` with 404/401, which the
gateway reports as 502). When that happens SpotiFLAC searches here and retries
with an ID that is actually streamable — ISRC match first, then artist+title.

## Run it (Docker Compose)

Add a second service next to SpotiFLAC:

```yaml
services:
  spotiflac:
    image: ghcr.io/akvaithi/spotiflac:latest
    ports: ["8080:8080"]
    volumes:
      - /DATA/Media/Music:/downloads
      - /DATA/AppData/spotiflac:/config
    restart: unless-stopped

  tidal-gateway:
    build: ./tidal-gateway          # or your own prebuilt image
    ports: ["8081:8081"]
    volumes:
      - /DATA/AppData/tidal-gateway:/config   # stores the Tidal session token
    restart: unless-stopped
```

```bash
docker compose up -d --build
```

## One-time login

The gateway needs you to log in once:

```bash
docker compose logs -f tidal-gateway
```

You'll see:
```
==================  TIDAL LOGIN REQUIRED  ==================
  Open:  https://link.tidal.com/XXXXX
```
Open that link, sign in with your Tidal subscription, approve. The token is saved
to `/config` and reused/refreshed automatically — you won't need to do this again
unless it fully expires.

## Point SpotiFLAC at it

In SpotiFLAC → **Settings → Your own gateway → Custom Tidal API URL**, enter:
```
http://<your-zimaos-ip>:8081
```
Click **Test** — it should say “online (returns a FULL manifest)”. Save. From now
on Tidal downloads go through your account.

## Notes / troubleshooting

- **Hi-Res**: your account must include HiFi Plus for `HI_RES_LOSSLESS` to return
  a hi-res manifest; otherwise it falls back to lossless.
- **401 / expired**: delete `/config/tidal-session.json` and restart to re-login.
- This is a minimal scaffold (~120 lines). If Tidal changes their API or the
  `tidalapi` library version drifts, the `playbackinfopostpaywall` call in
  `gateway.py` is the one place to adjust.
- A Qobuz gateway would follow the same pattern against Qobuz's
  `track/getFileUrl` signed endpoint, returning `{success:true,data:{url}}`.
