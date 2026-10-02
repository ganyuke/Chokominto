package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(context.Background(), filepath.Join(dir, "test.db"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testUser(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.CreateUser(context.Background(), "elaina", "hash")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigrateAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "c.db")
	db, err := Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	var v int
	db.Reader().QueryRow("PRAGMA user_version").Scan(&v)
	if v != SchemaVersion() {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion())
	}
	var mode string
	db.w.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %s", mode)
	}
	db.Close()

	// Reopening an up-to-date database changes nothing and takes no backup.
	db, err = Open(ctx, path, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup dir: %v", err)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	db := openTest(t)
	if _, err := db.Reader().Exec(`INSERT INTO users (name, password_hash, created_at) VALUES ('x', 'y', 0)`); err == nil {
		t.Fatal("write through the read pool succeeded")
	}
}

func TestBackupAndPrune(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	testUser(t, db)
	dir := t.TempDir()
	for _, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
		if _, err := db.Backup(ctx, dir, day); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "chokominto-before-v2.db"), nil, 0o600)
	if err := PruneBackups(dir, 2); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	want := map[string]bool{"chokominto-2026-09-02.db": true, "chokominto-2026-09-03.db": true, "chokominto-before-v2.db": true}
	if len(left) != len(want) {
		t.Fatalf("left %v", left)
	}
	for _, f := range left {
		if !want[filepath.Base(f)] {
			t.Fatalf("unexpected %s", f)
		}
	}
	// A backup is a working database.
	b, err := Open(ctx, filepath.Join(dir, "chokominto-2026-09-03.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.UserByName(ctx, "elaina"); err != nil {
		t.Fatal(err)
	}
}

func TestInsertListensDuplicates(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	ls := []NewListen{
		{ListenedAt: 1000, Artist: "YOASOBI", Title: "アイドル", Payload: []byte(`{}`)},
		{ListenedAt: 1000, Artist: "Alya", Title: "World Is Mine", Payload: []byte(`{}`)}, // same second, different song
		{ListenedAt: 1000, Artist: "YOASOBI", Title: "アイドル", Payload: []byte(`{}`)},       // duplicate in the same batch
	}
	res, err := db.InsertListens(ctx, u, "listenbrainz", nil, ls)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 2 || res.Duplicates != 1 {
		t.Fatalf("got %+v", res)
	}
	// Retrying the whole batch stores nothing new.
	res, _ = db.InsertListens(ctx, u, "listenbrainz", nil, ls)
	if res.Stored != 0 || res.Duplicates != 3 {
		t.Fatalf("retry got %+v", res)
	}
	n, oldest, newest, _ := db.ListenStats(ctx, u)
	if n != 2 || oldest != 1000 || newest != 1000 {
		t.Fatalf("stats %d %d %d", n, oldest, newest)
	}
	var payloads int
	db.Reader().QueryRow(`SELECT count(*) FROM listen_payloads`).Scan(&payloads)
	if payloads != 2 {
		t.Fatalf("payloads = %d", payloads)
	}
}

func TestListensPaging(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	var ls []NewListen
	for i := range 10 {
		ls = append(ls, NewListen{ListenedAt: int64(100 + i/2), Artist: "A", Title: string(rune('a' + i)), Payload: []byte(`{}`)})
	}
	db.InsertListens(ctx, u, "listenbrainz", nil, ls)

	first, _ := db.Listens(ctx, u, ListenRange{Limit: 4})
	if len(first) != 4 || first[0].Title != "j" || first[3].Title != "g" {
		t.Fatalf("first page %v", titles(first))
	}
	last := first[len(first)-1]
	next, _ := db.Listens(ctx, u, ListenRange{Before: &Cursor{last.ListenedAt, last.ID}, Limit: 4})
	if titles(next) != "fedc" {
		t.Fatalf("second page %s", titles(next))
	}
	top := next[0]
	back, _ := db.Listens(ctx, u, ListenRange{After: &Cursor{top.ListenedAt, top.ID}, Oldest: true, Limit: 4})
	if titles(back) != "jihg" {
		t.Fatalf("back page %s", titles(back))
	}
	// Time-only bounds: strictly before 102, strictly after 100.
	mid, _ := db.Listens(ctx, u, ListenRange{Before: ptr(CursorAt(102)), After: ptr(CursorAfter(100)), Limit: 10})
	if titles(mid) != "dc" {
		t.Fatalf("bounded %s", titles(mid))
	}
}

func ptr[T any](v T) *T { return &v }

