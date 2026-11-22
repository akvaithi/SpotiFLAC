#!/usr/bin/env python3
"""Refresh the library-backed ListenBrainz playlists through Navidrome."""

import collections
import hashlib
import json
import math
import os
import random
import runpy
import secrets
import sqlite3
import sys
import time
from datetime import datetime
from pathlib import Path
from urllib.parse import quote, urlencode
from urllib.request import urlopen
from zoneinfo import ZoneInfo

helpers = runpy.run_path(Path(__file__).with_name("listenbrainz-mix.py"))
get = helpers["get"]
normalized = helpers["normalized"]
primary = helpers["primary"]
base = helpers["LISTENBRAINZ"]
database = helpers["LIBRARY"]
playlist_extension = helpers["PLAYLIST_EXTENSION"]


def navidrome(credentials, endpoint, fields=()):
    salt = secrets.token_hex(16)
    auth = hashlib.md5((credentials["password"] + salt).encode()).hexdigest()
    query = urlencode([
        ("u", credentials["user"]), ("t", auth), ("s", salt),
        ("c", "listenbrainz-mixes"), ("v", "1.16.1"), ("f", "json"), *fields,
    ])
    url = "http://127.0.0.1:4534/rest/" + endpoint + ".view?" + query
    with urlopen(url, timeout=10) as reply:
        response = json.load(reply)["subsonic-response"]
    if response["status"] != "ok":
        raise ValueError(f"Navidrome rejected {endpoint}")
    return response


def index_library():
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as db:
        rows = db.execute("""
            select id, title, artist, album, mbz_recording_id, tags, genre
            from media_file where not missing order by id
        """).fetchall()
    by_name = collections.defaultdict(list)
    by_mbid = collections.defaultdict(list)
    catalog = {}
    for song_id, title, artist, album, mbid, raw_tags, raw_genre in rows:
        if title and artist:
            performer = primary(artist)
            by_name[normalized(title), performer].append((song_id, normalized(album or "")))
            try:
                tags = json.loads(raw_tags or "{}")
            except ValueError:
                tags = {}
            genres = {tag["value"].casefold() for tag in tags.get("genre", []) if tag.get("value")}
            if raw_genre:
                genres.add(raw_genre.casefold())
            isrc = next((tag["value"].upper() for tag in tags.get("isrc", []) if tag.get("value")), "")
            catalog[song_id] = (performer, genres, isrc or (normalized(title), performer))
        if mbid:
            by_mbid[mbid].append((song_id, normalized(album or "")))

    def match(title, artist, album="", mbid=""):
        if not title or not artist:
            return None
        candidates = by_mbid.get(mbid) or by_name.get((normalized(title), primary(artist)), [])
        album_matches = [row for row in candidates if album and row[1] == normalized(album)]
        unique = album_matches or candidates
        return unique[0][0] if len(unique) == 1 else None

    return match, catalog


def weekly(account, match):
    entries = get(base + f"user/{account}/playlists/createdfor?count=30")["playlists"]
    candidates = []
    for item in entries:
        playlist = item["playlist"]
        source = playlist.get("extension", {}).get(playlist_extension, {}).get("additional_metadata", {})
        if source.get("algorithm_metadata", {}).get("source_patch") == "weekly-jams":
            candidates.append(playlist)
    if not candidates:
        return []
    playlist_id = max(candidates, key=lambda item: item["date"])["identifier"].rsplit("/", 1)[-1]
    songs = get(base + "playlist/" + playlist_id)["playlist"]["track"]
    return list(dict.fromkeys(song_id for song in songs if (song_id := match(
        song.get("title"), song.get("creator"), song.get("album") or "",
        next((identifier.rsplit("/", 1)[-1] for identifier in song.get("identifier", [])), ""),
    ))))[:30]


def throwbacks(account, match, exclude):
    cutoff = int(time.time()) - 365 * 86400
    played = get(base + f"user/{account}/listens?count=1000&max_ts={cutoff}")["payload"]["listens"]
    counts = collections.Counter()
    performers = {}
    for listen in played:
        song = listen["track_metadata"]
        artist = song.get("artist_name", "")
        song_id = match(song.get("track_name"), artist, song.get("release_name") or "")
        if song_id and song_id not in exclude:
            counts[song_id] += 1
            performers[song_id] = primary(artist)
    selected = []
    by_artist = collections.Counter()
    for song_id, _ in counts.most_common():
        artist = performers[song_id]
        if by_artist[artist] < 4:
            selected.append(song_id)
            by_artist[artist] += 1
        if len(selected) == 30:
            break
    return selected


