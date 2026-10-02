package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Each edit on an item page changes what it should, and one Undo puts
// everything back.
func TestItemEdits(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	name := func(table string, id int64) string {
		var n string
		db.r.QueryRow(`SELECT name FROM `+table+` WHERE id = ?`, id).Scan(&n)
		return n
	}
	idolSong := m.song(t, db, m.idolJP)
	var character int64
	db.r.QueryRow(`SELECT id FROM labels WHERE name = 'Character'`).Scan(&character)

	for _, c := range []struct {
		name  string
		edit  func() (int64, error)
		check func() bool
	}{
		{"rename", func() (int64, error) { return db.Rename(ctx, m.u, "song", idolSong, "アイドル (Idol)") },
			func() bool { return name("songs", idolSong) == "アイドル (Idol)" }},
		// An English name would come first. Renaming to one keeps it first.
		{"rename to Latin", func() (int64, error) { return db.Rename(ctx, m.u, "artist", m.band, "Kessoku Band") },
			func() bool { return name("artists", m.band) == "Kessoku Band" }},
		{"add name", func() (int64, error) { return db.AddName(ctx, m.u, "artist", m.band, "Kessoku Band") },
			func() bool { return name("artists", m.band) == "Kessoku Band" }},
		{"label", func() (int64, error) { return db.SetLabel(ctx, m.u, "artist", m.kg, character, true) },
			func() bool {
				return count(t, db, `SELECT count(*) FROM entity_labels WHERE entity_id = ? AND entity_type = 'artist'`, m.kg) == 1
			}},
		{"kind", func() (int64, error) { return db.SetArtistKind(ctx, m.u, m.band, "group") },
			func() bool {
				return count(t, db, `SELECT count(*) FROM artists WHERE id = ? AND kind = 'group'`, m.band) == 1
			}},
		{"counts for", func() (int64, error) {
			return db.SetArtistLink(ctx, m.u, "artist_counts_for", m.yoasobi, m.ayase, "project", true)
		}, func() bool {
			return count(t, db, `SELECT count(*) FROM recording_artists WHERE recording_id = ? AND artist_id = ?`, m.idolEN, m.ayase) == 1
		}},
		{"member", func() (int64, error) { return db.SetArtistLink(ctx, m.u, "group_members", m.band, m.kg, "", true) },
			func() bool {
				return count(t, db, `SELECT count(*) FROM group_members WHERE group_id = ? AND member_id = ?`, m.band, m.kg) == 1
			}},
		{"remove member", func() (int64, error) { return db.SetArtistLink(ctx, m.u, "group_members", m.band, m.ayase, "", false) },
			func() bool { return count(t, db, `SELECT count(*) FROM group_members`) == 0 }},
		{"version", func() (int64, error) { return db.SetVersion(ctx, m.u, idolSong, m.idolJP, "TV size") },
			func() bool { return count(t, db, `SELECT count(*) FROM recordings WHERE version = 'TV size'`) == 1 }},
		{"own row", func() (int64, error) { return db.SetOwnRow(ctx, m.u, idolSong, m.idolJP, true) },
			func() bool { return count(t, db, `SELECT count(*) FROM recordings WHERE rank_alone = 1`) == 1 }},
		{"original", func() (int64, error) { return db.SetOriginal(ctx, m.u, idolSong, m.idolJP) },
			func() bool { return count(t, db, `SELECT count(*) FROM recordings WHERE is_original = 1`) == 1 }},
		{"album", func() (int64, error) {
			return db.SetAlbumDetails(ctx, m.u, m.book, AlbumDetails{Kind: "album", Released: "2023-10", Context: "Japanese edition"})
		}, func() bool { return count(t, db, `SELECT count(*) FROM releases WHERE released = '2023-10'`) == 1 }},
		{"override", func() (int64, error) {
			return db.SetOverride(ctx, m.u, "release", m.book, Override{Scope: "release", ScopeID: m.book, From: Ref{ID: m.kg}, To: Ref{ID: m.yoasobi}}, true)
		}, func() bool {
			// Kaguya's listens on THE BOOK 3 count for YOASOBI instead of Ayase.
			return count(t, db, `SELECT count(*) FROM recording_artists WHERE recording_id = ? AND artist_id = ?`, m.idolEN, m.ayase) == 0
		}},
		{"create label", func() (int64, error) { return db.CreateLabel(ctx, m.u, "Anime", true) },
			func() bool {
				return count(t, db, `SELECT count(*) FROM labels WHERE name = 'Anime' AND position = 1`) == 1
			}},
	} {
		start := snapshot(t, db)
		id, err := c.edit()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if id == 0 || !c.check() {
			t.Fatalf("%s didn't happen", c.name)
		}
		if _, err := db.UndoEdit(ctx, m.u, id); err != nil {
			t.Fatalf("undo %s: %v", c.name, err)
		}
		if got := snapshot(t, db); got != start {
			t.Fatalf("undo %s\n%s\nwant\n%s", c.name, got, start)
		}
	}
}

