"""Is this document the track that was asked for?

Kept out of gateway.py so it can be tested without Flask or Telethon installed
(`python3 test_matching.py`). Nothing here touches the network or the event
loop — it reads attributes off a message object and compares strings.

The gateway used to accept any FLAC that arrived after the send. That is the
known gap: a duplicate or late reply from a previous job is newer than the next
job's cutoff, so it can be picked up as *that* job's document and filed under
the wrong track's name. Retuning the resend made it unlikely, not impossible.
"""
import os
import re

# Set FLAC_MATCH_STRICT=0 to fall back to accepting anything, if the bot ever
# starts naming files in a way this can't recognise.
MATCH_STRICT = os.environ.get("FLAC_MATCH_STRICT", "1").strip().lower() not in ("0", "false", "no")

_NON_ALNUM = re.compile(r"[^a-z0-9]+")
_PAREN = re.compile(r"[\(\[][^\)\]]*[\)\]]")
_DASH_SUFFIX = re.compile(r"\s+[-–—]\s+.*$")
_EXTENSION = re.compile(r"\.[a-z0-9]{1,5}$", re.IGNORECASE)


def norm(s):
    return _NON_ALNUM.sub("", (s or "").lower())


def norm_core(s):
    """The title with its version qualifier dropped, so `Vaa Vaathi (From
    "Vaathi")` and `Vaa Vaathi - From "Vaathi"` both reduce to `vaavaathi`.

    Mirrors normStr() in SpotiFLAC's library.go so the two ends of the pipeline
    agree on what counts as one title: Spotify supplies the expected name and
    Deezer named the file, and they spell qualifiers differently.
    """
    s = (s or "").lower()
    s = _PAREN.sub("", s)
    s = _DASH_SUFFIX.sub("", s)
    return _NON_ALNUM.sub("", s)


def doc_text(msg):
    """Everything the reply says about what it contains — the file name plus the
    audio tags Telegram carries beside it."""
    parts = []
    doc = getattr(msg, "document", None)
    if doc is not None:
        for attr in getattr(doc, "attributes", []):
            for field in ("file_name", "title", "performer"):
                value = getattr(attr, field, None)
                if not value:
                    continue
                value = str(value)
                if field == "file_name":
                    # Drop the extension, or a name written in a non-Latin
                    # script normalises to "flac" instead of to nothing — which
                    # looks like a readable name this can judge, and rejects a
                    # perfectly good file.
                    value = _EXTENSION.sub("", value)
                parts.append(value)
    return " ".join(parts)


def matches_expected(msg, expect):
    """Whether this document is plausibly the track that was asked for.

    Lenient in one direction on purpose: no expectation, or a reply carrying no
    readable name (a Tamil-script filename normalises to nothing), is accepted.
    Filing the wrong track is worse than a slow download, but rejecting the
    *right* file is worse than both — it fails a download that used to work, and
    it would do so silently on exactly the non-Latin catalogue this library is
    full of.
    """
    if not MATCH_STRICT or not expect:
        return True
    wanted = [w for w in (norm(expect.get("title")), norm_core(expect.get("title"))) if w]
    if not wanted:
        return True
    have = norm(doc_text(msg))
    if not have:
        return True
    # Containment both ways: the bot's filename usually carries the artist too,
    # and occasionally carries a shorter title than the one asked for.
    return any(want in have or have in want for want in wanted)