# Lead with the tags that describe each mix most directly. A smaller weight
# lets neighboring styles broaden the rotation without taking it over.
MOODS = {
    "Late Night": (
        {"r&b", "alternative r&b", "contemporary r&b", "neo soul", "jazz", "jazz pop", "soul", "downtempo"},
        {"bedroom pop", "lo-fi", "trip hop"},
    ),
    "Chill & Focus": (
        {"ambient", "lo-fi", "bedroom pop", "indie folk", "easy listening", "acoustic", "instrumental"},
        {"indie pop", "soundtrack", "jazz", "indie"},
    ),
    "High Energy": (
        {"dance-pop", "electro house", "electropop", "electronic", "dance", "house", "rock", "pop rock", "alternative rock"},
        {"hip hop", "rap", "pop rap", "indie rock", "synth-pop"},
    ),
    "Feel Good": (
        {"k-pop", "girl group", "boy band", "pop", "mandopop", "c-pop", "filmi"},
        {"indie pop", "synth-pop", "country", "pop rock"},
    ),
}


def listening_affinity(account, match, catalog):
    tracks = collections.Counter()
    artists = collections.Counter()
    try:
        listens = get(base + f"user/{account}/listens?count=1000")["payload"]["listens"]
    except (OSError, ValueError, KeyError):
        print("ListenBrainz history unavailable. Using library tags for today's mood mixes.")
        return tracks, artists
    for listen in listens:
        item = listen["track_metadata"]
        song_id = match(item.get("track_name"), item.get("artist_name"), item.get("release_name") or "")
        if song_id in catalog:
            tracks[song_id] += 1
            artists[catalog[song_id][0]] += 1
    return tracks, artists


def daily_moods(catalog, track_plays, artist_plays, day):
    used = set()
    for mood, (core, related) in MOODS.items():
        rng = random.Random(f"{day.isoformat()}:{mood}")
        candidates = []
        for song_id, (artist, tags, recording) in catalog.items():
            strength = 2 if tags & core else 1 if tags & related else 0
            if not strength or recording in used:
                continue
            affinity = 1 + min(math.log1p(track_plays[song_id]), 2) / 2
            affinity += min(math.log1p(artist_plays[artist]), 3) / 4
            # Weighted sampling without replacement. A day's mix stays stable
            # across restarts, while a new date rotates the selection.
            score = rng.expovariate(strength * affinity)
            candidates.append((score, song_id, artist, recording))

        selected = []
        performer_count = collections.Counter()
        for _, song_id, artist, recording in sorted(candidates):
            if performer_count[artist] >= 3 or recording in used:
                continue
            selected.append(song_id)
            performer_count[artist] += 1
            used.add(recording)
            if len(selected) == 25:
                break
        yield mood, selected


def publish(credentials, name, song_ids, comment, existing):
    if not song_ids:
        print(f"{name}: no matching library tracks. Keeping the previous playlist.")
        return
    playlist = existing.get(name)
    if playlist:
        playlist_id = playlist["id"]
        old = navidrome(credentials, "getPlaylist", [("id", playlist_id)])["playlist"]
        if old.get("public") or old.get("comment") != comment:
            raise ValueError("Existing playlist is not managed by ListenBrainz")
        if [entry["id"] for entry in old.get("entry", [])] != song_ids:
            navidrome(credentials, "createPlaylist", [("playlistId", playlist_id),
                      *(("songId", song_id) for song_id in song_ids)])
    else:
        playlist_id = navidrome(credentials, "createPlaylist", [("name", name),
                               *(("songId", song_id) for song_id in song_ids)])["playlist"]["id"]
        navidrome(credentials, "updatePlaylist", [("playlistId", playlist_id), ("public", "false"), ("comment", comment)])
    print(f"{name}: {len(song_ids)} tracks")


def main():
    account = quote(os.environ["LISTENBRAINZ_USER"], safe="")
    path = Path(os.environ["CREDENTIALS_DIRECTORY"]) / "navidrome-mix"
    credentials = json.loads(path.read_text())
    match, catalog = index_library()
    owned = {p["name"]: p for p in navidrome(credentials, "getPlaylists")["playlists"].get("playlist", [])
             if p.get("owner") == credentials["user"]}
    track_plays, artist_plays = listening_affinity(account, match, catalog)
    day = datetime.now(ZoneInfo("America/Chicago")).date()
    for mood, songs in daily_moods(catalog, track_plays, artist_plays, day):
        publish(credentials, f"Daily Mix · {mood}", songs,
                f"Managed daily {mood} mix based on library tags and ListenBrainz listening history.", owned)
    jams = weekly(account, match)
    memories = throwbacks(account, match, set(jams))
    publish(credentials, "Weekly Jams · ListenBrainz", jams,
            "Managed mix from ListenBrainz Weekly Jams. Refreshed daily.", owned)
    publish(credentials, "Throwbacks · ListenBrainz", memories,
            "Managed mix from your ListenBrainz plays at least a year ago. Refreshed daily.", owned)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, sqlite3.Error) as error:
        # Avoid logging HTTP request URLs, which carry Subsonic authentication.
        print(f"Library mix refresh failed ({type(error).__name__}).", file=sys.stderr)
        sys.exit(1)
