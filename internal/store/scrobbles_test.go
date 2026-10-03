package store

import (
	"context"
	"database/sql"
	"testing"
)

func undone(t *testing.T, db *DB, u, editID int64, start string) {
	t.Helper()
	if _, err := db.UndoEdit(context.Background(), u, editID); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestSourcesOf(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	got, err := db.SourcesOf(ctx, m.u, "release", m.book)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Idol" || got[0].Listens != 2 || got[0].OnAlbum.ID != m.book || got[0].LatestListen == 0 {
		t.Fatalf("on THE BOOK 3: %+v", got)
	}
	if got[0].SetBy != "auto" || got[0].Fixed() {
		t.Errorf("set by %q", got[0].SetBy)
	}
	// By hand, the same text says so.
	if _, err := db.LinkSources(ctx, m.u, []int64{2}, m.idolJP); err != nil {
		t.Fatal(err)
	}
	got, err = db.SourcesOf(ctx, m.u, "song", m.song(t, db, m.idolJP))
	if err != nil {
		t.Fatal(err)
	}
	// アイドル has three recordings' worth of text now: its own, the demo
	// when they share the song, and the one just moved.
	var moved *LinkedSource
	for i := range got {
		if got[i].ID == 2 {
			moved = &got[i]
		}
	}
	if moved == nil || moved.SetBy != "link" || !moved.Fixed() {
		t.Fatalf("moved text: %+v", got)
	}
	fixed, total, err := db.FixedSources(ctx, m.u, "", 10, 0)
	if err != nil || total != 1 || len(fixed) != 1 || fixed[0].ID != 2 {
		t.Fatalf("fixed links: %v %d %+v", err, total, fixed)
	}
	if fixed, total, _ = db.FixedSources(ctx, m.u, "nothing like it", 10, 0); total != 0 || len(fixed) != 0 {
		t.Errorf("search found %d", total)
	}
}

func TestSetSourcesRelease(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	// The two "Idol" listens go from THE BOOK 3 to the single.
	res, err := db.SetSourcesRelease(ctx, m.u, []int64{2}, m.single)
	if err != nil {
		t.Fatal(err)
	}
	if res.Listens != 2 || res.Name != "アイドル" {
		t.Errorf("result %+v", res)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE recording_id = ? AND release_id = ?`, m.idolEN, m.single); n != 2 {
		t.Errorf("%d listens on the single", n)
	}
	if n := count(t, db, `SELECT count(*) FROM release_tracks WHERE release_id = ? AND recording_id = ?`, m.single, m.idolEN); n != 1 {
		t.Error("not added as a track")
	}
	if src, _ := db.SourceInfo(ctx, m.u, 2); !src.LinkedByOwner {
		t.Error("not marked as linked by hand")
	}
	// There already: nothing to do.
	if again, err := db.SetSourcesRelease(ctx, m.u, []int64{2}, m.single); err != nil || again.EditID != 0 {
		t.Errorf("second time: %v %+v", err, again)
	}
	undone(t, db, m.u, res.EditID, start)

	// No album at all.
	res, err = db.SetSourcesRelease(ctx, m.u, []int64{2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE recording_id = ? AND release_id IS NULL`, m.idolEN); n != 2 {
		t.Errorf("%d listens with no album", n)
	}
	undone(t, db, m.u, res.EditID, start)

	if _, err := db.SetSourcesRelease(ctx, m.u, []int64{2}, 999); err == nil {
		t.Error("put on an album that isn't there")
	}
	if _, err := db.SetSourcesRelease(ctx, m.u, []int64{4}, m.single); err == nil {
		t.Error("put unlinked text on an album")
	}
}

