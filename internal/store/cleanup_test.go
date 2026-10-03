package store

import (
	"errors"
	"strings"
	"testing"
)

func songOf(t *testing.T, db *DB, recording int64) int64 {
	t.Helper()
	return int64(count(t, db, `SELECT song_id FROM recordings WHERE id = ?`, recording))
}

func TestNamesShown(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	for _, n := range []string{"Kessoku Band", "Kessoku Bando", "kessoku band.mp3"} {
		if _, err := db.AddName(ctx, m.u, "artist", m.band, n); err != nil {
			t.Fatal(err)
		}
	}
	names := func() (string, string) {
		var under, byline string
		db.r.QueryRow(`SELECT other_names, byline FROM artists WHERE id = ?`, m.band).Scan(&under, &byline)
		return under, byline
	}
	as, _ := db.Aliases(ctx, "artist", m.band)
	if under, byline := names(); under != as[1].Name || byline != as[1].Name+" · "+as[2].Name+" · "+as[3].Name {
		t.Fatalf("before: %q / %q", under, byline)
	}
	byName := map[string]int64{}
	for _, a := range as {
		byName[a.Name] = a.ID
	}

	// Hide the file name, and show the original script in lists.
	jp := byName["結束バンド"]
	edit, err := db.SetNames(ctx, m.u, "artist", m.band, NamesChange{
		Names:   []NameChoice{{AliasID: byName["kessoku band.mp3"], Shown: false}},
		InLists: &jp,
	})
	if err != nil || edit == 0 {
		t.Fatal(edit, err)
	}
	under, byline := names()
	if under != "結束バンド" || byline != "結束バンド · Kessoku Bando" && byline != "Kessoku Bando · 結束バンド" {
		t.Fatalf("after: %q / %q", under, byline)
	}
	// The hidden name still finds the artist.
	if found, _ := db.SearchItems(ctx, m.u, "artist", "band.mp3", 5); len(found) != 1 {
		t.Fatalf("hidden name doesn't find it: %v", found)
	}
	// None in lists.
	none := int64(0)
	if _, err := db.SetNames(ctx, m.u, "artist", m.band, NamesChange{InLists: &none}); err != nil {
		t.Fatal(err)
	}
	if under, _ := names(); under != "" {
		t.Fatalf("none: %q", under)
	}
	// Removing the chosen one goes back to the first shown name.
	if _, err := db.SetNames(ctx, m.u, "artist", m.band, NamesChange{InLists: &jp}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetNames(ctx, m.u, "artist", m.band, NamesChange{First: byName["Kessoku Bando"]}); err != nil {
		t.Fatal(err)
	}
	if as, _ := db.Aliases(ctx, "artist", m.band); as[0].Name != "Kessoku Bando" {
		t.Fatalf("first: %+v", as[0])
	}
	// Undoing the first change shows the file name again. What it set in
	// lists is what's there now, so that goes back too.
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if _, byline := names(); !strings.Contains(byline, "kessoku band.mp3") {
		t.Fatalf("undo: %q", byline)
	}
}

func TestRecordingCredits(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	song := songOf(t, db, m.demo)
	// A character nobody scrobbled yet gets the credit, and counts for
	// their voice actor.
	char, made, err := db.CreateArtist(ctx, m.u, "後藤ひとり")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetArtistLink(ctx, m.u, "artist_counts_for", char, m.kg, "voice", true); err != nil {
		t.Fatal(err)
	}
	edit, err := db.SetRecordingCredits(ctx, m.u, song, m.demo, []CreditChoice{{ArtistID: char, Role: "main"}, {ArtistID: m.band, Role: "featured"}})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := db.RecordingCredits(ctx, m.demo)
	if len(cs) != 2 || cs[0].ID != char || cs[1].Role != "featured" {
		t.Fatalf("credits %+v", cs)
	}
	if n := count(t, db, `SELECT count(*) FROM recording_artists WHERE recording_id = ? AND artist_id IN (?, ?)`, m.demo, char, m.kg); n != 2 {
		t.Fatalf("listens don't count for the character and voice: %d", n)
	}
	// Ayase is no longer credited, but the override from the band stays.
	if n := count(t, db, `SELECT count(*) FROM recording_credits WHERE recording_id = ? AND artist_id = ?`, m.demo, m.ayase); n != 0 {
		t.Fatal("old credit kept")
	}
	if _, err := db.SetRecordingCredits(ctx, m.u, song, m.idolEN, []CreditChoice{{ArtistID: char, Role: "main"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other song's recording: %v", err)
	}
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if cs, _ := db.RecordingCredits(ctx, m.demo); len(cs) != 2 || cs[0].ID != m.ayase {
		t.Fatalf("after undo %+v", cs)
	}
	if _, err := db.SetRecordingCredits(ctx, m.u, song, m.demo, []CreditChoice{{ArtistID: char, Role: "featured"}}); !errors.Is(err, ErrNoMain) {
		t.Fatalf("no main artist: %v", err)
	}
	// Someone new is added in the same edit, and undo takes them away.
	edit, err = db.SetRecordingCredits(ctx, m.u, song, m.demo, []CreditChoice{{NewArtist: "伊地知虹夏", Role: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if sums, _ := db.EditSummaries(ctx, m.u, []int64{edit}); !strings.HasSuffix(sums[0], "adding the artist 伊地知虹夏") {
		t.Fatalf("summary %q", sums[0])
	}
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM artists WHERE name = '伊地知虹夏'`); n != 0 {
		t.Fatal("new artist kept after undo")
	}
	// The new artist can't go while something links to it.
	if _, err := db.DeleteItem(ctx, m.u, "artist", char); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete linked: %v", err)
	}
	if _, err := db.DeleteItem(ctx, m.u, "artist", m.yoasobi); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete credited: %v", err)
	}
	_ = made
}

func TestTakeOffAlbumAndDelete(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	if u, _ := db.ItemUsage(ctx, "release", m.book); u.Songs != 1 || u.Listens != 2 {
		t.Fatalf("usage %+v", u)
	}
	off, err := db.TakeOffAlbum(ctx, m.u, m.book, []int64{m.idolEN})
	if err != nil || off == 0 {
		t.Fatal(off, err)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE release_id = ?`, m.book); n != 0 {
		t.Fatalf("%d listens still on it", n)
	}
	// Reading the text again doesn't put it back.
	if n := count(t, db, `SELECT count(*) FROM sources s JOIN edits e ON e.id = s.linked_by WHERE s.id = 2 AND e.automatic = 0`); n != 1 {
		t.Fatal("not linked by hand")
	}
	if u, _ := db.ItemUsage(ctx, "release", m.book); u.Used() {
		t.Fatalf("still used %+v", u)
	}
	del, err := db.DeleteItem(ctx, m.u, "release", m.book)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM releases WHERE id = ?`, m.book); n != 0 {
		t.Fatal("not deleted")
	}
	// Undo brings back the album, its names, label and credits, then the
	// tracks and listens.
	if _, err := db.UndoEdit(ctx, m.u, del); err != nil {
		t.Fatal(err)
	}
	var name string
	db.r.QueryRow(`SELECT name FROM releases WHERE id = ?`, m.book).Scan(&name)
	if name != "THE BOOK 3" || count(t, db, `SELECT count(*) FROM entity_labels WHERE entity_type = 'release' AND entity_id = ?`, m.book) != 1 ||
		count(t, db, `SELECT count(*) FROM release_credits WHERE release_id = ?`, m.book) != 1 {
		t.Fatal("album not back whole")
	}
	if _, err := db.UndoEdit(ctx, m.u, off); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM listens WHERE release_id = ?`, m.book); n != 2 {
		t.Fatalf("%d listens back", n)
	}
	if _, err := db.DeleteItem(ctx, m.u, "release", m.book); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete used album: %v", err)
	}

	// An unused artist can go, and its credit on an album with it.
	a, _, _ := db.CreateArtist(ctx, m.u, "4 million views")
	if _, err := db.SetAlbumArtists(ctx, m.u, m.single, []CreditChoice{{ArtistID: m.yoasobiFW}, {ArtistID: a}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteItem(ctx, m.u, "artist", a); !errors.Is(err, ErrInUse) {
		t.Fatalf("credited on an album: %v", err)
	}
	if _, err := db.SetAlbumArtists(ctx, m.u, m.single, []CreditChoice{{ArtistID: m.yoasobiFW}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteItem(ctx, m.u, "artist", a); err != nil {
		t.Fatal(err)
	}
}

func TestGraveyard(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	song := songOf(t, db, m.idolEN)
	total := func() int { return count(t, db, `SELECT count(*) FROM listens WHERE deleted_by IS NULL`) }
	before := total()
	bury, err := db.BurySong(ctx, m.u, song)
	if err != nil {
		t.Fatal(err)
	}
	if total() != before-2 {
		t.Fatal("listens not hidden")
	}
	// The same text sent again goes to the graveyard too.
	if _, err := db.InsertListens(ctx, m.u, "listenbrainz", nil, []NewListen{{ListenedAt: 500, Artist: "YOASOBI", Title: "Idol", Album: "THE BOOK 3", Payload: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if total() != before-2 {
		t.Fatal("new listen not hidden")
	}
	g, err := db.Graveyard(ctx, m.u)
	if err != nil || len(g) != 1 || g[0].Listens != 3 || g[0].ID != song {
		t.Fatalf("graveyard %+v %v", g, err)
	}
	if found, _ := db.SearchItems(ctx, m.u, "song", "Idol", 5); len(found) != 0 {
		t.Fatalf("buried song offered: %v", found)
	}
	if _, err := db.Merge(ctx, m.u, "recording", m.idolJP, m.idolEN); !errors.Is(err, ErrBuried) {
		t.Fatalf("merge into buried: %v", err)
	}
	if _, err := db.LinkSources(ctx, m.u, []int64{3}, m.idolEN); !errors.Is(err, ErrBuried) {
		t.Fatalf("link to buried: %v", err)
	}
	// Undo brings back all three, the one sent later too.
	if _, err := db.UndoEdit(ctx, m.u, bury); err != nil {
		t.Fatal(err)
	}
	if total() != before+1 {
		t.Fatalf("after undo %d, want %d", total(), before+1)
	}
	// Bring back works the same way.
	db.BurySong(ctx, m.u, song)
	if _, err := db.UnburySong(ctx, m.u, song); err != nil {
		t.Fatal(err)
	}
	if total() != before+1 {
		t.Fatal("not brought back")
	}
	// Deleting from the graveyard keeps the song there, empty.
	db.BurySong(ctx, m.u, song)
	del, err := db.DeleteBuriedListens(ctx, m.u, song)
	if err != nil || del == 0 {
		t.Fatal(del, err)
	}
	if g, _ := db.Graveyard(ctx, m.u); len(g) != 0 {
		t.Fatalf("still listed %+v", g)
	}
	if total() != before-2 {
		t.Fatal("deleted listens came back")
	}
	if buried, _ := db.SongBuried(ctx, song); !buried {
		t.Fatal("song left the graveyard")
	}
}