func TestItemEditRefusals(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	aliases, _ := db.Aliases(ctx, "artist", m.ayase)
	if _, err := db.RemoveName(ctx, m.u, "artist", m.ayase, aliases[0].ID); !errors.Is(err, ErrLastName) {
		t.Errorf("removing the last name: %v", err)
	}
	// Kaguya counts for Ayase, so Ayase can't count for Kaguya.
	if _, err := db.SetArtistLink(ctx, m.u, "artist_counts_for", m.ayase, m.kg, "", true); !errors.Is(err, ErrLoop) {
		t.Errorf("loop: %v", err)
	}
	if _, err := db.SetArtistLink(ctx, m.u, "group_members", m.ayase, m.band, "", true); !errors.Is(err, ErrLoop) {
		t.Errorf("group in its own member: %v", err)
	}
	if _, err := db.SetAlbumDetails(ctx, m.u, m.book, AlbumDetails{Kind: "album", Released: "April 2023"}); !errors.Is(err, ErrValue) {
		t.Errorf("bad date: %v", err)
	}
	if _, err := db.Rename(ctx, m.u, "song", m.song(t, db, m.demo), "  "); !errors.Is(err, ErrName) {
		t.Errorf("empty name: %v", err)
	}
	// An override on another song's recording.
	if _, err := db.SetOverride(ctx, m.u, "song", m.song(t, db, m.idolJP), Override{Scope: "recording", ScopeID: m.demo, From: Ref{ID: m.band}, To: Ref{ID: m.kg}}, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("override elsewhere: %v", err)
	}
	other, _ := db.CreateUser(ctx, "someone", "h")
	if _, err := db.Rename(ctx, other, "artist", m.ayase, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renaming someone else's: %v", err)
	}
}

func TestPinAndLangAndMoveLabel(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	db.AddName(ctx, m.u, "artist", m.band, "Kessoku Band")
	as, _ := db.Aliases(ctx, "artist", m.band)
	if as[0].Name != "Kessoku Band" {
		t.Fatalf("order %+v", as)
	}
	if _, err := db.PinName(ctx, m.u, "artist", m.band, as[1].ID); err != nil {
		t.Fatal(err)
	}
	if as, _ := db.Aliases(ctx, "artist", m.band); as[0].Name != "結束バンド" || !as[0].Pinned {
		t.Fatalf("pin %+v", as)
	}
	if _, err := db.SetNameLang(ctx, m.u, "artist", m.band, as[0].ID, "romaji"); err != nil {
		t.Fatal(err)
	}
	if as, _ := db.Aliases(ctx, "artist", m.band); as[1].Lang != "romaji" || !as[1].LangSet {
		t.Fatalf("lang %+v", as)
	}
	ls, _ := db.Labels(ctx, m.u)
	if _, err := db.MoveLabel(ctx, m.u, ls[1].ID, -1); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Labels(ctx, m.u); got[0].ID != ls[1].ID {
		t.Fatalf("order %v", got)
	}
}

func TestEntityStats(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	// THE BOOK 3 has two listens, アイドル (single) one: THE BOOK 3 is #1.
	st, err := db.EntityStats(ctx, m.u, "release", m.book)
	if err != nil || st.Listens != 2 || st.Rank != 1 || st.First != 200 || st.Last != 300 {
		t.Fatalf("book %+v %v", st, err)
	}
	if st, _ := db.EntityStats(ctx, m.u, "release", m.single); st.Rank != 2 {
		t.Fatalf("single %+v", st)
	}
	// Characters are hidden in Top artists by default, so they get no rank
	// and don't push anyone else down.
	var character int64
	db.r.QueryRow(`SELECT id FROM labels WHERE name = 'Character'`).Scan(&character)
	db.SetLabel(ctx, m.u, "artist", m.kg, character, true)
	if st, _ := db.EntityStats(ctx, m.u, "artist", m.kg); st.Rank != 0 || st.Listens == 0 {
		t.Fatalf("character %+v", st)
	}
	// Ayase has YOASOBI's two listens through Kaguya, plus the demo.
	if st, _ := db.EntityStats(ctx, m.u, "artist", m.ayase); st.Rank != 1 || st.Listens != 3 {
		t.Fatalf("ayase %+v", st)
	}
	if st, _ := db.EntityStats(ctx, m.u, "artist", m.yoasobi); st.Rank != 2 {
		t.Fatalf("yoasobi %+v", st)
	}
	var unheard int64
	db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		unheard, err = CreateArtistTx(ctx, tx, m.u, "Nobody Yet")
		return err
	})
	if st, _ := db.EntityStats(ctx, m.u, "artist", unheard); st.Listens != 0 || st.Rank != 0 {
		t.Fatalf("never heard %+v", st)
	}
}
