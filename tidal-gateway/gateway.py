"""
Minimal self-hosted Tidal gateway for SpotiFLAC — HI-RES (PKCE) edition.

Logs into Tidal with your own subscription using tidalapi's PKCE flow, which is
the only way to unlock LOSSLESS / HI-RES FLAC (the device-login flow is capped at
HIGH/AAC). Serves the endpoint SpotiFLAC's "Custom Tidal API URL" expects:

    GET /track/?id=<trackId>&quality=<LOSSLESS|HI_RES_LOSSLESS|HIGH|LOW>
      -> { "data": { "trackId": ..., "assetPresentation": "FULL",
                     "manifestMimeType": "...", "manifest": "<base64>" } }

Login is a one-time browser step at  http://<this-host>:8081/login

Personal use only. Downloading via this almost certainly violates Tidal's ToS,
even with a paid account — you accept that responsibility.
"""
import json
import os
import threading
from datetime import datetime

import requests
import tidalapi
from flask import Flask, jsonify, request

SESSION_FILE = os.environ.get("SESSION_FILE", "/config/tidal-session.json")
API = "https://api.tidal.com/v1"

app = Flask(__name__)

_cfg = tidalapi.Config()
# Optional override of the PKCE client (advanced; normally not needed).
if os.environ.get("TIDAL_CLIENT_ID") and os.environ.get("TIDAL_CLIENT_SECRET"):
    _cfg.client_id_pkce = os.environ["TIDAL_CLIENT_ID"]
    _cfg.client_secret_pkce = os.environ["TIDAL_CLIENT_SECRET"]
_session = tidalapi.Session(_cfg)
_lock = threading.Lock()

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
        "is_pkce": True,
    }
    os.makedirs(os.path.dirname(SESSION_FILE), exist_ok=True)
    with open(SESSION_FILE, "w") as f:
        json.dump(data, f)


def _load():
    if not os.path.exists(SESSION_FILE):
        return False
    with open(SESSION_FILE) as f:
        d = json.load(f)
    exp = datetime.fromisoformat(d["expiry_time"]) if d.get("expiry_time") else None
    ok = _session.load_oauth_session(
        d["token_type"], d["access_token"], d.get("refresh_token"), exp,
        is_pkce=d.get("is_pkce", True),
    )
    if ok:
        _session.client_enable_hires()  # ensure token refresh uses the PKCE client
    return ok


def logged_in():
    try:
        if _session.access_token and _session.check_login():
            return True
    except Exception:
        pass
    try:
        return _load() and _session.check_login()
    except Exception:
        return False


def refresh_if_needed():
    if _session.expiry_time and datetime.utcnow() >= _session.expiry_time.replace(tzinfo=None):
        try:
            if _session.token_refresh(_session.refresh_token):
                _save()
        except Exception as e:  # noqa
            print("token refresh failed:", e, flush=True)


def require_login():
    if not logged_in():
        return jsonify({"error": "not logged in — open http://<host>:8081/login in a browser"}), 401
    return None


@app.route("/login", methods=["GET", "POST"])
def login():
    if request.method == "POST":
        url = (request.form.get("url") or "").strip()
        if not url and request.is_json:
            url = (request.get_json(silent=True) or {}).get("url", "").strip()
        if not url:
            return _login_page("Please paste the redirect URL."), 400
        try:
            with _lock:
                token = _session.pkce_get_auth_token(url)
                _session.process_auth_token(token, is_pkce_token=True)
                _session.client_enable_hires()
                _save()
            return _login_page(None, done=True)
        except Exception as e:  # noqa
            return _login_page("Login failed: " + str(e)), 400
    # GET
    if logged_in():
        return _login_page(None, already=True)
    return _login_page(None)


def _login_page(err, done=False, already=False):
    if done:
        body = "<h2>✅ Logged in with hi-res access.</h2><p>You can close this tab and start downloading in SpotiFLAC.</p>"
    elif already:
        body = "<h2>Already logged in.</h2><p>Hi-res access is active.</p>"
    else:
        url = _session.pkce_login_url()
        e = f"<p style='color:#e5484d'>{err}</p>" if err else ""
        body = f"""
        <h2>Tidal hi-res login</h2>
        <ol>
          <li><a href="{url}" target="_blank" rel="noopener">Click here to log in to Tidal</a> (opens a new tab).</li>
          <li>Sign in with your subscription. You'll land on an <b>“Oops” / not-found</b> page — that's expected.</li>
          <li><b>Copy that page's full URL</b> (starts with <code>https://tidal.com/android/login/auth?code=…</code>) and paste it below.</li>
        </ol>
        {e}
        <form method="post">
          <input name="url" style="width:100%;padding:8px" placeholder="https://tidal.com/android/login/auth?code=..." autofocus>
          <br><br><button type="submit" style="padding:8px 16px">Complete login</button>
        </form>
        """
    return (
        "<!doctype html><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>Tidal gateway login</title>"
        "<body style='font:15px/1.6 system-ui,sans-serif;max-width:640px;margin:40px auto;padding:0 16px;color:#111'>"
        + body + "</body>"
    )


@app.route("/track/")
def track():
    tid = request.args.get("id")
    q = QUALITY.get((request.args.get("quality") or "LOSSLESS").upper(), "LOSSLESS")
    if not tid:
        return jsonify({"error": "missing id"}), 400
    gate = require_login()
    if gate:
        return gate
    refresh_if_needed()

    r = requests.get(
        f"{API}/tracks/{tid}/playbackinfopostpaywall",
        params={"audioquality": q, "playbackmode": "STREAM",
                "assetpresentation": "FULL", "countryCode": _session.country_code},
        headers={"Authorization": f"Bearer {_session.access_token}"},
        timeout=30,
    )
    if r.status_code != 200:
        return jsonify({"error": f"tidal returned {r.status_code}", "body": r.text[:300]}), 502
    d = r.json()
    return jsonify({"data": {
        "trackId": int(tid),
        "assetPresentation": d.get("assetPresentation", "FULL"),
        "manifestMimeType": d.get("manifestMimeType", ""),
        "manifest": d.get("manifest", ""),
        "audioQuality": d.get("audioQuality", q),
    }})


@app.route("/account")
def account():
    gate = require_login()
    if gate:
        return gate
    refresh_if_needed()
    out = {"user_id": getattr(_session.user, "id", None), "country": _session.country_code,
           "is_pkce": getattr(_session, "is_pkce", None)}
    try:
        uid = _session.user.id
        r = requests.get(f"{API}/users/{uid}/subscription",
                         params={"countryCode": _session.country_code},
                         headers={"Authorization": f"Bearer {_session.access_token}"}, timeout=20)
        out["subscription"] = r.json() if r.status_code == 200 else {"status": r.status_code}
    except Exception as e:  # noqa
        out["subscription_error"] = str(e)
    return jsonify(out)


@app.route("/")
def health():
    return jsonify({"ok": True, "logged_in": logged_in()})


if __name__ == "__main__":
    if logged_in():
        print("Tidal session loaded (hi-res).", flush=True)
    else:
        print("\n===============  TIDAL LOGIN REQUIRED  ===============", flush=True)
        print("  Open in a browser:  http://<your-server-ip>:8081/login", flush=True)
        print("  (one-time; unlocks hi-res FLAC)", flush=True)
        print("=====================================================\n", flush=True)
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8081")))
