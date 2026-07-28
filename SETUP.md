# SpotiFLAC on ZimaOS — full setup guide

Two prebuilt images, pulled straight from GitHub Container Registry:

- `ghcr.io/akvaithi/spotiflac:latest` — the web app
- `ghcr.io/akvaithi/tidal-gateway:latest` — optional: your own Tidal account gateway

No building required. Personal use only.

---

## Step 0 — Make the packages public (one time)

New ghcr packages start **private**. So `docker pull` works without a login, make
each public once, in your browser:

1. https://github.com/users/akvaithi/packages/container/spotiflac/settings
   → **Danger Zone → Change visibility → Public** → confirm (`spotiflac`).
2. https://github.com/users/akvaithi/packages/container/tidal-gateway/settings
   → **Danger Zone → Change visibility → Public** → confirm (`tidal-gateway`).

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
      - /DATA/AppData/spotiflac:/config      # settings/history/session (keep!)
    environment:
      - ADDR=:8080
      - DOWNLOAD_DIR=/downloads
      - CONFIG_DIR=/config
    restart: unless-stopped

  tidal-gateway:
    image: ghcr.io/akvaithi/tidal-gateway:latest
    container_name: tidal-gateway
    ports:
      - "8081:8081"
    volumes:
      - /DATA/AppData/tidal-gateway:/config   # stores your Tidal login token
    restart: unless-stopped
```

Save (`Ctrl+O`, `Enter`, `Ctrl+X`).

> Don't want the gateway yet? Delete the whole `tidal-gateway:` block and skip
> Steps 3–4. You'll use the community servers (subject to their rate limits).

---

## Step 2 — Start it

```bash
docker compose pull
docker compose up -d
```

Open **http://<your-zimaos-ip>:8080** in a browser. The app should load.

Check both are running:
```bash
docker compose ps
```

---

## Step 3 — Log in to the Tidal gateway (one time, hi-res)

The gateway uses Tidal's PKCE login, which is what unlocks **lossless / hi-res FLAC**
(the simpler device login is capped at AAC).

1. Open **http://<your-zimaos-ip>:8081/login** in a browser.
2. Click **"Click here to log in to Tidal"**, sign in with your subscription.
3. You'll be redirected to an **"Oops" / not-found page** — that's expected.
4. **Copy that page's full URL** (starts with `https://tidal.com/android/login/auth?code=...`)
   and paste it into the box, then **Complete login**.

You should see "Logged in with hi-res access." The token is saved to `/config`
(the `tidal-gateway` volume) and auto-refreshed — one time only.

---

## Step 4 — Point SpotiFLAC at your gateway

1. In SpotiFLAC → **Settings** tab.
2. Under **Your own gateway**, set **Custom Tidal API URL** to:
   ```
   http://<your-zimaos-ip>:8081
   ```
3. Click **Test** → it should say **“online (returns a FULL manifest)”**.
4. Click **Save gateway URLs**.

Now Tidal downloads go through your account — no community servers, no Cloudflare
check, no rate limits.

---

## Step 5 — Download something

1. **Get** tab → paste a Spotify track/album/playlist URL (or switch to Search).
2. Pick **Service = Tidal**, choose format, click **Fetch**, then **Download**.
3. Watch the **Queue** tab for progress. Files appear under **Files** and in your
   `/downloads` share.

If you did NOT set up the gateway, a download may show a **Verification required**
modal (community Cloudflare check) or a **rate-limit banner** — both are normal;
follow the modal, or wait for the retry.

---

## Updating later

```bash
cd ~/spotiflac
docker compose pull && docker compose up -d
```

## Common issues

| Symptom | Fix |
|---|---|
| `pull access denied` / `not found` | Package still private — do Step 0. |
| Gateway **Test** fails | Check `docker compose logs tidal-gateway`; make sure you completed the `link.tidal.com` login. |
| Stuck on “downloading” with a yellow banner | Community server rate limit/cooldown — wait, or use the gateway. |
| Queue won’t start (“already downloading”) | Click **Stop** on the Queue tab, or `docker restart spotiflac`. |
| Hi-Res not working | Needs HiFi Plus on your Tidal account; otherwise it serves lossless. |
| Re-login to Tidal | Delete the `tidal-session.json` in the gateway’s `/config` volume, then `docker restart tidal-gateway`. |
