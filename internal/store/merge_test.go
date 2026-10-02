package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// music is a small library for merge tests:
//
//	ＹＯＡＳＯＢＩ - アイドル, on アイドル (single)
//	YOASOBI - Idol, on THE BOOK 3, with the Vocaloid label
//	Ayase - アイドル (demo), and 結束バンド (group, member Ayase)
//	Kaguya (counts for Ayase)
type music struct {
	u                                   int64
	yoasobiFW, yoasobi, ayase, band, kg int64
	idolJP, idolEN, demo                int64 // recordings
	single, book                        int64 // releases
	vocaloid                            int64 // label
}

func (m music) song(t *testing.T, db *DB, rec int64) int64 {
	t.Helper()
	var id int64
	if err := db.r.QueryRow(`SELECT song_id FROM recordings WHERE id = ?`, rec).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newMusic(t *testing.T, db *DB) music {
	t.Helper()
	ctx := context.Background()
	m := music{u: testUser(t, db)}
	m.vocaloid = addLabel(t, db, m.u, "Vocaloid")
	ls := []NewListen{
		{ListenedAt: 100, Artist: "ＹＯＡＳＯＢＩ", Title: "アイドル", Album: "アイドル", Payload: []byte(`{}`)},
		{ListenedAt: 200, Artist: "YOASOBI", Title: "Idol", Album: "THE BOOK 3", Payload: []byte(`{}`)},
		{ListenedAt: 300, Artist: "YOASOBI", Title: "Idol", Album: "THE BOOK 3", Payload: []byte(`{}`)},
		{ListenedAt: 400, Artist: "Ayase", Title: "アイドル", Payload: []byte(`{}`)},
	}
	if _, err := db.InsertListens(ctx, m.u, "listenbrainz", nil, ls); err != nil {
		t.Fatal(err)
	}
	err := db.Write(ctx, func(tx *sql.Tx) error {
		must := func(id int64, err error) int64 {
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		m.yoasobiFW = must(CreateArtistTx(ctx, tx, m.u, "ＹＯＡＳＯＢＩ"))
		m.yoasobi = must(CreateArtistTx(ctx, tx, m.u, "YOASOBI"))
		m.ayase = must(CreateArtistTx(ctx, tx, m.u, "Ayase"))
		m.band = must(CreateArtistTx(ctx, tx, m.u, "結束バンド"))
		m.kg = must(CreateArtistTx(ctx, tx, m.u, "Kaguya"))
		tx.Exec(`INSERT INTO group_members VALUES (?, ?)`, m.band, m.ayase)
		tx.Exec(`INSERT INTO artist_counts_for VALUES (?, ?, 'voice')`, m.kg, m.ayase)
		m.idolJP = must(CreateRecordingTx(ctx, tx, m.u, "アイドル", []Credit{{m.yoasobiFW, "main"}}))
		m.idolEN = must(CreateRecordingTx(ctx, tx, m.u, "Idol", []Credit{{m.yoasobi, "main"}, {m.kg, "featured"}}))
		m.demo = must(CreateRecordingTx(ctx, tx, m.u, "アイドル", []Credit{{m.ayase, "main"}, {m.band, "featured"}}))
		m.single = must(CreateReleaseTx(ctx, tx, m.u, "アイドル", []int64{m.yoasobiFW}))
		m.book = must(CreateReleaseTx(ctx, tx, m.u, "THE BOOK 3", []int64{m.yoasobi}))
		AddReleaseTrackTx(ctx, tx, m.single, m.idolJP)
		AddReleaseTrackTx(ctx, tx, m.book, m.idolEN)
		AddLabelTx(ctx, tx, m.vocaloid, "artist", m.yoasobi)
		AddLabelTx(ctx, tx, m.vocaloid, "artist", m.yoasobiFW)
		AddLabelTx(ctx, tx, m.vocaloid, "release", m.book)
		tx.Exec(`INSERT INTO credit_overrides VALUES ('recording', ?, ?, ?)`, m.demo, m.band, m.ayase)
		for _, r := range []int64{m.idolJP, m.idolEN, m.demo} {
			if err := RebuildRecordingArtistsTx(ctx, tx, r); err != nil {
				return err
			}
		}
		links := map[int64][2]int64{1: {m.idolJP, m.single}, 2: {m.idolEN, m.book}, 3: {m.demo, 0}}
		for id, l := range links {
			s, err := SourceTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := LinkSourceTx(ctx, tx, s, l[0], l[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.r.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// nothingPointsAt fails if any row still refers to a merged entity.
func nothingPointsAt(t *testing.T, db *DB, kind string, id int64) {
	t.Helper()
	checks := map[string][]string{
		"artist": {
			`SELECT count(*) FROM artist_aliases WHERE artist_id = ?`,
			`SELECT count(*) FROM group_members WHERE group_id = ?1 OR member_id = ?1`,
			`SELECT count(*) FROM artist_counts_for WHERE artist_id = ?1 OR target_id = ?1`,
			`SELECT count(*) FROM recording_credits WHERE artist_id = ?`,
			`SELECT count(*) FROM release_credits WHERE artist_id = ?`,
			`SELECT count(*) FROM credit_overrides WHERE from_id = ?1 OR to_id = ?1`,
			`SELECT count(*) FROM recording_artists WHERE artist_id = ?`,
			`SELECT count(*) FROM entity_labels WHERE entity_type = 'artist' AND entity_id = ?`,
		},
		"song": {
			`SELECT count(*) FROM song_aliases WHERE song_id = ?`,
			`SELECT count(*) FROM recordings WHERE song_id = ?`,
			`SELECT count(*) FROM entity_labels WHERE entity_type = 'song' AND entity_id = ?`,
		},
		"recording": {
			`SELECT count(*) FROM sources WHERE recording_id = ?`,
			`SELECT count(*) FROM listens WHERE recording_id = ?`,
			`SELECT count(*) FROM release_tracks WHERE recording_id = ?`,
			`SELECT count(*) FROM recording_credits WHERE recording_id = ?`,
			`SELECT count(*) FROM credit_overrides WHERE scope = 'recording' AND scope_id = ?`,
			`SELECT count(*) FROM rules WHERE recording_id = ?`,
			`SELECT count(*) FROM listen_totals WHERE recording_id = ?`,
		},
		"release": {
			`SELECT count(*) FROM release_aliases WHERE release_id = ?`,
			`SELECT count(*) FROM release_credits WHERE release_id = ?`,
			`SELECT count(*) FROM release_tracks WHERE release_id = ?`,
			`SELECT count(*) FROM sources WHERE release_id = ?`,
			`SELECT count(*) FROM listens WHERE release_id = ?`,
			`SELECT count(*) FROM credit_overrides WHERE scope = 'release' AND scope_id = ?`,
			`SELECT count(*) FROM rules WHERE release_id = ?`,
			`SELECT count(*) FROM entity_labels WHERE entity_type = 'release' AND entity_id = ?`,
		},
	}[kind]
	for _, q := range checks {
		if n := count(t, db, q, id); n > 0 {
			t.Errorf("%s %d merged but %d rows left: %s", kind, id, n, q)
		}
	}
}

func TestMergeArtists(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)

	id, err := db.Merge(ctx, m.u, "artist", m.yoasobiFW, m.yoasobi)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "artist", m.yoasobiFW)
	if n := count(t, db, `SELECT count(*) FROM artist_aliases WHERE artist_id = ?`, m.yoasobi); n != 2 {
		t.Errorf("winner has %d names, want 2", n)
	}
	// The label both had is kept once.
	if n := count(t, db, `SELECT count(*) FROM entity_labels WHERE entity_type = 'artist' AND entity_id = ?`, m.yoasobi); n != 1 {
		t.Errorf("winner has %d labels", n)
	}
	// Both recordings now count for the winner.
	if n := count(t, db, `SELECT count(*) FROM recording_artists WHERE artist_id = ?`, m.yoasobi); n != 2 {
		t.Errorf("winner credited on %d recordings", n)
	}
	var name, others string
	db.r.QueryRow(`SELECT name, other_names FROM artists WHERE id = ?`, m.yoasobi).Scan(&name, &others)
	if name != "YOASOBI" || others != "ＹＯＡＳＯＢＩ" {
		t.Errorf("names %q / %q", name, others)
	}
	var merged int64
	db.r.QueryRow(`SELECT merged_into FROM artists WHERE id = ?`, m.yoasobiFW).Scan(&merged)
	if merged != m.yoasobi {
		t.Errorf("merged_into = %d", merged)
	}

	if _, err := db.Merge(ctx, m.u, "artist", m.yoasobiFW, m.ayase); !errors.Is(err, ErrMergedAway) {
		t.Errorf("merging a merged artist again: %v", err)
	}
	if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

// A merge that would make "also counts for" or membership loop drops the
// moved link instead.
func TestMergeDropsLoops(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	// Kaguya counts for Ayase. Merging Ayase into Kaguya would make Kaguya
	// count for itself. Ayase is in 結束バンド, which moves to Kaguya.
	id, err := db.Merge(ctx, m.u, "artist", m.ayase, m.kg)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "artist", m.ayase)
	if n := count(t, db, `SELECT count(*) FROM artist_counts_for WHERE artist_id = target_id`); n != 0 {
		t.Error("self link left")
	}
	if n := count(t, db, `SELECT count(*) FROM group_members WHERE group_id = ? AND member_id = ?`, m.band, m.kg); n != 1 {
		t.Error("membership not moved")
	}
	// The override from the band to Ayase now points at Kaguya.
	if n := count(t, db, `SELECT count(*) FROM credit_overrides WHERE from_id = ? AND to_id = ?`, m.band, m.kg); n != 1 {
		t.Error("override not moved")
	}
	if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestMergeRecordingsTakesTheSong(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	loserSong, winnerSong := m.song(t, db, m.idolEN), m.song(t, db, m.idolJP)

	id, err := db.Merge(ctx, m.u, "recording", m.idolEN, m.idolJP)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "recording", m.idolEN)
	nothingPointsAt(t, db, "song", loserSong)
	if n := count(t, db, `SELECT n FROM listen_totals WHERE recording_id = ? AND release_id = ?`, m.idolJP, m.book); n != 2 {
		t.Errorf("listens on THE BOOK 3 now %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM song_aliases WHERE song_id = ?`, winnerSong); n != 2 {
		t.Errorf("song has %d names", n)
	}
	// Owner-merged text is never moved back by a reparse.
	if n := count(t, db, `SELECT count(*) FROM sources WHERE recording_id = ? AND linked_by = ?`, m.idolJP, id); n != 1 {
		t.Errorf("%d sources linked by the merge", n)
	}
	if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestMergeReleases(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE releases SET released = '2023-04-12' WHERE id = ?`, m.single)
		return err
	})
	start = snapshot(t, db)
	id, err := db.Merge(ctx, m.u, "release", m.single, m.book)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "release", m.single)
	if n := count(t, db, `SELECT count(*) FROM release_tracks WHERE release_id = ?`, m.book); n != 2 {
		t.Errorf("%d tracks", n)
	}
	var released string
	db.r.QueryRow(`SELECT released FROM releases WHERE id = ?`, m.book).Scan(&released)
	if released != "2023-04-12" {
		t.Errorf("release date %q not filled in", released)
	}
	if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestSplitSong(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	song := m.song(t, db, m.idolJP)
	// Put the demo under the same song first, then split it off again.
	if _, err := db.Merge(ctx, m.u, "song", m.song(t, db, m.demo), song); err != nil {
		t.Fatal(err)
	}
	start := snapshot(t, db)
	if _, _, err := db.SplitSong(ctx, m.u, song, []int64{m.idolJP, m.demo}); !errors.Is(err, ErrSplitAll) {
		t.Errorf("splitting everything off: %v", err)
	}
	newSong, id, err := db.SplitSong(ctx, m.u, song, []int64{m.demo})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.song(t, db, m.demo); got != newSong {
		t.Fatalf("demo is on song %d, want %d", got, newSong)
	}
	var name string
	db.r.QueryRow(`SELECT name FROM songs WHERE id = ?`, newSong).Scan(&name)
	if name != "アイドル" {
		t.Errorf("new song named %q", name)
	}
	if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestMergeRefusals(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	other, _ := db.CreateUser(ctx, "someone", "hash")
	for _, c := range []struct {
		user          int64
		kind          string
		loser, winner int64
		want          error
	}{
		{m.u, "artist", m.ayase, m.ayase, ErrMergeSelf},
		{m.u, "artist", m.ayase, 999, ErrNotFound},
		{other, "artist", m.ayase, m.kg, ErrNotFound},
	} {
		if _, err := db.Merge(ctx, c.user, c.kind, c.loser, c.winner); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if _, err := db.Merge(ctx, m.u, "listen", 1, 2); err == nil {
		t.Error("merged listens")
	}
}
