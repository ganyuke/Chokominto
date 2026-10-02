package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

func TestSearchRecordings(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	for q, want := range map[string][]int64{
		"aidoru":  {m.idolJP, m.demo},   // romaji for kana names
		"kessoku": {m.demo},             // guessed reading of 結束バンド
		"book 3":  {m.idolEN},           // album name
		"ＩＤＯＬ":    {m.idolEN},           // width and case
		"yoasobi": {m.idolJP, m.idolEN}, // artist
		"アイドル":    {m.idolJP, m.demo},   // original script
		"nothing": nil,
	} {
		found, err := db.SearchRecordings(ctx, m.u, q, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, f := range found {
			got = append(got, f.RecordingID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%q found %v, want %v", q, got, want)
		}
	}
}

func TestLinkSourcesKeepsAlbum(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	start := snapshot(t, db)
	// The THE BOOK 3 listens were "Idol". Link them to アイドル: they stay on
	// THE BOOK 3, which gets アイドル as a track.
	res, err := db.LinkSources(ctx, m.u, []int64{2}, m.idolJP)
	if err != nil {
		t.Fatal(err)
	}
	if res.Listens != 2 {
		t.Errorf("moved %d listens", res.Listens)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE recording_id = ? AND release_id = ?`, m.idolJP, m.book); n != 2 {
		t.Errorf("%d listens of アイドル on THE BOOK 3", n)
	}
	if n := count(t, db, `SELECT count(*) FROM release_tracks WHERE release_id = ? AND recording_id = ?`, m.book, m.idolJP); n != 1 {
		t.Error("not added to the album")
	}
	src, _ := db.SourceInfo(ctx, m.u, 2)
	if !src.LinkedByOwner {
		t.Error("not marked as linked by hand")
	}
	if _, err := db.UndoEdit(ctx, m.u, res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}
	if _, err := db.LinkSources(ctx, m.u, []int64{2}, 999); err == nil {
		t.Error("linked to a recording that isn't there")
	}
}

func TestGuessKeysJob(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE artist_aliases SET guess_key = NULL`)
		return err
	})
	if err := db.GuessKeysJob(ctx, Job{Key: "artist_aliases:0"}); err != nil {
		t.Fatal(err)
	}
	var k string
	db.r.QueryRow(`SELECT guess_key FROM artist_aliases WHERE artist_id = ?`, m.band).Scan(&k)
	if k != "kessokubando" {
		t.Errorf("guess %q", k)
	}
}
