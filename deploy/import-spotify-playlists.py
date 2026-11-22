#!/usr/bin/env python3
"""Transfer the authorized Spotify playlist snapshot to private Navidrome playlists."""

import collections
import json
import os
import re
import runpy
import sqlite3
import sys
import tempfile
from pathlib import Path
from urllib.parse import urlsplit

helpers = runpy.run_path(Path(__file__).with_name("listenbrainz-library-mixes.py"))
navidrome = helpers["navidrome"]
normalized = helpers["normalized"]
primary = helpers["primary"]
database = helpers["database"]

SPOTIFY = Path("/srv/fast/apps/.spotify-oauth/snapshot.json")
OUTPUT = Path("/srv/fast/apps/SpotiFLAC/config/spotify-playlists.json")
SOURCE = "Imported from Spotify: https://open.spotify.com/playlist/"
SPOTIFY_ID = re.compile(r"[A-Za-z0-9]{22}")


def catalog():
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as db:
        rows = db.execute("""
            select id, title, artist, album, album_id, artist_id, suffix, duration,
                   has_cover_art, tags
            from media_file where not missing order by id
        """).fetchall()

    by_isrc = collections.defaultdict(list)
    by_name = collections.defaultdict(list)
    for row in rows:
        local_id, title, artists, album, album_id, artist_id, suffix, duration, cover, tags = row
        if not title or not artists:
            continue
        entry = {
            "LocalID": local_id, "Title": title, "Artists": artists, "Album": album or "",
            "AlbumID": album_id, "ArtistID": artist_id, "Suffix": suffix,
            "Duration": round(duration or 0), "HasCover": bool(cover),
        }
        try:
            isrc = next((tag["value"].upper() for tag in json.loads(tags or "{}").get("isrc", [])
                         if tag.get("value")), "")
        except ValueError:
            isrc = ""
        if isrc:
            by_isrc[isrc].append(entry)
        by_name[normalized(title), primary(artists)].append(entry)
    return by_isrc, by_name


def entry_for(track, by_isrc, by_name):
    if not track or track.get("type") != "track" or track.get("local"):
        return None
    track_id = track.get("id") or ""
    name = track.get("name") or ""
    artists = track.get("artists") or []
    if not SPOTIFY_ID.fullmatch(track_id) or not name or not artists:
        return None
    album = track.get("album") or ""
    duration = track.get("duration") or 0
    isrc = (track.get("isrc") or "").upper()
    exact = by_isrc.get(isrc) if isrc else None
    if exact:
        same_album = [row for row in exact if album and normalized(row["Album"]) == normalized(album)]
        return (same_album or exact)[0]

    named = by_name.get((normalized(name), primary(artists[0])), [])
    # With no recording-code match require the same release and a plausible
    # duration. If two local editions still match, defer to the source ID.
    local = [row for row in named if album and normalized(row["Album"]) == normalized(album)
             and (not duration or abs(row["Duration"] - duration) <= 4)]
    if len(local) == 1:
        return local[0]
    artwork = track.get("artwork") or ""
    url = urlsplit(artwork)
    if url.scheme != "https" or url.hostname not in {"i.scdn.co", "mosaic.scdn.co"}:
        artwork = ""
    return {"Source": "spotify", "ID": track_id, "Title": name,
            "Artists": " • ".join(artists), "Album": album, "Duration": duration,
            "ISRC": isrc, "CoverURL": artwork}


def available_name(name, taken):
    name = name.strip() or "Spotify playlist"
    number = 2
    result = name
    while result.casefold() in taken:
        result = f"{name} ({number})"
        number += 1
    taken.add(result.casefold())
    return result


def publish():
    snapshot = json.loads(SPOTIFY.read_text())
    if snapshot.get("playlist_track_schema") != 2:
        raise ValueError("Refresh the Spotify snapshot before importing")
    path = Path(os.environ["CREDENTIALS_DIRECTORY"]) / "navidrome-mix"
    credentials = json.loads(path.read_text())
    by_isrc, by_name = catalog()
    playlists = navidrome(credentials, "getPlaylists")["playlists"].get("playlist", [])
    taken = {row["name"].casefold() for row in playlists}
    existing = {row["comment"]: row for row in playlists
                if row.get("owner") == credentials["user"] and not row.get("public")
                and (row.get("comment") or "").startswith(SOURCE)}
    # Keep previous successfully transferred playlists if access later changes.
    imported = json.loads(OUTPUT.read_text()) if OUTPUT.exists() else {}
    accessible = missing = skipped = local = acquired_on_tap = 0
    for playlist in snapshot["playlists"]:
        if "tracks" not in playlist:
            missing += 1
            continue
        if not SPOTIFY_ID.fullmatch(playlist["id"]):
            continue
        reference = SOURCE + playlist["id"]
        rows = [entry for track in playlist["tracks"] if (entry := entry_for(track, by_isrc, by_name))]
        skipped += len(playlist["tracks"]) - len(rows)
        if reference in existing:
            target_id = existing[reference]["id"]
        else:
            name = available_name(playlist["name"], taken)
            target_id = navidrome(credentials, "createPlaylist", [("name", name)])["playlist"]["id"]
            navidrome(credentials, "updatePlaylist",
                      [("playlistId", target_id), ("public", "false"), ("comment", reference)])
        imported[target_id] = rows
        accessible += 1
        local += sum("LocalID" in row for row in rows)
        acquired_on_tap += sum("Source" in row for row in rows)

    with tempfile.NamedTemporaryFile(mode="w", dir=OUTPUT.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(imported, output, ensure_ascii=False, separators=(",", ":"))
            output.write("\n")
            output.close()
            os.replace(temporary, OUTPUT)
        finally:
            temporary.unlink(missing_ok=True)
    print(f"Imported {accessible} playlists: {local} local entries, {acquired_on_tap} tap-to-acquire entries.")
    print(f"Spotify withheld tracks for {missing} playlists. Skipped {skipped} unsupported entries.")


if __name__ == "__main__":
    try:
        publish()
    except (OSError, ValueError, KeyError, sqlite3.Error) as error:
        print(f"Spotify playlist import failed ({type(error).__name__}).", file=sys.stderr)
        sys.exit(1)
