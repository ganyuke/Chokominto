#!/usr/bin/env python3
"""Act like Pano Scrobbler and Web Scrobbler against a Chokominto server.

Payloads follow the two clients' serializers, as in
internal/listenbrainz/api_test.go. Prints one line per check and a timing
summary, and exits non-zero if anything a real client would choke on
happens.

  clients.py BASE USER PANO_TOKEN WEB_TOKEN [--listens N] [--stats] [--pages]
"""
import json, random, statistics, sys, time, urllib.error, urllib.request

base, user, pano_token, web_token = sys.argv[1:5]
opts = sys.argv[5:]
listens = int(opts[opts.index("--listens") + 1]) if "--listens" in opts else 30
rnd = random.Random(3939)
timings = {}
failures = []


def call(what, method, path, token=None, scheme="token", body=None):
    req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Content-Type", "application/json; charset=UTF-8")
    if token:
        req.add_header("Authorization", f"{scheme} {token}")
    t = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            code, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, raw = e.code, e.read()
    ms = (time.perf_counter() - t) * 1000
    timings.setdefault(what, []).append(ms)
    try:
        out = json.loads(raw) if raw else None
    except ValueError:
        out = None
        if path.startswith(("/1/", "/apis/")):
            failures.append(f"{what}: reply is not JSON")
    return code, out, raw


def expect(what, cond, detail=""):
    if not cond:
        failures.append(f"{what}: {detail}")
        print(f"FAIL {what} {detail}")


ANI = [("YOASOBI", "アイドル", "アイドル", ""), ("後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド", "結束バンド"),
       ("supercell feat. やなぎなぎ", "君の知らない物語", "君の知らない物語", "supercell"),
       ("ClariS", "コネクト", "魔法少女まどか☆マギカ OST", "Various Artists"),
       ("LiSA", "紅蓮華", "", ""), ("Aimer", "残響散歌", "残響散歌", "Aimer")]


def pano(ts, artist, title, album, kind="single"):
    tm = {"artist_name": artist, "track_name": title,
          "additional_info": {"duration_ms": 213000, "submission_client": "Pano Scrobbler", "submission_client_version": "4.9"}}
    if album:
        tm["release_name"] = album
    l = {"track_metadata": tm}
    if kind != "playing_now":
        l["listened_at"] = ts
    return {"listen_type": kind, "payload": [l]}


def webscrobbler(ts, artist, title, album, album_artist):
    ai = {"submission_client": "Web Scrobbler", "submission_client_version": "3.14.0", "music_service_name": "YouTube",
          "origin_url": f"https://www.youtube.com/watch?v={rnd.randrange(10**9)}", "duration": 229}
    if album_artist:
        ai["release_artist_name"] = album_artist
    tm = {"artist_name": artist, "track_name": title, "additional_info": ai}
    if album:
        tm["release_name"] = album
    return {"listened_at": ts, "track_metadata": tm}


# Setup: Pano checks its token, through both the plain and the Maloja path.
for path in ("/1/validate-token", "/apis/listenbrainz/1/validate-token"):
    code, out, _ = call("validate-token", "GET", path, pano_token)
    expect("pano validate-token " + path, code == 200 and out.get("valid") is True and out.get("user_name") == user, f"{code} {out}")
code, out, _ = call("validate-token", "GET", "/1/validate-token", "wrong")
expect("bad token is refused", code == 401 or (out and out.get("valid") is False), f"{code} {out}")

code, out, _ = call("listen-count", "GET", f"/1/user/{user}/listen-count", pano_token)
start_count = out["payload"]["count"] if code == 200 else None
expect("listen-count", code == 200, f"{code} {out}")

