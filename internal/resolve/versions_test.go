package resolve

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"chokominto/internal/store"
)

// The owner's decisions from their real history (2026-09-30).
func TestVersionsAndOtherNames(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scrobble("YURiKA", "鏡面の波", "")
	e.scrobble("YURiKA", "鏡面の波 (Instrumental)", "")
	e.scrobble("YURiKA", "鏡面の波 (Instrumental)", "")
	e.scrobble("YURiKA", "鏡面の波 - Kyoumen no Nami", "")
	e.scrobble("YURiKA", "Kyoumen no Nami", "")
	e.scrobble("YURiKA", "鏡面の波 (TV size)", "")

	if n := e.one(`SELECT count(*) FROM songs`); n != "1" {
		t.Fatalf("%s songs, want one song with its versions", n)
	}
	if got := e.q(`SELECT version || ':' || rank_alone FROM recordings ORDER BY id`); len(got) != 3 ||
		got[0] != ":0" || got[1] != "Instrumental:1" || got[2] != "TV size:0" {
		t.Fatalf("versions %v", got)
	}
	if got := e.one(`SELECT group_concat(name, ' | ') FROM (SELECT name FROM song_aliases ORDER BY id)`); got != "鏡面の波 | Kyoumen no Nami" {
		t.Fatalf("names %q", got)
	}

	// Combined, the instrumental keeps its own row. The TV size pools.
	rows, err := e.db.TopSongs(ctx, e.user, store.RankQuery{From: 0, To: 1 << 40, Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Listens != 4 || rows[0].Version != "" || rows[1].Listens != 2 || rows[1].Version != "Instrumental" {
		t.Fatalf("top songs %+v", rows)
	}
}

func TestGroupNamedWithCharacters(t *testing.T) {
	e := newEnv(t)
	e.scrobble("桜高軽音部 [平沢唯・秋山澪(CV:豊崎愛生、日笠陽子)]", "ふわふわ時間", "")
	if got := e.one(`SELECT kind FROM artists WHERE name = '桜高軽音部'`); got != "group" {
		t.Fatalf("kind %q", got)
	}
	members := e.q(`SELECT m.name FROM group_members g JOIN artists m ON m.id = g.member_id ORDER BY m.name`)
	if len(members) != 2 {
		t.Fatalf("members %v", members)
	}
	// Listens count for the group, its characters and their voices.
	if got := e.lastCredits(); got != "平沢唯:group, 日笠陽子:group, 桜高軽音部:credited, 秋山澪:group, 豊崎愛生:group" {
		t.Fatalf("credits %s", got)
	}
}

// "テオ - Teo" when both "テオ" and "Teo" are songs already: the title's
// song, not "which one?".
func TestTwoTitlesBothKnown(t *testing.T) {
	e := newEnv(t)
	e.scrobble("Omoi", "テオ", "")
	e.scrobble("Omoi", "Teo", "")
	e.scrobble("Omoi", "テオ - Teo", "")
	first := e.one(`SELECT r.song_id FROM sources s JOIN recordings r ON r.id = s.recording_id WHERE s.title_text = 'テオ'`)
	if got := e.one(`SELECT r.song_id FROM listens l JOIN recordings r ON r.id = l.recording_id WHERE l.listened_at = ?`, e.ts); got != first {
		t.Fatalf("linked to song %s, want テオ's %s", got, first)
	}
}

// Several characters singing one character's song is another recording of it.
// One featured artist more or less is the same recording.
func TestRecordingByArtists(t *testing.T) {
	e := newEnv(t)
	e.scrobble("Mai Sakurajima(CV:Asami Seto)", "不可思議のカルテ", "")
	e.scrobble("Mai Sakurajima(CV:Asami Seto), Tomoe Koga(CV:Nao Toyama), Rio Futaba(CV:Atsumi Tanezaki)", "不可思議のカルテ", "")
	e.scrobble("siinamota", "少女A", "")
	e.scrobble("siinamota, Kagamine Rin", "少女A", "")
	e.scrobble("siinamota", "少女A (feat. Kagamine Rin)", "")
	if got := e.one(`SELECT count(*) FROM recordings r JOIN songs s ON s.id = r.song_id WHERE s.name = '不可思議のカルテ'`); got != "2" {
		t.Errorf("%s recordings of 不可思議のカルテ, want 2", got)
	}
	if got := e.one(`SELECT count(DISTINCT song_id) FROM recordings`); got != "2" {
		t.Errorf("%s songs, want 2", got)
	}
	if got := e.one(`SELECT count(*) FROM recordings r JOIN songs s ON s.id = r.song_id WHERE s.name = '少女A'`); got != "1" {
		t.Errorf("%s recordings of 少女A, want 1", got)
	}
}

// A cover title naming the singer in another script is read when the
// singer is that artist, and left as sent when it's someone else.
func TestCoverNamedInOtherScript(t *testing.T) {
	e := newEnv(t)
	e.scrobble("Suisei Hoshimachi", "Stellar Stellar", "")
	sid := e.one(`SELECT id FROM artists WHERE name = 'Suisei Hoshimachi'`)
	e.db.Write(context.Background(), func(tx *sql.Tx) error {
		id, _ := strconv.ParseInt(sid, 10, 64)
		return store.AddAliasTx(context.Background(), tx, "artist_aliases", id, "星街すいせい")
	})
	e.scrobble("Suisei Hoshimachi", "フォニイ / 星街すいせい(Cover)", "")
	if got := e.one(`SELECT sg.name FROM listens l JOIN recordings r ON r.id = l.recording_id JOIN songs sg ON sg.id = r.song_id WHERE l.listened_at = ?`, e.ts); got != "フォニイ" {
		t.Fatalf("song %q", got)
	}
	e.scrobble("Suisei Hoshimachi", "Song / Someone Else(Cover)", "")
	if got := e.one(`SELECT sg.name FROM listens l JOIN recordings r ON r.id = l.recording_id JOIN songs sg ON sg.id = r.song_id WHERE l.listened_at = ?`, e.ts); got != "Song / Someone Else(Cover)" {
		t.Fatalf("someone else's cover read as %q", got)
	}
}

// A soundtrack whose tracks name different composers is one album,
// credited to all of them. A generic name by two bands stays two albums.
func TestSoundtrackOneAlbum(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ost := "Atelier Ryza: Ever Darkness & the Secret Hideout Original Soundtrack"
	e.scrobble("柳川和樹", "Prologue", ost)
	e.scrobble("水上浩介", "Field", ost)
	e.scrobble("三武亜紗美", "Battle", ost)
	e.scrobbleAA("中村新一郎", "Town", ost, "Various Artists")
	if n := e.one(`SELECT count(*) FROM releases`); n != "1" {
		t.Fatalf("%s albums, want one", n)
	}
	if got := e.one(`SELECT group_concat(a.name, ', ') FROM (SELECT a.name FROM release_credits rc JOIN artists a ON a.id = rc.artist_id ORDER BY rc.position) a`); got != "柳川和樹, 水上浩介, 三武亜紗美, 中村新一郎" {
		t.Fatalf("credited to %q", got)
	}
	if n := e.one(`SELECT count(*) FROM artists WHERE name = 'Various Artists'`); n != "0" {
		t.Fatal("made a Various Artists artist")
	}

	e.scrobble("Band A", "Song A", "Best")
	e.scrobble("Band B", "Song B", "Best")
	if n := e.one(`SELECT count(*) FROM releases WHERE name = 'Best'`); n != "2" {
		t.Fatalf("%s albums called Best, want two", n)
	}

	// With the reading off, a soundtrack splits by composer again.
	if _, err := SetReadings(ctx, e.db, e.user, map[string]bool{"compilation": false}); err != nil {
		t.Fatal(err)
	}
	e.scrobble("Composer X", "Theme", "Another Game Original Soundtrack")
	e.scrobble("Composer Y", "Credits", "Another Game Original Soundtrack")
	if n := e.one(`SELECT count(*) FROM releases WHERE name = 'Another Game Original Soundtrack'`); n != "2" {
		t.Fatalf("%s albums with the reading off", n)
	}
}

// A song's page ranks it by the row Top songs shows for it: with the
// instrumental on its own row, the song ranks by its other versions only.
func TestSongRankMatchesTopSongs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for range 3 {
		e.scrobble("YURiKA", "鏡面の波", "")
		e.scrobble("YURiKA", "鏡面の波 (Instrumental)", "")
	}
	for range 4 {
		e.scrobble("the peggies", "Centimeter", "")
	}
	song, _ := strconv.ParseInt(e.one(`SELECT id FROM songs WHERE name = '鏡面の波'`), 10, 64)
	st, err := e.db.EntityStats(ctx, e.user, "song", song)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.db.TopSongs(ctx, e.user, store.RankQuery{From: 0, To: 1 << 40, Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	var want int
	for _, r := range rows {
		if r.SongID == song && r.RecordingID == 0 {
			want = r.Rank
		}
	}
	if st.Listens != 6 || st.Rank != want || want != 2 {
		t.Fatalf("song page %d listens at #%d, Top songs has it at #%d (%+v)", st.Listens, st.Rank, want, rows)
	}
}

// "Unknown" from a player that doesn't know is read as nothing sent: no
// artist or album of that name.
func TestUnknownIsNothing(t *testing.T) {
	e := newEnv(t)
	e.scrobble("Unknown", "Shoujorei_offvocal_[Master].wav", "Unknown")
	e.scrobble("<unknown>", "Track 01", "")
	e.scrobble("midorijeon", "Seashore", "Unknown")
	e.scrobbleAA("Sayaka Kanda", "Prologue", "Atelier Ryza Original Soundtrack", "Unknown Artist")
	if got := e.q(`SELECT name FROM artists ORDER BY name`); strings.Join(got, ", ") != "Sayaka Kanda, midorijeon" {
		t.Fatalf("artists %v", got)
	}
	if got := e.q(`SELECT name FROM releases ORDER BY name`); strings.Join(got, ", ") != "Atelier Ryza Original Soundtrack" {
		t.Fatalf("albums %v", got)
	}
	if n := e.one(`SELECT count(*) FROM sources WHERE recording_id IS NULL`); n != "2" {
		t.Fatalf("%s texts without a song, want the two with no artist", n)
	}
}

// Text linked the old way, to an artist called "Unknown", comes off when
// it's read again, and its listens stop counting.
func TestUnknownUnlinkedOnReparse(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scrobble("Unknown", "Shoujorei_offvocal_[Master].wav", "")
	var src store.Source
	err := e.db.Write(ctx, func(tx *sql.Tx) error {
		id, err := strconv.ParseInt(e.one(`SELECT id FROM sources`), 10, 64)
		if err != nil {
			return err
		}
		artist, err := store.CreateArtistTx(ctx, tx, e.user, "Unknown")
		if err != nil {
			return err
		}
		rec, err := store.CreateRecordingTx(ctx, tx, e.user, "Shoujorei_offvocal_[Master].wav", []store.Credit{{ArtistID: artist, Role: "main"}})
		if err != nil {
			return err
		}
		if src, err = store.SourceTx(ctx, tx, id); err != nil {
			return err
		}
		return store.LinkSourceByRuleTx(ctx, tx, src, rec, 0, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.one(`SELECT coalesce(sum(n), 0) FROM listen_totals`); n != "1" {
		t.Fatalf("old link counts %s listens", n)
	}
	r := &Resolver{DB: e.db}
	if err := r.ReparseJob(ctx, store.Job{Key: fmt.Sprint("source:", src.ID)}); err != nil {
		t.Fatal(err)
	}
	if got := e.one(`SELECT coalesce(recording_id, 'none') FROM sources`); got != "none" {
		t.Fatalf("still linked to %s", got)
	}
	if n := e.one(`SELECT coalesce(sum(n), 0) FROM listen_totals`); n != "0" {
		t.Fatalf("%s listens still counted", n)
	}
}
