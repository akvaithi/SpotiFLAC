# SpotiFLAC (self-hosted, headless)

A **headless Docker rebuild** of [afkarxyz/SpotiFLAC](https://github.com/afkarxyz/SpotiFLAC),
this fork's upstream base. The original is a Wails desktop app; this repo replaces
that GUI with an HTTP server + embedded web UI, so it runs unattended on a home
server instead of a laptop. Paste a Spotify track/album/playlist URL (or search),
and it lands as a lossless FLAC on a mounted volume.

It also serves a **Subsonic API** at `/rest/*` that reverse-proxies Navidrome, so
an ordinary unmodified Subsonic client — Cassette, Amperfy, Arpeggi — can search
the Spotify catalog and download a track to the server by favouriting it. See
[`SUBSONIC-FACADE.md`](SUBSONIC-FACADE.md).

> Personal, self-hosted tooling — see [`README-WEB.md`](README-WEB.md) for the full
> architecture and [`SETUP.md`](SETUP.md) for a from-scratch ZimaOS/Docker walkthrough.

## Current download source: DoubleDouble

Search in Arpeggi, then favourite a download result to queue it on the server.
The Subsonic facade forwards ordinary playback and library operations to Navidrome.
Downloads now use [us.doubledouble.top](https://us.doubledouble.top), through a
separate profile in the existing server Chromium container. The old Telegram source
is retained in the repository as historical code and is not deployed.

Pixeldrain and other external uploads are always disabled in both the page controls
and the download request. Only same-origin direct downloads are accepted. Single-track
ZIPs are unpacked safely; a response containing multiple tracks or lossy audio is rejected.
The original audio identity is checked before SpotiFLAC writes the requested tags.

DoubleDouble currently requires a CAPTCHA. Its own page handles verification in
server Chromium; the queue waits up to 25 minutes for the user and resumes afterward.
No CAPTCHA is solved or bypassed by this code. The browser profile keeps the site's
session between downloads, subject to the site's own expiry rules.

See [the current deployment guide](doubledouble-gateway/README.md) for server setup,
verification and rollback. Older setup documents describe the retired deployment.

## Disclaimer

This project is for **personal, self-hosted use**. It is not affiliated with,
endorsed by, or connected to Spotify, Deezer, Telegram, or the maintainers of
the upstream SpotiFLAC or FlacIt projects. You are responsible for complying
with your local laws and the Terms of Service of the platforms involved. The
software is provided "as is", without warranty of any kind — see
[`LICENSE`](LICENSE).

## Credits

- [afkarxyz/SpotiFLAC](https://github.com/afkarxyz/SpotiFLAC) — the upstream
  project this fork is built on: Spotify catalog/metadata, tagging, the
  download queue and library tooling
- [BunnY-exe/FlacIt](https://github.com/BunnY-exe/FlacIt) — the Telegram/MTProto
  download pipeline `flacit-gateway` is ported from
- [MusicBrainz](https://musicbrainz.org) · [LRCLIB](https://lrclib.net) ·
  [Songlink/Odesli](https://song.link) — metadata, lyrics and link resolution
