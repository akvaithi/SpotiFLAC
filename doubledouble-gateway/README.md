# DoubleDouble source adapter

The existing Chromium container runs this Python adapter as user abc. It uses the
image's installed websockets package and a dedicated browser profile at
/config/doubledouble/profile. Its private CDP port listens on container loopback.
The HTTP adapter listens only inside the Chromium Docker network, without a
published host port.

The adapter preserves the original source contract: POST /fetch queues an exact
Deezer track, GET /fetch/ID reports progress, GET /fetch/ID/file returns its FLAC,
and DELETE /fetch/ID cancels or releases the job. The Go worker remains serial.
If the adapter restarts, the durable Go queue retries interrupted work.

Verification uses DoubleDouble's own page in a visible server Chromium window.
The user completes its CAPTCHA. The browser keeps session state, but this is not
a promise that one verification lasts forever. Waiting is reported explicitly
and times out after 25 minutes. Cancellation interrupts the waiting page.

The external-upload checkbox is forced off and the request wrapper removes
external from every source submission. Redirects to other origins are rejected.
The source must return real FLAC audio or a safe ZIP containing exactly one FLAC.
The downloader compares the original ISRC, or its title and artist, before
retagging. Different recordings are errors, never silently relabelled.


DoubleDouble currently reports only Amazon Music as working. The adapter maps the
exact Deezer recording to its Amazon Music track through song.link's public track
page, checks that the page identifies the requested Deezer ID, and rejects album-only
links. It never substitutes a text-search result. If no exact mapping exists, the
job fails visibly. The final FLAC identity check remains mandatory.

## Deployment

1. Push this repository and wait for its container build to succeed.
2. Clone or fast-forward /srv/fast/apps/SpotiFLAC on akvcloud.
3. Create config there, owned by uid 1000. Read the existing navidrome and
   spotiflac-api Keychain items on the Mac and supply NAVIDROME_USER,
   NAVIDROME_PASSWORD and API_TOKEN as process environment variables to compose.
4. Install deploy/doubledouble-browser.service and enable it. The unit copies the
   source from the checkout into Chromium's persistent config on every start.
5. Pull and start the spotiflac compose service. It joins chromium_default and
   navidrome_default; its HTTP port is bound to host loopback.
6. Seed ScanLibrary with /downloads so owned tracks are filtered from acquisition
   search results. Library scanning reads tags and does not alter music files.
7. First set SPOTIFLAC_MUSIC_BIND to 127.0.0.1:14533 for pilot checks. Test the
   complete Navidrome web interface and Subsonic API through that listener.
8. Back up the Navidrome compose file. Move its host binding from port 4533
   to 127.0.0.1:4534, leaving the container port and music/data mounts unchanged.
   Then recreate SpotiFLAC with SPOTIFLAC_MUSIC_BIND=0.0.0.0:4533. The tunnel and
   Arpeggi URL remain unchanged. Only the music service is briefly interrupted.
9. Verify playback, XML and JSON search, authentication, acquisition, source
   verification and the resulting Navidrome row. Update the dashboard catalog.

Arpeggi uses the existing public music URL and Navidrome credentials. Tap a
download row to queue it without favoriting. The facade waits up to 12 seconds
and starts playback if Navidrome has indexed the file. If the source needs more
time or CAPTCHA, the job continues and the track can be played from the library
after it appears. See [the Subsonic facade](../SUBSONIC-FACADE.md#55-stream-and-download-on-a-virtual-row).

The dashboard's playlist backfill uses the scoped bearer token accepted by
`POST /api/playlist-backfill`. Submit a public Spotify playlist link with
`queue: false` for a read-only preview, then `queue: true` to request a bounded
batch of missing tracks. The endpoint skips library matches and active queue
entries. It does not create or sync a Navidrome playlist.

## Troubleshooting

The gateway's root health response checks the HTTP service. Test a real
acquisition to confirm the Chromium verification window opens. If the job says
`Verification browser did not become ready` after a container recreation:

1. Verify the remote Chromium desktop responds and the dedicated browser's
   DevTools endpoint on container loopback port 9223 is unavailable.
2. Check whether `/config/doubledouble/profile/SingletonLock` names a previous
   container hostname. If it does, identify any stranded Chromium child with
   that exact profile path and stop that child while the source queue is idle.
3. Once the profile is idle, move the stale `SingletonLock`, `SingletonCookie`
   and `SingletonSocket` symlinks into a backup directory. Leave browser profile
   data in place.
4. Retry an exact track request. After this recovery, a test reached DoubleDouble's
   normal CAPTCHA prompt in about 2.5 seconds with Pixeldrain disabled. That
   measurement ended at the prompt.

## Provider trials, November 2026

- Monochrome had the fastest successful catalog search in the server test. Its
  audio backend was unreachable for the sample.
- Jumo-DL's track search returned HTTP 403. Cyan-DL completed a sample archive
  in 2.4–6.3 seconds after the archive action, but its FLAC request failed.
  The fallback file was Opus.
- The official Mornye Desktop 5.4.0 Linux package launched its graphical runtime
  for `--help` and aborted under the server's virtual display. Its Linux
  launcher has no download subcommand. The separate
  [Mornye Python CLI](https://github.com/mornyeapp/Mornye-Python) 2.2.2 ran in an
  isolated environment. The tested Deezer, Qobuz, Tidal and Amazon extensions
  encountered verification or catalog failures. None produced a sample FLAC.
  These tests did not establish a faster lossless source.

## Rollback

Stop the SpotiFLAC container, restore Navidrome's compose backup and recreate
Navidrome on its original host port 4533. Stop doubledouble-browser.service. Existing music
and the original Navidrome database remain in place.

## Checks

Run CGO_ENABLED=0 go test ./... and CGO_ENABLED=0 go vet ./... from the repo root.
Run python3 -m unittest discover -s doubledouble-gateway for source archive and
URL handling tests. Live verification requires the user in server Chromium;
unit tests never contact the source or solve a challenge.