func titles(ls []Listen) string {
	s := ""
	for _, l := range ls {
		s += l.Title
	}
	return s
}

func TestDeleteUndoRedo(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	db.InsertListens(ctx, u, "listenbrainz", nil, []NewListen{{ListenedAt: 500, Artist: "A", Title: "T", Payload: []byte(`{}`)}})
	ls, _ := db.Listens(ctx, u, ListenRange{Limit: 1})
	l, err := db.FindListen(ctx, u, 500, ls[0].MSID)
	if err != nil {
		t.Fatal(err)
	}

	del, err := db.DeleteListen(ctx, u, l)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _, _ := db.ListenStats(ctx, u); n != 0 {
		t.Fatal("listen still visible after delete")
	}
	if _, err := db.FindListen(ctx, u, 500, l.MSID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted listen still findable")
	}
	// Deleting it again fails cleanly, the row no longer matches.
	if _, err := db.DeleteListen(ctx, u, l); !errors.Is(err, ErrStale) {
		t.Fatalf("second delete: %v", err)
	}

	undo, err := db.UndoEdit(ctx, u, del)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _, _ := db.ListenStats(ctx, u); n != 1 {
		t.Fatal("listen not back after undo")
	}
	if _, err := db.UndoEdit(ctx, u, del); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("double undo: %v", err)
	}

	// Undoing the undo deletes it again and puts the original back in effect.
	if _, err := db.UndoEdit(ctx, u, undo); err != nil {
		t.Fatal(err)
	}
	if n, _, _, _ := db.ListenStats(ctx, u); n != 0 {
		t.Fatal("redo did not delete")
	}
	es, _ := db.Edits(ctx, u, 0, 10)
	if len(es) != 3 || es[0].Summary != "Redo: Deleted listen: T by A" || es[2].UndoneAt.Valid {
		t.Fatalf("edits %+v", es)
	}
}

