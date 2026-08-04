# Self-hosted Telegram gateway for SpotiFLAC

Runs alongside SpotiFLAC on your ZimaOS box. It's SpotiFLAC's **only** download
source: a persistent Telethon session drives `@deezload2bot` over MTProto and
downloads the FLAC it posts with 16 parallel exported senders against Telegram's
own DC — the technique from
[FlacIt](https://github.com/BunnY-exe/FlacIt)'s `ultra_parallel_download`, at
3.5–4.6 MB/s instead of the ~0.3 MB/s a single MTProto stream is throttled to.

> Personal use only. `@deezload2bot` sources from Deezer; this is ToS-gray
> personal tooling, same framing as the rest of this repo.

## The contract it implements

```
POST   /fetch              {"url": "<spotify or deezer track url>",
                            "title": "<expected title>",   # optional
                            "artist": "<expected artist>"} # optional
                           -> {"job_id", "state"}
GET    /fetch/<job_id>      -> {"state": resolving|downloading|ready|failed,
                                 "filename", "size", "downloaded", "speed_mbps",
                                 "mismatched", "error"}
GET    /fetch/<job_id>/file -> streams the finished FLAC
DELETE /fetch/<job_id>      -> drops the temp file
POST   /bot/command         {"command": "/help"} -> {"command", "reply"}
```

A job API rather than one blocking request: bot delivery genuinely takes tens of
seconds, and the parallel download completes out of order, so nothing here can be
served synchronously from a single request. Fetches are processed **one at a
time** — the bot chat is a single stateful conversation and a reply is matched by
"a new inbound message after the id recorded before sending", which two
concurrent fetches would race for.

`title`/`artist` are what the reply is checked against. Without them the gateway
accepts any FLAC newer than the send, which is how a duplicate or late reply for
a *previous* track gets attributed to this job and filed under the wrong name.
The bot names its files after the Deezer track, so the name is the evidence:
`matching.py` compares it loosely (qualifiers like `- From "Film"` dropped, both
directions of containment) and keeps waiting on a mismatch instead of accepting.
It fails open wherever it can't judge — no expectation given, or a filename in a
non-Latin script with nothing comparable left — because rejecting a correct file
fails a download that would otherwise have worked. `mismatched` counts the
rejections, and the failure message names them. `FLAC_MATCH_STRICT=0` restores
the old accept-anything behaviour.

Tests: `python3 test_matching.py` (stdlib only, no Flask or Telethon needed).

`/bot/command` reads the bot's own menu — `/help`, `/info`, `/follow`,
`/privacy`, `/settings` — which is the only documentation it has, and the way to
answer "what links does it actually accept?" without guessing. Allowlisted
rather than free text: this shares one stateful conversation with the fetch
worker, so an arbitrary-send primitive would be both a footgun and a way to
start downloads outside the job API. It refuses with 409 while a fetch is in
flight, because the reply matcher expects the next inbound message to be a
document.

## Run it (Docker Compose)

```yaml
services:
  spotiflac:
    image: ghcr.io/akvaithi/spotiflac:latest
    ports: ["8080:8080"]
    volumes:
      - /DATA/Media/Music:/downloads
      - /DATA/AppData/spotiflac:/config
    restart: unless-stopped

  flacit-gateway:
    image: ghcr.io/akvaithi/flacit-gateway:latest
    ports: ["8082:8082"]
    volumes:
      - /DATA/AppData/flacit-gateway:/config   # stores the Telegram session
    restart: unless-stopped
```

## One-time login

The gateway needs an authenticated Telegram session at
`/config/telegram-session.session`. Two ways to get one:

**Copy an existing session (recommended).** If you already have a working
Telethon session — e.g. from FlacIt's `~/.newsong_session.session`, which has
already started `@deezload2bot` and joined its channel — copy it into
`/DATA/AppData/flacit-gateway/telegram-session.session` before starting the
container. It works immediately on boot.

**Or log in fresh** at `http://<your-zimaos-ip>:8082/login` (phone → code →
2FA if enabled). After that succeeds, **open Telegram once yourself**, start a
chat with `@deezload2bot`, press Start, and join its channel — the bot won't
respond to a session that hasn't done this, and the gateway can't automate it.

Either way, on first successful connection the gateway automatically drives the
bot's `/settings` menu to lock its output to FLAC (otherwise it defaults to MP3
320kbps) and records that in `/config/.flac_quality_set` so it only happens once.

## Point SpotiFLAC at it

`FLACIT_GATEWAY_URL` env var on the SpotiFLAC container, or `flacitApiUrl` in its
`config.json`:
```
http://172.17.0.1:8082
```
**Must be docker0's host gateway plus the published port, not a container IP or
container name.** Both containers run `network_mode: bridge`, so container IPs
shift on every recreate and container-name DNS doesn't resolve — a saved
`172.17.0.x` address breaks silently on the next deploy.

## Notes / troubleshooting

- `GET /` reports `{ok, logged_in, me, flac_quality_set, active_job}` — check
  this first when a download won't start.
- Timed out waiting for a FLAC almost always means the account hasn't started
  `@deezload2bot` and joined its channel yet (see above), or the track isn't on
  Deezer.
- Jobs are reaped (temp file removed) 10 minutes after finishing, whether
  `ready` or `failed`, in case a caller never calls `DELETE`.
- 16-bit/44.1kHz only — this is what Deezer serves. There's no hi-res tier here.
