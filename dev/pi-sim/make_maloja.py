#!/usr/bin/env python3
"""Build a synthetic Maloja database shaped like an anisong listening history.

Every name is made up from random syllables, so nothing here is anyone's
real data. Usage: make_maloja.py out.sqlite [scrobbles]
"""
import json, random, sqlite3, sys

out = sys.argv[1]
total = int(sys.argv[2]) if len(sys.argv) > 2 else 80000
rnd = random.Random(39)

KANA = "かきくけこさしすせそたちつてとなにぬねのはひふへほまみむめもやゆよらりるれろわん"
ROMA = ["ka", "ki", "ku", "ke", "ko", "sa", "shi", "su", "se", "so", "ta", "chi", "tsu", "te", "to",
        "na", "ni", "nu", "ne", "no", "ha", "hi", "fu", "he", "ho", "ma", "mi", "mu", "me", "mo",
        "ya", "yu", "yo", "ra", "ri", "ru", "re", "ro", "wa", "n"]


def kana(n):
    return "".join(rnd.choice(KANA) for _ in range(n))


def romaji(n):
    return "".join(rnd.choice(ROMA) for _ in range(n)).capitalize()


def person():
    return rnd.choice([kana(2) + kana(3), romaji(2) + " " + romaji(3)])


singers = [person() for _ in range(300)]
groups = [rnd.choice([romaji(3), kana(4), romaji(2) + " Band"]) for _ in range(60)]
characters = [(kana(2) + kana(2), rnd.choice(singers)) for _ in range(150)]
albums = [rnd.choice([kana(5) + " OST", romaji(3) + " Original Soundtrack", kana(4), romaji(4) + " e.p."])
          for _ in range(400)]


def credit():
    r = rnd.random()
    if r < 0.15:
        name, va = rnd.choice(characters)
        return rnd.choice([f"{name} (CV: {va})", f"{name}(CV:{va})", f"{name}（CV：{va}）"])
    if r < 0.30:
        return f"{rnd.choice(groups)} feat. {rnd.choice(singers)}"
    if r < 0.35:
        return f"{rnd.choice(singers)} ft. {rnd.choice(singers)}"
    if r < 0.45:
        return rnd.choice(groups)
    return rnd.choice(singers)


tracks = []
for i in range(4000):
    artist = credit()
    title = rnd.choice([kana(rnd.randint(3, 8)), romaji(rnd.randint(2, 4)), "Main Theme"])
    album = rnd.choice(albums) if rnd.random() < 0.8 else ""
    tracks.append((artist, title, album))
    # Some songs also turn up on a second album, or as a YouTube upload.
    if rnd.random() < 0.08:
        tracks.append((artist, title, rnd.choice(albums)))
    if rnd.random() < 0.05:
        tracks.append((artist, f"【MV】{title} / {artist}", f"{rnd.randint(1, 99)}M plays"))

db = sqlite3.connect(out)
db.executescript("""
CREATE TABLE scrobbles (timestamp INTEGER PRIMARY KEY, rawscrobble VARCHAR, origin VARCHAR, duration INTEGER, track_id INTEGER, extra VARCHAR);
CREATE TABLE tracks (id INTEGER PRIMARY KEY AUTOINCREMENT, title VARCHAR, title_normalized VARCHAR, length INTEGER, album_id INTEGER);
CREATE TABLE artists (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR, name_normalized VARCHAR);
CREATE TABLE albums (id INTEGER PRIMARY KEY AUTOINCREMENT, albtitle VARCHAR, albtitle_normalized VARCHAR);
CREATE TABLE trackartists (id INTEGER PRIMARY KEY, artist_id INTEGER, track_id INTEGER, UNIQUE (artist_id, track_id));
CREATE TABLE albumartists (id INTEGER PRIMARY KEY, artist_id INTEGER, album_id INTEGER, UNIQUE (artist_id, album_id));
CREATE TABLE associated_artists (source_artist INTEGER, target_artist INTEGER, UNIQUE (source_artist, target_artist));
""")
artist_ids, album_ids = {}, {}


def artist_id(name):
    if name not in artist_ids:
        artist_ids[name] = db.execute("INSERT INTO artists (name, name_normalized) VALUES (?, ?)", (name, name.lower())).lastrowid
    return artist_ids[name]


def album_id(name):
    if not name:
        return None
    if name not in album_ids:
        album_ids[name] = db.execute("INSERT INTO albums (albtitle, albtitle_normalized) VALUES (?, ?)", (name, name.lower())).lastrowid
    return album_ids[name]


track_ids = []
for artist, title, album in tracks:
    tid = db.execute("INSERT INTO tracks (title, title_normalized, length, album_id) VALUES (?, ?, ?, ?)",
                     (title, title.lower(), rnd.randint(180, 330), album_id(album))).lastrowid
    # Maloja's own cleaned-up version: artists split up, credits dropped.
    for name in artist.replace(" feat. ", "\n").replace(" ft. ", "\n").split("\n"):
        db.execute("INSERT OR IGNORE INTO trackartists (artist_id, track_id) VALUES (?, ?)", (artist_id(name.split(" (CV")[0]), tid))
    track_ids.append(tid)

# A long tail: a few favourites get most of the plays.
weights = [1 / (i + 1) ** 0.9 for i in range(len(tracks))]
ts = 1_600_000_000
for _ in range(total):
    ts += rnd.randint(150, 2400)
    i = rnd.choices(range(len(tracks)), weights)[0]
    artist, title, album = tracks[i]
    raw = None
    if rnd.random() > 0.1:
        raw = {"track_artists": [artist], "track_title": title, "scrobble_time": ts}
        if album:
            raw["album_title"] = album
        raw = json.dumps(raw, ensure_ascii=False)
    db.execute("INSERT INTO scrobbles VALUES (?, ?, ?, ?, ?, NULL)",
               (ts, raw, rnd.choice(["client:pano", "client:web", None]), rnd.randint(180, 330), track_ids[i]))
db.commit()
print(f"{total} scrobbles of {len(tracks)} tracks, last at {ts}")