// The stored count must always equal a real count, through every path.
func TestListenCountStaysExact(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	other, _ := db.CreateUser(ctx, "other", "h")
	check := func(when string) {
		t.Helper()
		var real int
		db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE user_id = ? AND deleted_by IS NULL`, u).Scan(&real)
		n, _, _, err := db.ListenStats(ctx, u)
		if err != nil || n != real {
			t.Fatalf("%s: stored %d, real %d (%v)", when, n, real, err)
		}
	}
	check("empty")
	ls := []NewListen{{ListenedAt: 1, Artist: "A", Title: "1", Payload: []byte(`{}`)}, {ListenedAt: 2, Artist: "A", Title: "2", Payload: []byte(`{}`)}}
	db.InsertListens(ctx, u, "listenbrainz", nil, ls)
	db.InsertListens(ctx, u, "listenbrainz", nil, ls) // duplicates
	db.InsertListens(ctx, other, "listenbrainz", nil, ls)
	check("after inserts")
	got, _ := db.Listens(ctx, u, ListenRange{Limit: 1})
	del, _ := db.DeleteListen(ctx, u, got[0])
	check("after delete")
	undo, _ := db.UndoEdit(ctx, u, del)
	check("after undo")
	db.UndoEdit(ctx, u, undo)
	check("after redo")
	if n, _, _, _ := db.ListenStats(ctx, other); n != 2 {
		t.Fatalf("other user count %d", n)
	}
}

func TestUndoConflict(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	db.InsertListens(ctx, u, "listenbrainz", nil, []NewListen{{ListenedAt: 500, Artist: "A", Title: "T", Payload: []byte(`{}`)}})
	ls, _ := db.Listens(ctx, u, ListenRange{Limit: 1})
	del, _ := db.DeleteListen(ctx, u, ls[0])

	// A later edit restores the listen by some other route.
	restore, err := db.ApplyEdit(ctx, u, "test", "restore", func(int64) []Change {
		return []Change{{Table: "listens", ID: ls[0].ID, Before: map[string]any{"deleted_by": del}, After: map[string]any{"deleted_by": nil}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UndoEdit(ctx, u, del)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Later) != 1 || ce.Later[0] != restore {
		t.Fatalf("got %v", err)
	}
}

func TestEditWhitelist(t *testing.T) {
	db := openTest(t)
	u := testUser(t, db)
	_, err := db.ApplyEdit(context.Background(), u, "x", "x", func(int64) []Change {
		return []Change{{Table: "users", ID: u, Before: map[string]any{"name": "elaina"}, After: map[string]any{"name": "x"}}}
	})
	if err == nil {
		t.Fatal("edit to a non-whitelisted table succeeded")
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	base := time.Unix(1_800_000_000, 0)
	now = func() time.Time { return base }
	t.Cleanup(func() { now = time.Now })

	h := []byte("session-hash")
	db.CreateSession(ctx, u, h)
	if got, err := db.SessionUser(ctx, h); err != nil || got != u {
		t.Fatalf("got %d %v", got, err)
	}
	// Two days later the session slides forward.
	now = func() time.Time { return base.Add(48 * time.Hour) }
	db.SessionUser(ctx, h)
	now = func() time.Time { return base.Add(SessionLifetime + time.Hour) }
	if _, err := db.SessionUser(ctx, h); err != nil {
		t.Fatalf("session did not slide: %v", err)
	}
	now = func() time.Time { return base.Add(3 * SessionLifetime) }
	if _, err := db.SessionUser(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session accepted")
	}
	// Changing the password signs out every session.
	now = func() time.Time { return base }
	db.CreateSession(ctx, u, []byte("other"))
	db.SetPassword(ctx, u, "new")
	if _, err := db.SessionUser(ctx, []byte("other")); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived password change")
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	id, _ := db.CreateToken(ctx, u, "Pano Scrobbler", []byte("h1"))
	if _, err := db.CreateToken(ctx, u, "dup", []byte("h1")); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("dup token: %v", err)
	}
	tid, uid, err := db.TokenUser(ctx, []byte("h1"))
	if err != nil || tid != id || uid != u {
		t.Fatal(err)
	}
	db.RevokeToken(ctx, u, id)
	if _, _, err := db.TokenUser(ctx, []byte("h1")); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked token accepted")
	}
}

// A backup must not hold up writes: it runs on the read pool.
func TestBackupDoesNotBlockWrites(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	var ls []NewListen
	for i := range 20000 {
		ls = append(ls, NewListen{ListenedAt: int64(i), Artist: "A", Title: "T", Payload: []byte(`{"padding":"` + string(make([]byte, 200)) + `"}`)})
	}
	db.InsertListens(ctx, u, "listenbrainz", nil, ls)
	done := make(chan error, 1)
	go func() {
		_, err := db.Backup(ctx, t.TempDir(), "x")
		done <- err
	}()
	start := time.Now()
	if _, err := db.InsertListens(ctx, u, "listenbrainz", nil, []NewListen{{ListenedAt: 999999, Artist: "B", Title: "T", Payload: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	writeTook := time.Since(start)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if writeTook > 500*time.Millisecond {
		t.Fatalf("a write waited %v for the backup", writeTook)
	}
}

func TestUpdateLabelAndUndo(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	ls, _ := db.Labels(ctx, u)
	if len(ls) != 1 || ls[0].Name != "Character" || !ls[0].HideDefault {
		t.Fatalf("default labels %+v", ls)
	}
	id := ls[0].ID
	if err := db.UpdateLabel(ctx, u, id, "Characters", false); err != nil {
		t.Fatal(err)
	}
	ls, _ = db.Labels(ctx, u)
	if ls[0].Name != "Characters" || ls[0].HideDefault {
		t.Fatalf("after update %+v", ls[0])
	}
	es, _ := db.Edits(ctx, u, 0, 1)
	if es[0].Summary != "Renamed label Character to Characters. Label Characters now shown in rankings by default" {
		t.Fatal(es[0].Summary)
	}
	if _, err := db.UndoEdit(ctx, u, es[0].ID); err != nil {
		t.Fatal(err)
	}
	ls, _ = db.Labels(ctx, u)
	if ls[0].Name != "Character" || !ls[0].HideDefault {
		t.Fatalf("after undo %+v", ls[0])
	}
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO labels (user_id, name) VALUES (?, 'Vocaloid')`, u)
		return err
	})
	if err := db.UpdateLabel(ctx, u, id, "Vocaloid", true); !errors.Is(err, ErrLabelName) {
		t.Fatalf("duplicate name: %v", err)
	}
}

// A panic inside a write rolls it back, so later writes aren't stuck
// behind it.
func TestWriteRollsBackOnPanic(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	func() {
		defer func() { recover() }()
		db.Write(ctx, func(tx *sql.Tx) error {
			tx.Exec(`INSERT INTO users (name, password_hash, created_at) VALUES ('half', 'h', 0)`)
			panic("boom")
		})
	}()
	done := make(chan error, 1)
	go func() { _, err := db.CreateUser(ctx, "next", "h"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writes are stuck after a panic")
	}
	if _, err := db.UserByName(ctx, "half"); err == nil {
		t.Fatal("the panicking write was kept")
	}
}
