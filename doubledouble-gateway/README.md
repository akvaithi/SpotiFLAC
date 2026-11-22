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
7. Back up the cloudflared config. Add an ingress rule for music.akvaithi.page
   with path ^/rest/ and service http://localhost:8080 before its existing rule.
   Keep the existing default rule pointing at Navidrome on port 4533.
8. Validate tunnel ingress and reload its config. Confirm normal playback, XML
   and JSON search, authentication, acquisition, source verification and the
   resulting Navidrome row. Update the akvcloud dashboard catalog.

Arpeggi keeps its current public music URL and Navidrome credentials. Search for
a track; favourite its download row to queue it. Downloads do not turn a virtual
row into instant playback. Once Navidrome indexes the real file, refresh the
library and play the normal row.

## Rollback

Restore the tunnel config backup to route every request directly to Navidrome.
Stop the SpotiFLAC container and doubledouble-browser.service. Existing music
and the original Navidrome database remain in place.

## Checks

Run CGO_ENABLED=0 go test ./... and CGO_ENABLED=0 go vet ./... from the repo root.
Run python3 -m unittest discover -s doubledouble-gateway for source archive and
URL handling tests. Live verification requires the user in server Chromium;
unit tests never contact the source or solve a challenge.
