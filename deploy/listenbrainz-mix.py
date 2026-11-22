#!/usr/bin/env python3
"""Publish the newest ListenBrainz exploration as a playable catalog mix."""

import concurrent.futures
import json
import os
import re
import sqlite3
import sys
import tempfile
import unicodedata
from pathlib import Path
from urllib.parse import quote, urlencode
from urllib.request import Request, urlopen

LISTENBRAINZ = "https://api.listenbrainz.org/1/"
DEEZER = "https://api.deezer.com/"
OUTPUT = Path("/srv/fast/apps/SpotiFLAC/config/listenbrainz-mix.json")
LIBRARY = Path("/srv/fast/apps/navidrome/data/navidrome.db")
PLAYLIST_EXTENSION = "https://musicbrainz.org/doc/jspf#playlist"


def get(url):
    request = Request(url, headers={"User-Agent": "SpotiFLAC-ListenBrainz-Mix/1.0"})
    with urlopen(request, timeout=8) as reply:
        result = json.load(reply)
    if result.get("error"):
        raise ValueError("Remote catalog returned an error")
    return result


def normalized(text):
    return "".join(c for c in unicodedata.normalize("NFKD", text.casefold()) if c.isalnum())


def primary(artists):
    return normalized(re.split(r"\s+feat\.?\s+|[•;,+]", artists, maxsplit=1, flags=re.I)[0])


def resolve(track):
    title, artist = track.get("title", ""), track.get("creator", "")
    if not title or not artist:
        return None
    search = f"{artist} {title}"
    data = get(DEEZER + "search?" + urlencode({"q": search, "limit": 12}))["data"]

    def exact(row):
        name = normalized(row.get("title", ""))
        return (name == normalized(title)
                and primary(row.get("artist", {}).get("name", "")) == primary(artist)
                and (not track.get("duration") or abs(row["duration"] - track["duration"] / 1000) <= 4))

    candidates = [row for row in data if exact(row)]
    album = normalized(track.get("album", ""))
    releases = [row for row in candidates if album and normalized(row.get("album", {}).get("title", "")) == album]
    matches = releases or (candidates if track.get("duration") else [])
    if not matches:
        return None
    if len(matches) > 1 and not releases:
        return None
    row = get(DEEZER + "track/" + str(matches[0]["id"]))
    cover = row.get("album", {})
    return {
        "source": "deezer", "id": str(row["id"]), "title": row["title"],
        "artists": row["artist"]["name"], "album": cover["title"],
        "coverURL": cover.get("cover_big", ""), "duration": row["duration"],
        "isrc": row.get("isrc", ""),
    }


def build():
    account = os.environ["LISTENBRAINZ_USER"]
    playlist_id = os.environ["NAVIDROME_DISCOVERY_PLAYLIST_ID"]
    summaries = get(LISTENBRAINZ + "user/" + quote(account, safe="") + "/playlists/createdfor?count=30")
    generated = []
    for entry in summaries["playlists"]:
        playlist = entry["playlist"]
        ext = playlist.get("extension", {}).get(PLAYLIST_EXTENSION, {})
        source = ext.get("additional_metadata", {}).get("algorithm_metadata", {}).get("source_patch")
        if source == "weekly-exploration":
            generated.append(playlist)
    if not generated:
        raise ValueError("No Weekly Exploration is available")
    latest = max(generated, key=lambda entry: entry["date"])
    identifier = latest["identifier"].rsplit("/", 1)[-1]
    if OUTPUT.exists():
        previous = json.loads(OUTPUT.read_text())
        if previous.get("source_id") == identifier and previous.get("playlist_id") == playlist_id:
            print(f"ListenBrainz exploration is current ({latest['date'][:10]}).")
            return

    payload = get(LISTENBRAINZ + "playlist/" + quote(identifier, safe=""))["playlist"]
    with sqlite3.connect(LIBRARY.as_uri() + "?mode=ro", uri=True) as db:
        rows = db.execute("select title, artist, mbz_recording_id from media_file").fetchall()
    local_keys = {(normalized(title), primary(artist)) for title, artist, _ in rows if title and artist}
    local_mbids = {mbid for _, _, mbid in rows if mbid}
    candidates = []
    for track in payload.get("track", []):
        title, artist = track.get("title", ""), track.get("creator", "")
        mbids = {value.rsplit("/", 1)[-1] for value in track.get("identifier", [])}
        if title and artist and not (local_mbids & mbids) and (normalized(title), primary(artist)) not in local_keys:
            candidates.append(track)

    resolved = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        for row in pool.map(resolve, candidates):
            if row and (normalized(row["title"]), primary(row["artists"])) not in local_keys:
                resolved.setdefault(row["isrc"] or row["id"], row)
    tracks = list(resolved.values())[:30]
    if not tracks:
        raise ValueError("No exact catalog tracks resolved. Keeping the last mix.")
    with tempfile.NamedTemporaryFile(mode="w", dir=OUTPUT.parent, delete=False) as file:
        temp = Path(file.name)
        try:
            json.dump({"playlist_id": playlist_id, "source_id": identifier,
                       "source_date": latest["date"], "tracks": tracks}, file, ensure_ascii=False)
            file.write("\n")
            file.close()
            os.replace(temp, OUTPUT)
        finally:
            temp.unlink(missing_ok=True)
    print(f"Published {len(tracks)} discovery tracks from ListenBrainz {latest['date'][:10]}.")


if __name__ == "__main__":
    try:
        build()
    except (OSError, ValueError, KeyError, sqlite3.Error) as error:
        print(f"ListenBrainz refresh failed ({type(error).__name__}). Keeping the last mix.", file=sys.stderr)
        sys.exit(1)
