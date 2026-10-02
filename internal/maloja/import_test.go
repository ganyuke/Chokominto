package maloja

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chokominto/internal/auth"
	"chokominto/internal/store"
)

// Maloja's schema, from maloja/database/sqldb.py (DBTABLES).
const malojaSchema = `
CREATE TABLE scrobbles (timestamp INTEGER PRIMARY KEY, rawscrobble VARCHAR, origin VARCHAR, duration INTEGER, track_id INTEGER, extra VARCHAR);
CREATE TABLE tracks (id INTEGER PRIMARY KEY AUTOINCREMENT, title VARCHAR, title_normalized VARCHAR, length INTEGER, album_id INTEGER);
CREATE TABLE artists (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR, name_normalized VARCHAR);
CREATE TABLE albums (id INTEGER PRIMARY KEY AUTOINCREMENT, albtitle VARCHAR, albtitle_normalized VARCHAR);
CREATE TABLE trackartists (id INTEGER PRIMARY KEY, artist_id INTEGER, track_id INTEGER, UNIQUE (artist_id, track_id));
CREATE TABLE albumartists (id INTEGER PRIMARY KEY, artist_id INTEGER, album_id INTEGER, UNIQUE (artist_id, album_id));
CREATE TABLE associated_artists (source_artist INTEGER, target_artist INTEGER, UNIQUE (source_artist, target_artist));
`

func makeMaloja(t *testing.T, schema string, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "malojadb.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range append([]string{schema}, stmts...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return path
}

func openStore(t *testing.T) (*store.DB, int64) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(context.Background(), "elaina", "h")
	return db, u
}

func TestImport(t *testing.T) {
	ctx := context.Background()
	path := makeMaloja(t, malojaSchema,
		`INSERT INTO artists VALUES (1, 'Kessoku Band', 'kessoku band'), (2, 'Hitori Gotoh', 'hitori gotoh'), (3, 'YOASOBI', 'yoasobi')`,
		`INSERT INTO albums VALUES (1, 'Kessoku Band', 'kessoku band')`,
		`INSERT INTO tracks VALUES (1, 'Guitar, Loneliness and Blue Planet', '', 229, 1), (2, 'Idol', '', 213, NULL)`,
		`INSERT INTO trackartists VALUES (1, 2, 1), (2, 1, 1), (3, 3, 2)`,
		// From the ListenBrainz API: the artist string exactly as sent.
		`INSERT INTO scrobbles VALUES (1700000000, '{"track_artists": ["結束バンド"], "track_title": "ギターと孤独と蒼い惑星", "album_title": "結束バンド", "scrobble_time": 1700000000}', 'client:pano', 229, 1, NULL)`,
		// From the native API: a list of artists.
		`INSERT INTO scrobbles VALUES (1700000300, '{"track_artists": ["YOASOBI", "Ayase"], "track_title": "アイドル"}', 'client:web', 213, 2, NULL)`,
		// Old or imported: no raw scrobble.
		`INSERT INTO scrobbles VALUES (1700000600, NULL, 'import:lastfm', NULL, 1, NULL)`,
	)
	db, u := openStore(t)
	var calls int
	res, err := Import(ctx, db, u, path, func(done, total int) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 || res.Stored != 3 || res.Reconstructed != 1 || calls == 0 {
		t.Fatalf("%+v calls=%d", res, calls)
	}
	ls, _ := db.Listens(ctx, u, store.ListenRange{Limit: 10})
	got := map[int64]store.Listen{}
	for _, l := range ls {
		got[l.ListenedAt] = l
		if l.Origin != "import:maloja" {
			t.Errorf("origin %s", l.Origin)
		}
	}
	if l := got[1700000000]; l.Artist != "結束バンド" || l.Title != "ギターと孤独と蒼い惑星" || l.Album != "結束バンド" {
		t.Errorf("raw LB scrobble: %+v", l)
	}
	if l := got[1700000300]; l.Artist != "YOASOBI, Ayase" || l.Album != "" {
		t.Errorf("raw list scrobble: %+v", l)
	}
	if l := got[1700000600]; l.Artist != "Hitori Gotoh, Kessoku Band" || l.Title != "Guitar, Loneliness and Blue Planet" || l.Album != "Kessoku Band" {
		t.Errorf("reconstructed: %+v", l)
	}
	var payload string
	db.Reader().QueryRow(`SELECT p.payload FROM listen_payloads p JOIN listens l ON l.id = p.listen_id WHERE l.listened_at = 1700000600`).Scan(&payload)
	if payload == "" || !strings.Contains(payload, `"reconstructed_from":"maloja"`) {
		t.Errorf("reconstructed payload: %s", payload)
	}

	// Running it again adds nothing.
	res, err = Import(ctx, db, u, path, nil)
	if err != nil || res.Stored != 0 || res.Duplicates != 3 {
		t.Fatalf("re-run: %+v %v", res, err)
	}
}

func TestImportOldMalojaWithoutAlbums(t *testing.T) {
	path := makeMaloja(t, `
CREATE TABLE scrobbles (timestamp INTEGER PRIMARY KEY, rawscrobble VARCHAR, origin VARCHAR, duration INTEGER, track_id INTEGER, extra VARCHAR);
CREATE TABLE tracks (id INTEGER PRIMARY KEY, title VARCHAR, title_normalized VARCHAR, length INTEGER);
CREATE TABLE artists (id INTEGER PRIMARY KEY, name VARCHAR, name_normalized VARCHAR);
CREATE TABLE trackartists (id INTEGER PRIMARY KEY, artist_id INTEGER, track_id INTEGER);`,
		`INSERT INTO artists VALUES (1, 'supercell', '')`,
		`INSERT INTO tracks VALUES (1, 'World Is Mine', '', 0)`,
		`INSERT INTO trackartists VALUES (1, 1, 1)`,
		`INSERT INTO scrobbles VALUES (1600000000, NULL, NULL, NULL, 1, NULL)`,
	)
	db, u := openStore(t)
	res, err := Import(context.Background(), db, u, path, nil)
	if err != nil || res.Stored != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestImportRejectsOtherFiles(t *testing.T) {
	path := makeMaloja(t, `CREATE TABLE notes (x TEXT)`)
	db, u := openStore(t)
	if _, err := Import(context.Background(), db, u, path, nil); err == nil {
		t.Fatal("non-Maloja database accepted")
	}
	if _, err := Import(context.Background(), db, u, filepath.Join(t.TempDir(), "missing.sqlite"), nil); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestImportLeavesMalojaUntouched(t *testing.T) {
	path := makeMaloja(t, malojaSchema, `INSERT INTO scrobbles VALUES (1700000000, NULL, NULL, NULL, NULL, NULL)`)
	before, _ := os.ReadFile(path)
	db, u := openStore(t)
	Import(context.Background(), db, u, path, nil)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("Maloja database was modified")
	}
}

func TestImportAPIKeys(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "apikeys.yml")
	os.WriteFile(p, []byte("pano: abc123secret\nweb scrobbler: def456secret\nempty: ''\n"), 0o600)
	db, u := openStore(t)
	res, err := ImportAPIKeys(ctx, db, u, p)
	if err != nil || res.Added != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, uid, err := db.TokenUser(ctx, auth.HashSecret("abc123secret")); err != nil || uid != u {
		t.Fatal("imported key doesn't work")
	}
	res, _ = ImportAPIKeys(ctx, db, u, p)
	if res.Added != 0 || res.Skipped != 2 {
		t.Fatalf("re-run %+v", res)
	}
}
