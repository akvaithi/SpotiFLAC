# SpotiFLAC (self-hosted, headless)

A **headless Docker rebuild** of [afkarxyz/SpotiFLAC](https://github.com/afkarxyz/SpotiFLAC),
this fork's upstream base. The original is a Wails desktop app; this repo replaces
that GUI with an HTTP server + embedded web UI, so it runs unattended on a home
server instead of a laptop. Paste a Spotify track/album/playlist URL (or search),
and it lands as a lossless FLAC on a mounted volume — used as the acquisition
backend for [Harmony](https://github.com/akvaithi/Harmony), a native macOS client
for a Navidrome + SpotiFLAC stack, but works standalone through its own web UI too.

> Personal, self-hosted tooling — see [`README-WEB.md`](README-WEB.md) for the full
> architecture and [`SETUP.md`](SETUP.md) for a from-scratch ZimaOS/Docker walkthrough.

## Custom download engine: Telegram, not Tidal/Qobuz/Amazon

The upstream project resolves a Spotify track against Tidal, Qobuz and Amazon
Music, each behind its own community-server/gateway arrangement. This fork
replaces all three with a single custom source, ported from
[**FlacIt**](https://github.com/BunnY-exe/FlacIt): a `flacit-gateway` sidecar
runs a persistent [Telethon](https://github.com/LonamiWebs/Telethon) (MTProto)
session against Telegram's `@deezload2bot`, which sources lossless FLAC from
Deezer, and pulls the result down with 16 parallel connections against the
file's own Telegram data center.

```
web UI ──HTTP/SSE──► spotiflac (Go)  ── Spotify catalog, tagging, queue,
                          │              library index, dedup, enrichment
                          └──HTTP──► flacit-gateway (Python/Telethon)
                                       ── drives @deezload2bot, downloads
                                          the FLAC it posts
```

Two containers, two images (`ghcr.io/akvaithi/spotiflac`,
`ghcr.io/akvaithi/flacit-gateway`) — everything Spotify-side (catalog, search,
metadata, tagging, the durable download queue, library dedup, lyrics/genre/cover
enrichment, Navidrome rescan) is unchanged from upstream and provider-agnostic;
only the *download source* was swapped. Trade-offs that come with it: 16-bit/
44.1kHz FLAC only (no hi-res tier), and no fallback source for a track Deezer
doesn't carry. See [`CLAUDE.md`](CLAUDE.md) for the mechanism in full and
[`flacit-gateway/README.md`](flacit-gateway/README.md) for that sidecar's own
contract.

## Quick start

```bash
docker compose up -d
# then open http://<host>:8080
```

Needs a bootstrapped Telegram session before downloads work — see
[`SETUP.md`](SETUP.md) for the full walkthrough, including how to get one.

## FAQ

<details>
<summary>Is this free?</summary>

Yes. No account, login or subscription — the Telegram bot and Spotify's public
metadata are all it needs.

</details>

<details>
<summary>Can this get my Spotify account suspended or banned?</summary>

No. Spotify data is read from its public web player, not through user
authentication — there's no Spotify login involved at all.

</details>

<details>
<summary>Where does the audio come from?</summary>

Deezer, by way of a Telegram bot (`@deezload2bot`) — see the architecture
section above.

</details>

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
