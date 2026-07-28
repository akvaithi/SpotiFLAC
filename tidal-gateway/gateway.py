"""
Minimal self-hosted Tidal gateway for SpotiFLAC.

Logs into Tidal ONCE with your own subscription (OAuth device flow), then serves
the endpoint SpotiFLAC's "Custom Tidal API URL" expects:

    GET /track/?id=<trackId>&quality=<LOSSLESS|HI_RES_LOSSLESS|HIGH|LOW>
      -> { "data": { "trackId": <id>, "assetPresentation": "FULL",
                     "manifestMimeType": "...", "manifest": "<base64>" } }

Point SpotiFLAC (Settings -> Custom Tidal API URL) at http://<this-host>:8081

Personal use only. Using this to download almost certainly violates Tidal's ToS,
even with a paid account — you accept that responsibility.
"""
import json
import os
import threading
import time

import requests
import tidalapi
from flask import Flask, jsonify, request

SESSION_FILE = os.environ.get("SESSION_FILE", "/config/tidal-session.json")
API = "https://api.tidal.com/v1"

app = Flask(__name__)
_session = tidalapi.Session()
_lock = threading.Lock()

# SpotiFLAC quality string -> Tidal audioquality
QUALITY = {
    "LOW": "LOW",
    "HIGH": "HIGH",
    "LOSSLESS": "LOSSLESS",
    "HI_RES": "HI_RES_LOSSLESS",
    "HI_RES_LOSSLESS": "HI_RES_LOSSLESS",
}


def _save():
    data = {
        "token_type": _session.token_type,
        "access_token": _session.access_token,
        "refresh_token": _session.refresh_token,
        "expiry_time": _session.expiry_time.isoformat() if _session.expiry_time else None,
    }
    os.makedirs(os.path.dirname(SESSION_FILE), exist_ok=True)
    with open(SESSION_FILE, "w") as f:
        json.dump(data, f)


def _load():
    if not os.path.exists(SESSION_FILE):
        return False
    from datetime import datetime
    with open(SESSION_FILE) as f:
        d = json.load(f)
    exp = datetime.fromisoformat(d["expiry_time"]) if d.get("expiry_time") else None
    return _session.load_oauth_session(d["token_type"], d["access_token"], d["refresh_token"], exp)


def ensure_login():
    with _lock:
        if _session.access_token and _session.check_login():
            return
        if _load() and _session.check_login():
            return
        # Interactive device login. The link is printed to the container logs.
        login, future = _session.login_oauth()
        print("\n==================  TIDAL LOGIN REQUIRED  ==================", flush=True)
        print(f"  Open:  https://{login.verification_uri_complete}", flush=True)
        print("  Log in with your Tidal subscription, then this continues.", flush=True)
        print("===========================================================\n", flush=True)
        future.result()  # blocks until you complete the login in your browser
        _save()
        print("Tidal login complete. Session saved to", SESSION_FILE, flush=True)


def refresh_if_needed():
    from datetime import datetime
    if _session.expiry_time and datetime.utcnow() >= _session.expiry_time.replace(tzinfo=None):
        try:
            if _session.token_refresh(_session.refresh_token):
                _save()
        except Exception as e:  # noqa
            print("token refresh failed:", e, flush=True)


@app.route("/track/")
def track():
    tid = request.args.get("id")
    q = QUALITY.get((request.args.get("quality") or "LOSSLESS").upper(), "LOSSLESS")
    if not tid:
        return jsonify({"error": "missing id"}), 400
    ensure_login()
    refresh_if_needed()

    r = requests.get(
        f"{API}/tracks/{tid}/playbackinfopostpaywall",
        params={
            "audioquality": q,
            "playbackmode": "STREAM",
            "assetpresentation": "FULL",
            "countryCode": _session.country_code,
        },
        headers={"Authorization": f"Bearer {_session.access_token}"},
        timeout=30,
    )
    if r.status_code != 200:
        return jsonify({"error": f"tidal returned {r.status_code}", "body": r.text[:300]}), 502
    d = r.json()
    return jsonify({
        "data": {
            "trackId": int(tid),
            "assetPresentation": d.get("assetPresentation", "FULL"),
            "manifestMimeType": d.get("manifestMimeType", ""),
            "manifest": d.get("manifest", ""),
            "audioQuality": d.get("audioQuality", q),
        }
    })


@app.route("/")
def health():
    return jsonify({"ok": True, "logged_in": bool(_session.access_token)})


if __name__ == "__main__":
    # Trigger login at startup so the link appears immediately in the logs.
    try:
        ensure_login()
    except Exception as e:  # noqa
        print("startup login error (will retry on first request):", e, flush=True)
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8081")))