func TestLinkListenAlone(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	var listen int64
	db.r.QueryRow(`SELECT id FROM listens WHERE source_id = 2 ORDER BY id LIMIT 1`).Scan(&listen)

	// One of the two "Idol" listens was really アイドル.
	res, err := db.LinkListen(ctx, m.u, listen, m.idolJP, AlbumChoice{ID: m.single})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "Idol" || res.Name != "アイドル" {
		t.Errorf("result %+v", res)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE source_id = 2 AND recording_id = ?`, m.idolEN); n != 1 {
		t.Errorf("%d listens left on Idol, want 1", n)
	}
	if n := count(t, db, `SELECT n FROM listen_totals WHERE recording_id = ? AND release_id = ?`, m.idolJP, m.single); n != 2 {
		t.Errorf("アイドル on its single has %d listens, want 2", n)
	}

	// The text moving elsewhere leaves the listen where it was put.
	moved, err := db.LinkSources(ctx, m.u, []int64{2}, m.demo)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Listens != 1 {
		t.Errorf("moved %d listens with the text, want 1", moved.Listens)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE id = ? AND recording_id = ?`, listen, m.idolJP); n != 1 {
		t.Error("the listen followed its text")
	}
	if srcs, _ := db.SourcesOf(ctx, m.u, "song", m.song(t, db, m.demo)); len(srcs) == 0 || srcs[len(srcs)-1].Listens > 1 {
		t.Errorf("text counts the listen linked alone: %+v", srcs)
	}
	alone, err := db.FixedListens(ctx, m.u, "song", m.song(t, db, m.idolJP))
	if err != nil || len(alone) != 1 || alone[0].ID != listen || !alone[0].Fixed {
		t.Fatalf("linked alone: %v %+v", err, alone)
	}

	// A merge takes the listen along.
	mergeID, err := db.Merge(ctx, m.u, "recording", m.idolJP, m.idolEN)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "recording", m.idolJP)
	if _, err := db.UndoEdit(ctx, m.u, mergeID); err != nil {
		t.Fatal(err)
	}
	albums, err := db.Merge(ctx, m.u, "release", m.single, m.book)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "release", m.single)
	if _, err := db.UndoEdit(ctx, m.u, albums); err != nil {
		t.Fatal(err)
	}
	// So does taking the song off the album.
	off, err := db.TakeOffAlbum(ctx, m.u, m.single, []int64{m.idolJP})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE release_id = ?`, m.single); n != 0 {
		t.Errorf("%d listens still on the album", n)
	}
	if _, err := db.UndoEdit(ctx, m.u, off); err != nil {
		t.Fatal(err)
	}

	// Following the text again puts it with the other listen.
	back, err := db.UnfixListen(ctx, m.u, listen)
	if err != nil || back == 0 {
		t.Fatal(err, back)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE source_id = 2 AND recording_id = ? AND fixed_by IS NULL`, m.demo); n != 2 {
		t.Errorf("%d listens with the text, want 2", n)
	}
	if again, _ := db.UnfixListen(ctx, m.u, listen); again != 0 {
		t.Error("followed the text twice")
	}
	for _, id := range []int64{back, moved.EditID, res.EditID} {
		if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
}

func TestMergeMany(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	var third int64
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		third, err = CreateReleaseTx(ctx, tx, m.u, "アイドル (Single)", []int64{m.yoasobi})
		if err == nil {
			err = AddReleaseTrackTx(ctx, tx, third, m.idolJP)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start = snapshot(t, db)

	related, err := db.RelatedReleases(ctx, m.u, m.single, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != third || related[0].Shared != 1 {
		t.Fatalf("related to the single: %+v", related)
	}
	albums, err := db.SongAlbums(ctx, m.u, m.song(t, db, m.idolJP))
	if err != nil || len(albums) != 2 || albums[0].ID != m.single || albums[0].Listens != 1 {
		t.Fatalf("albums of アイドル: %v %+v", err, albums)
	}
	on, err := db.RecordingAlbums(ctx, []int64{m.idolJP, m.demo})
	if err != nil || len(on[m.idolJP]) != 2 || len(on[m.demo]) != 0 {
		t.Fatalf("albums per recording: %v %+v", err, on)
	}

	id, err := db.MergeMany(ctx, m.u, "release", []int64{m.single, third, m.book}, m.book)
	if err != nil {
		t.Fatal(err)
	}
	nothingPointsAt(t, db, "release", m.single)
	nothingPointsAt(t, db, "release", third)
	if n := count(t, db, `SELECT count(*) FROM release_tracks WHERE release_id = ?`, m.book); n != 2 {
		t.Errorf("%d tracks, want 2", n)
	}
	if n := count(t, db, `SELECT count(*) FROM edits WHERE id = ? AND summary = 'Merged アイドル, アイドル (Single) into THE BOOK 3'`, id); n != 1 {
		t.Error("summary doesn't name what was merged")
	}
	undone(t, db, m.u, id, start)
	if _, err := db.MergeMany(ctx, m.u, "release", []int64{m.book}, m.book); err == nil {
		t.Error("merged an album into itself")
	}
}