# Listening: now playing, then the scrobble, alternating clients.
now = int(time.time())
sent = 0
for i in range(listens):
    artist, title, album, aa = rnd.choice(ANI)
    ts = now - (listens - i) * 240
    if i % 2 == 0:
        code, out, _ = call("submit playing_now", "POST", "/1/submit-listens?return_msid=true", pano_token, body=pano(ts, artist, title, album, "playing_now"))
        expect("pano playing_now", code == 200 and out.get("status") == "ok", f"{code} {out}")
        code, out, _ = call("submit single", "POST", "/1/submit-listens", pano_token, body=pano(ts, artist, title, album))
        expect("pano single", code == 200 and out.get("status") == "ok", f"{code} {out}")
    else:
        np = {"listen_type": "playing_now", "payload": [webscrobbler(ts, artist, title, album, aa)]}
        del np["payload"][0]["listened_at"]
        code, out, _ = call("submit playing_now", "POST", "/apis/listenbrainz/1/submit-listens", web_token, "Token", np)
        expect("web scrobbler playing_now", code == 200 and out.get("status") == "ok", f"{code} {out}")
        code, out, _ = call("submit single", "POST", "/apis/listenbrainz/1/submit-listens", web_token, "Token",
                            {"listen_type": "single", "payload": [webscrobbler(ts, artist, title, album, aa)]})
        expect("web scrobbler single", code == 200 and out.get("status") == "ok", f"{code} {out}")
    sent += 1

# Web Scrobbler coming back online with 50 queued listens, one of them broken.
batch = [webscrobbler(now - 90000 - j * 200, *rnd.choice(ANI)) for j in range(50)]
batch[7]["track_metadata"]["artist_name"] = ""
code, out, _ = call("submit import", "POST", "/1/submit-listens", web_token, "Token", {"listen_type": "import", "payload": batch})
expect("web scrobbler import of 50 with one bad", code == 200 and out.get("status") == "ok", f"{code} {out}")
sent += 50

# Pano reads its screens back.
code, out, _ = call("user listens", "GET", f"/1/user/{user}/listens?count=100", pano_token)
expect("pano listens", code == 200 and len(out["payload"]["listens"]) > 0, f"{code}")
latest = out["payload"]["listens"][0] if code == 200 else None
code, out, _ = call("playing-now", "GET", f"/1/user/{user}/playing-now", pano_token)
expect("pano playing-now", code == 200 and out["payload"]["count"] == 1, f"{code} {out}")
code, out, _ = call("listen-count", "GET", f"/1/user/{user}/listen-count", pano_token)
expect("listen count went up", code == 200 and out["payload"]["count"] == start_count + sent, f"{start_count} + {sent} -> {out}")

if "--stats" in opts:
    for kind in ("artists", "releases", "recordings"):
        for rng in ("this_week", "this_month", "this_year", "all_time"):
            code, out, _ = call(f"stats {kind}", "GET", f"/1/stats/user/{user}/{kind}?range={rng}&count=25", pano_token)
            expect(f"stats {kind} {rng}", code in (200, 204), f"{code} {out}")
    code, out, _ = call("stats listening-activity", "GET", f"/1/stats/user/{user}/listening-activity?range=this_year", pano_token)
    expect("stats listening-activity", code in (200, 204), f"{code}")

# Pano deletes the newest listen.
if latest:
    code, out, _ = call("delete-listen", "POST", "/1/delete-listen", pano_token,
                        body={"listened_at": latest["listened_at"], "recording_msid": latest["recording_msid"]})
    expect("pano delete-listen", code == 200 and out.get("status") == "ok", f"{code} {out}")
    code, out, _ = call("listen-count", "GET", f"/1/user/{user}/listen-count", pano_token)
    expect("delete lowers the count", out["payload"]["count"] == start_count + sent - 1, f"{out}")

if "--pages" in opts:
    for path in ("/", "/history", "/top/songs", "/top/artists", "/top/albums", "/top/songs?period=all",
                 "/top/artists?period=all", "/top/albums?period=all", "/top/songs?period=year"):
        for _ in range(3):
            code, _, raw = call("page " + path, "GET", path)
            expect("page " + path, code == 200, f"{code}")

print(f"\n{'request':34} {'n':>4} {'median':>8} {'p95':>8} {'max':>8}  (ms)")
for what, ts in timings.items():
    ts = sorted(ts)
    p95 = ts[min(len(ts) - 1, int(len(ts) * 0.95))]
    print(f"{what:34} {len(ts):4} {statistics.median(ts):8.1f} {p95:8.1f} {ts[-1]:8.1f}")
if failures:
    print(f"\n{len(failures)} problems")
    sys.exit(1)
print("\nall client checks passed")
