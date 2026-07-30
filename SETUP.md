# SpotiFLAC on ZimaOS — full setup guide

Two prebuilt images, pulled straight from GitHub Container Registry:

- `ghcr.io/akvaithi/spotiflac:latest` — the web app
- `ghcr.io/akvaithi/flacit-gateway:latest` — the Telegram gateway (required; this
  is SpotiFLAC's only download source)

No building required. Personal use only.

---

## Step 0 — Make the packages public (one time)

New ghcr packages start **private**. So `docker pull` works without a login, make
each public once, in your browser:

1. https://github.com/users/akvaithi/packages/container/spotiflac/settings
   → **Danger Zone → Change visibility → Public** → confirm (`spotiflac`).
2. https://github.com/users/akvaithi/packages/container/flacit-gateway/settings
   → **Danger Zone → Change visibility → Public** → confirm (`flacit-gateway`).

(If you skip this, you must run `docker login ghcr.io -u akvaithi` on the server
first, with a token that has `read:packages`.)

---

## Step 1 — Create the compose file on ZimaOS

SSH into ZimaOS (or use its terminal). Make a folder and a compose file:

```bash
mkdir -p ~/spotiflac && cd ~/spotiflac
nano docker-compose.yml
```

Paste this, then adjust the **left-hand** volume paths to real folders on your box:

```yaml
services:
  spotiflac:
    image: ghcr.io/akvaithi/spotiflac:latest
    container_name: spotiflac
    ports:
      - "8080:8080"
    volumes:
      - /DATA/Media/Music:/downloads        # where FLACs land
      - /DATA/AppData/spotiflac:/config      # settings/history/index (keep!)
    environment:
      - ADDR=:8080
      - DOWNLOAD_DIR=/downloads
      - CONFIG_DIR=/config
      - FLACIT_GATEWAY_URL=http://172.17.0.1:8082
    restart: unless-stopped

  flacit-gateway:
    image: ghcr.io/akvaithi/flacit-gateway:latest
    container_name: flacit-gateway
    ports:
      - "8082:8082"
    volumes:
      - /DATA/AppData/flacit-gateway:/config   # stores the Telegram session
    restart: unless-stopped
```

Save (`Ctrl+O`, `Enter`, `Ctrl+X`).

---

## Step 2 — Bootstrap the Telegram session (before first start)

`flacit-gateway` needs an authenticated Telethon session — and a fresh login
alone isn't enough, because the account also has to have manually started
`@deezload2bot` and joined its channel once, or every download times out.

**If you already have a working session** (e.g. from
[FlacIt](https://github.com/BunnY-exe/FlacIt) — `~/.newsong_session.session`),
copy it onto the box before starting the container:

```bash
mkdir -p /DATA/AppData/flacit-gateway
scp ~/.newsong_session.session <user>@<zimaos-ip>:/DATA/AppData/flacit-gateway/telegram-session.session
```

**Starting from scratch instead?** Skip ahead to Step 3, then:

1. Open **http://<your-zimaos-ip>:8082/login**, enter the phone number, the
   code sent to Telegram, and a 2FA password if you have one set.
2. Open Telegram yourself with that account, search **@deezload2bot**, press
   **Start**, and join its channel when it asks. This is the one step the
   gateway can't automate.

Either way, on first successful connection the gateway automatically switches
the bot's output quality to FLAC (it defaults to MP3 320kbps) — nothing to do
there.

---

## Step 3 — Start it

```bash
docker compose pull
docker compose up -d
```

Open **http://<your-zimaos-ip>:8080** in a browser. The app should load.

Check both are running:
```bash
docker compose ps
```

Check the gateway sees your session:
```bash
curl -s http://<your-zimaos-ip>:8082/
# {"ok":true,"logged_in":true,"me":"yourusername","flac_quality_set":true,"active_job":null}
```

---

## Step 4 — Download something

1. **Get** tab → paste a Spotify track/album/playlist URL (or switch to Search).
2. Pick a **Filename** format, click **Fetch**, then **Download**.
3. Watch the **Queue** tab for progress. Files appear under **Files** and in your
   `/downloads` share.

If `logged_in` was false in Step 3, downloads will fail with a "not logged in"
error — go back to Step 2.

---

## Updating later

```bash
cd ~/spotiflac
docker compose pull && docker compose up -d
```

**Re-check the gateway URL after every update.** Both containers run
`network_mode: bridge`, so their IPs can shift on recreate — `172.17.0.1` (the
docker0 host gateway) is the one stable address; a container IP saved
elsewhere will silently break.

## Common issues

| Symptom | Fix |
|---|---|
| `pull access denied` / `not found` | Package still private — do Step 0. |
| Downloads fail immediately, "not logged in" | `flacit-gateway`'s session is missing or expired — redo Step 2. |
| Stuck "resolving" / times out waiting for a FLAC | The account hasn't started `@deezload2bot` / joined its channel — see Step 2's second bullet. Or the track just isn't on Deezer. |
| Queue won't start ("already downloading") | Click **Stop** on the Queue tab, or `docker restart spotiflac`. |
| Only 16-bit/44.1kHz FLAC | Expected — Deezer via the bot doesn't offer hi-res; there's no fallback tier for it. |
| Re-login to Telegram | Delete `telegram-session.session` in the gateway's `/config` volume, then `docker restart flacit-gateway` and redo Step 2. |
