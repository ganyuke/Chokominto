package store

import (
	"context"
	"database/sql"
	"testing"
)

func addArt(t *testing.T, db *DB, sha string) int64 {
	t.Helper()
	var id int64
	err := db.Write(context.Background(), func(tx *sql.Tx) error {
		var err error
		id, err = AddArtworkTx(context.Background(), tx, Artwork{SHA256: sha, Format: "jpeg", Width: 500, Height: 500, Origin: "upload"}, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestArtworkChoice(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	found := addArt(t, db, "aa")
	chosen := addArt(t, db, "bb")
	if again := addArt(t, db, "aa"); again != found {
		t.Fatal("same picture stored twice")
	}
	set := func(id int64, pinned, auto bool) int64 {
		t.Helper()
		var e int64
		err := db.Write(ctx, func(tx *sql.Tx) error {
			var err error
			e, err = SetArtworkTx(ctx, tx, m.u, "release", m.book, id, pinned, auto)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	set(found, false, true)
	if a, pinned, _ := db.ItemArtwork(ctx, "release", m.book); a == nil || a.ID != found || pinned {
		t.Fatalf("found picture not shown: %+v %v", a, pinned)
	}
	edit := set(chosen, true, false)
	// A lookup never replaces the owner's choice.
	if e := set(found, false, true); e != 0 {
		t.Fatal("lookup replaced a chosen picture")
	}
	if a, pinned, _ := db.ItemArtwork(ctx, "release", m.book); a.ID != chosen || !pinned {
		t.Fatal("choice not kept")
	}
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if a, pinned, _ := db.ItemArtwork(ctx, "release", m.book); a.ID != found || pinned {
		t.Fatal("undo didn't bring the found picture back")
	}
	// Both are still referenced by the edit log, so neither is unused.
	if unused, _ := db.UnusedArtwork(ctx); len(unused) != 0 {
		t.Fatalf("unused %+v", unused)
	}
	lone := addArt(t, db, "cc")
	if unused, _ := db.UnusedArtwork(ctx); len(unused) != 1 || unused[0].ID != lone {
		t.Fatalf("unused %+v", unused)
	}

	// Merging gives the winner the loser's picture when it has none.
	if _, err := db.Merge(ctx, m.u, "release", m.book, m.single); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := db.ItemArtwork(ctx, "release", m.single); a == nil || a.ID != found {
		t.Fatal("merge lost the picture")
	}
}
