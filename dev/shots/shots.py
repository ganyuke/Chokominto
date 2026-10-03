# Screenshots of a throwaway Chokominto with demo listens, for checking how
# pages look. Runs inside the image from dev/shots/Containerfile with
# no network: see run.sh and .claude/skills/screenshots/SKILL.md.
#
# Usage: shots.py <step> ...
#   Each argument is one screenshot: a path, then optional actions joined
#   by " | ":
#     click:<selector>          click something
#     fill:<selector>=<text>    type into a field
#     select:<selector>=<value> pick an option
#     upload:<selector>=<file>  choose a file (relative to /work)
#     playing:<artist>=<title>=<album>  tell the server what's playing now
#     wait:<seconds>            let the page update itself
#     connect:<name>=<redirect> an agent registers and asks to connect
#     mcp:<tool>=<json args>    an agent (chokominto mcp) calls a tool
#   A path with "#id" is cropped to that section, from its heading to the
#   next h2. Screenshots are written to /out/00.png, 01.png, ...
#
# Environment: LISTENS (demo listens JSON, default /work/listens.json),
# WIDTH (default 1280).
import json, os, re, subprocess, sys, time, urllib.parse, urllib.request
from playwright.sync_api import sync_playwright

BIN, DATA, WORK, OUT = "/out/chokominto", "/tmp/data", "/work", "/out"
URL = "http://127.0.0.1:3939"
env = dict(os.environ, CHOKOMINTO_DATA_DIR=DATA, CHOKOMINTO_LISTEN="127.0.0.1:3939", CHOKOMINTO_PASSWORD="miku3939")
os.makedirs(DATA, exist_ok=True)

# The first start makes the account, then a token, the way the README
# sets them up.
server = subprocess.Popen([BIN, "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
for _ in range(50):
    try:
        urllib.request.urlopen(URL + "/healthz")
        break
    except Exception:
        time.sleep(0.2)
out = subprocess.run([BIN, "token", "add", "ruby", "Pano"], env=env, check=True, capture_output=True, text=True).stdout
token = re.search(r"\n  (\S+)\n", out).group(1)

# Demo listens through the ListenBrainz API, ten minutes apart, newest
# first, then a moment for background linking.
listens = json.load(open(os.environ.get("LISTENS", WORK + "/listens.json")))
now = int(time.time())
payload = []
for i, (artist, title, album) in enumerate(listens):
    meta = {"artist_name": artist, "track_name": title}
    if album:
        meta["release_name"] = album
    payload.append({"listened_at": now - 600 * (i + 1), "track_metadata": meta})
req = urllib.request.Request(URL + "/1/submit-listens", data=json.dumps({"listen_type": "import", "payload": payload}).encode(),
                             headers={"Authorization": "Token " + token, "Content-Type": "application/json"})
urllib.request.urlopen(req)
time.sleep(3)

width = int(os.environ.get("WIDTH", "1280"))
with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={"width": width, "height": 900})
    page.goto(URL + "/login")
    page.fill("#name", "ruby")
    page.fill("#password", "miku3939")
    page.click("button[type=submit]")
    for n, arg in enumerate(sys.argv[1:]):
        steps = arg.split(" | ")
        page.goto(URL + steps[0])
        for step in steps[1:]:
            kind, _, rest = step.partition(":")
            sel, _, val = rest.partition("=")
            if kind == "click":
                page.click(rest)
            elif kind == "fill":
                page.fill(sel, val)
            elif kind == "select":
                page.select_option(sel, val)
            elif kind == "upload":
                page.set_input_files(sel, WORK + "/" + val)
            elif kind == "playing":
                artist, title, album = (rest.split("=") + ["", ""])[:3]
                meta = {"artist_name": artist, "track_name": title}
                if album:
                    meta["release_name"] = album
                urllib.request.urlopen(urllib.request.Request(URL + "/1/submit-listens",
                    data=json.dumps({"listen_type": "playing_now", "payload": [{"track_metadata": meta}]}).encode(),
                    headers={"Authorization": "Token " + token, "Content-Type": "application/json"}))
            elif kind == "connect":
                reg = urllib.request.urlopen(urllib.request.Request(URL + "/oauth/register",
                    data=json.dumps({"client_name": sel, "redirect_uris": [val]}).encode(),
                    headers={"Content-Type": "application/json"}))
                client = json.load(reg)["client_id"]
                q = urllib.parse.urlencode({"response_type": "code", "client_id": client, "redirect_uri": val, "state": "s",
                    "code_challenge": "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGrSstw-cM", "code_challenge_method": "S256"})
                page.goto(URL + "/oauth/authorize?" + q)
            elif kind == "mcp":
                msgs = [{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "shots", "version": "1"}}},
                        {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": sel, "arguments": json.loads(val or "{}")}}]
                subprocess.run([BIN, "mcp"], env=env, input="".join(json.dumps(m) + "\n" for m in msgs), capture_output=True, text=True, check=True)
                page.reload()
            elif kind == "wait":
                page.wait_for_timeout(float(rest) * 1000)
            else:
                sys.exit("unknown step " + step)
        shot = f"{OUT}/{n:02d}.png"
        frag = steps[0].partition("#")[2]
        if frag:
            top = page.eval_on_selector("#" + frag, "e => e.getBoundingClientRect().top + window.scrollY")
            bottom = page.evaluate("t => { for (const h of document.querySelectorAll('h2')) {"
                                   " const y = h.getBoundingClientRect().top + window.scrollY; if (y > t + 5) return y }"
                                   " return document.body.scrollHeight }", top)
            page.screenshot(path=shot, full_page=True, clip={"x": 0, "y": top - 10, "width": width, "height": bottom - top})
        else:
            page.screenshot(path=shot, full_page=True)
        print(n, page.url)
    browser.close()
server.terminate()
