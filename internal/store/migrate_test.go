package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"testing"
)

// openAtVersion creates a database migrated only up to version v, so a
// later migration can be tested against data written by older code.
func openAtVersion(t *testing.T, path string, v int) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:v] {
		if _, err := raw.Exec(m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMigrateAlbumArtist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")
	raw := openAtVersion(t, path, 2)
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := raw.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	u := exec(`INSERT INTO users (name, password_hash, created_at) VALUES ('elaina', 'h', 0)`)
	song := exec(`INSERT INTO songs (user_id, name, created_at) VALUES (?, 'コネクト', 0)`, u)
	rec := exec(`INSERT INTO recordings (user_id, song_id, created_at) VALUES (?, ?, 0)`, u, song)
	auto := exec(`INSERT INTO edits (user_id, kind, summary, automatic, created_at) VALUES (?, 'link', 'Linked', 1, 0)`, u)
	owner := exec(`INSERT INTO edits (user_id, kind, summary, automatic, created_at) VALUES (?, 'link', 'Linked', 0, 0)`, u)

	source := func(title string, linkedBy any) int64 {
		var recID any
		if linkedBy != nil {
			recID = rec
		}
		return exec(`INSERT INTO sources (user_id, artist_text, title_text, album_text, msid, recording_id, linked_by) VALUES (?, 'ClariS', ?, 'OST', ?, ?, ?)`,
			u, title, "msid-"+title, recID, linkedBy)
	}
	ts := int64(1_700_000_000)
	listen := func(src int64, info string) {
		ts += 60
		var recID any
		raw.QueryRow(`SELECT recording_id FROM sources WHERE id = ?`, src).Scan(&recID)
		l := exec(`INSERT INTO listens (user_id, listened_at, source_id, origin, received_at, recording_id) VALUES (?, ?, ?, 'listenbrainz', 0, ?)`, u, ts, src, recID)
		exec(`INSERT INTO listen_payloads (listen_id, payload) VALUES (?, ?)`, l,
			`{"track_metadata":{"artist_name":"ClariS","additional_info":`+info+`}}`)
	}
	const va = `{"release_artist_name":"Various Artists"}`
	// A: first listen without an album artist, then two with. Linked
	// automatically.
	a := source("a", auto)
	listen(a, `{}`)
	listen(a, va)
	listen(a, va)
	// B: the other way round, with its link in the edit log like the
	// resolver records it.
	autoB := exec(`INSERT INTO edits (user_id, kind, summary, automatic, created_at) VALUES (?, 'link', 'Linked', 1, 0)`, u)
	b := source("b", autoB)
	exec(`INSERT INTO edit_changes (edit_id, seq, op, tbl, row_key, before, after) VALUES (?, 0, 'update', 'sources', ?, ?, ?)`,
		autoB, fmt.Sprintf(`{"id":%d}`, b), `{"linked_by":null,"recording_id":null,"release_id":null}`,
		fmt.Sprintf(`{"linked_by":%d,"recording_id":%d,"release_id":null}`, autoB, rec))
	listen(b, va)
	listen(b, `{}`)
	// C: a value of the wrong type counts as none.
	c := source("c", nil)
	listen(c, `{"release_artist_name":5}`)
	// D: linked by the owner, keeps its link.
	d := source("d", owner)
	listen(d, va)
	// E: no listens.
	source("e", nil)
	raw.Close()

	db, err := Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type row struct {
		id     int64
		aa     string
		msid   string
		linked bool
		n      int
	}
	got := map[string][]row{}
	rows, err := db.Reader().Query(`SELECT s.id, s.title_text, s.album_artist_text, s.msid, s.recording_id IS NOT NULL,
		(SELECT count(*) FROM listens l WHERE l.source_id = s.id) FROM sources s ORDER BY s.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r row
		var title string
		rows.Scan(&r.id, &title, &r.aa, &r.msid, &r.linked, &r.n)
		got[title] = append(got[title], r)
	}
	rows.Close()

	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	check := func(title string, want ...row) {
		t.Helper()
		g := got[title]
		if len(g) != len(want) {
			t.Fatalf("%s: %+v, want %+v", title, g, want)
		}
		for i, w := range want {
			if g[i].aa != w.aa || g[i].linked != w.linked || g[i].n != w.n {
				t.Errorf("%s[%d] = %+v, want %+v", title, i, g[i], w)
			}
			if i == 0 && (g[i].id != w.id || g[i].msid != "msid-"+title) {
				t.Errorf("%s: first received text lost its row: %+v", title, g[i])
			}
			if i > 0 && !uuid.MatchString(g[i].msid) {
				t.Errorf("%s: new msid %q", title, g[i].msid)
			}
		}
	}
	check("a", row{id: a, aa: "", linked: true, n: 1}, row{aa: "Various Artists", n: 2})
	check("b", row{id: b, aa: "Various Artists", n: 1}, row{aa: "", n: 1})
	check("c", row{id: c, aa: "", n: 1})
	check("d", row{id: d, aa: "Various Artists", linked: true, n: 1})
	check("e", row{id: got["e"][0].id, aa: ""})

	// Listens follow their text's link, and counts stay exact.
	var stray, total, counted int
	db.Reader().QueryRow(`SELECT count(*) FROM listens l JOIN sources s ON s.id = l.source_id
		WHERE l.recording_id IS NOT s.recording_id`).Scan(&stray)
	db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE recording_id IS NOT NULL`).Scan(&counted)
	db.Reader().QueryRow(`SELECT coalesce(sum(n), 0) FROM listen_totals`).Scan(&total)
	if stray != 0 || total != counted || counted != 2 {
		t.Fatalf("stray links %d, totals %d, linked listens %d (want 2)", stray, total, counted)
	}

	// Everything unlinked is queued for linking.
	var unqueued int
	db.Reader().QueryRow(`SELECT count(*) FROM sources WHERE recording_id IS NULL
		AND 'source:' || id NOT IN (SELECT key FROM jobs WHERE kind = 'resolve')`).Scan(&unqueued)
	if unqueued != 0 {
		t.Fatalf("%d unlinked sources not queued", unqueued)
	}

	// The old automatic link on B can't be undone over the new state.
	if _, err := db.UndoEdit(ctx, u, autoB); err == nil {
		t.Fatal("undoing a link the migration dropped went through")
	}

	// Foreign keys are enforced again afterwards.
	var fk int
	db.w.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys left off")
	}
	if _, err := db.InsertListens(ctx, u, "listenbrainz", nil, []NewListen{
		{ListenedAt: ts + 60, Artist: "ClariS", Title: "a", Album: "OST", AlbumArtist: "Various Artists", Payload: []byte(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	var sa int
	db.Reader().QueryRow(`SELECT count(*) FROM sources WHERE title_text = 'a'`).Scan(&sa)
	if sa != 2 {
		t.Fatalf("a listen with known text made new text: %d rows for a", sa)
	}
}

func TestMigrateLabelReferences(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v3.db")
	raw := openAtVersion(t, path, 3)
	for _, q := range []string{
		`INSERT INTO users (id, name, password_hash, created_at) VALUES (1, 'elaina', 'h', 0)`,
		`INSERT INTO labels (id, user_id, name, hide_default) VALUES (5, 1, 'Vocaloid', 0)`,
		`INSERT INTO entity_labels VALUES (5, 'artist', 10), (5, 'song', 11)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	db, err := Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	db.Reader().QueryRow(`SELECT count(*) FROM entity_labels WHERE label_id = 5`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d labelled things kept, want 2", n)
	}
	// A label still in use can't be removed behind the edit log's back.
	err = db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM labels WHERE id = 5`)
		return err
	})
	if err == nil {
		t.Fatal("removing a label in use took it off things silently")
	}
	if _, err := db.DeleteLabel(ctx, 1, 5); err != nil {
		t.Fatal(err)
	}
}

// Labels keep the order they were listed in before they had one: by name.
func TestMigrateLabelOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v4.db")
	raw := openAtVersion(t, path, 4)
	for _, q := range []string{
		`INSERT INTO users (id, name, password_hash, created_at) VALUES (1, 'elaina', 'h', 0), (2, 'someone', 'h', 0)`,
		`INSERT INTO labels (user_id, name) VALUES (1, 'Vocaloid'), (1, 'Game'), (2, 'Anime')`,
		`INSERT INTO artists (id, user_id, name, created_at) VALUES (1, 1, '結束バンド', 0)`,
		`INSERT INTO artist_aliases (artist_id, name, lang, match_key) VALUES (1, '結束バンド', 'original', '結束ばんど')`,
		`INSERT INTO sources (user_id, artist_text, title_text, album_text, msid) VALUES (1, 'ＹＯＡＳＯＢＩ', 'アイドル', 'THE BOOK 3', 'm1')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	db, err := Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got []string
	for _, u := range []int64{1, 2} {
		ls, err := db.Labels(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			var pos int
			db.Reader().QueryRow(`SELECT position FROM labels WHERE id = ?`, l.ID).Scan(&pos)
			got = append(got, fmt.Sprint(l.Name, pos))
		}
	}
	if want := "[Character0 Game1 Vocaloid2 Anime0 Character1]"; fmt.Sprint(got) != want {
		t.Fatalf("labels %v, want %s", got, want)
	}
	// Names already there get their readings guessed in the background.
	var jobs []string
	rows, _ := db.Reader().Query(`SELECT key FROM jobs WHERE kind = 'guess_keys' ORDER BY key`)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		jobs = append(jobs, k)
	}
	rows.Close()
	if fmt.Sprint(jobs) != "[artist_aliases:0]" {
		t.Fatalf("guess jobs %v", jobs)
	}
	// Older text is searched the same way as new text.
	var key string
	db.Reader().QueryRow(`SELECT search_key FROM sources WHERE msid = 'm1'`).Scan(&key)
	if want := SearchKey(SourceText{Artist: "ＹＯＡＳＯＢＩ", Title: "アイドル", Album: "THE BOOK 3"}); key != want {
		t.Fatalf("search key %q, want %q", key, want)
	}
}

// Existing users get the comma and slash split rules once. New users get
// them from the start.
func TestMigrateListSplits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v5.db")
	raw := openAtVersion(t, path, 5)
	if _, err := raw.Exec(`INSERT INTO users (id, name, password_hash, created_at) VALUES (1, 'elaina', 'h', 0)`); err != nil {
		t.Fatal(err)
	}
	// The owner already added one of them by hand.
	if _, err := raw.Exec(`INSERT INTO rules (user_id, kind, field, match_mode, pattern, created_at) VALUES (1, 'split', 'artist', 'exact', ', ', 0)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := db.CreateUser(ctx, "someone", "h")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []int64{1, other} {
		var commas, slashes int
		db.Reader().QueryRow(`SELECT count(*) FROM rules WHERE user_id = ? AND pattern = ', '`, u).Scan(&commas)
		db.Reader().QueryRow(`SELECT count(*) FROM rules WHERE user_id = ? AND pattern = ' / '`, u).Scan(&slashes)
		if commas != 1 || slashes != 1 {
			t.Errorf("user %d has %d comma and %d slash rules", u, commas, slashes)
		}
	}
}

func TestNeedsUpdate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if need, err := NeedsUpdate(ctx, filepath.Join(dir, "none.db")); need || err != nil {
		t.Fatalf("missing file: %v %v", need, err)
	}
	old := filepath.Join(dir, "old.db")
	openAtVersion(t, old, 1).Close()
	if need, err := NeedsUpdate(ctx, old); !need || err != nil {
		t.Fatalf("old file: %v %v", need, err)
	}
	db, err := Open(ctx, old, "")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if need, err := NeedsUpdate(ctx, old); need || err != nil {
		t.Fatalf("after update: %v %v", need, err)
	}
}

// Two processes opening a new database at once, like the service starting
// while the first account is created, must both succeed.
func TestOpenConcurrently(t *testing.T) {
	ctx := context.Background()
	for i := range 10 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("new%d.db", i))
		errs := make(chan error, 3)
		for range 3 {
			go func() {
				db, err := Open(ctx, path, "")
				if err == nil {
					db.Close()
				}
				errs <- err
			}()
		}
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
	}
}
