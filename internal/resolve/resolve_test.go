package resolve

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"chokominto/internal/jobs"
	"chokominto/internal/store"
)

type env struct {
	t    *testing.T
	db   *store.DB
	user int64
	run  *jobs.Runner
	ts   int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(context.Background(), "elaina", "h")
	r := &Resolver{DB: db}
	run := &jobs.Runner{DB: db, Handlers: map[string]jobs.Handler{"resolve": r.Job}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &env{t, db, u, run, 1_700_000_000}
}

// scrobble stores a listen the normal way and runs the queued linking.
func (e *env) scrobble(artist, title, album string) {
	e.t.Helper()
	e.ts += 60
	if _, err := e.db.InsertListens(context.Background(), e.user, "listenbrainz", nil,
		[]store.NewListen{{ListenedAt: e.ts, Artist: artist, Title: title, Album: album, Payload: []byte(`{}`)}}); err != nil {
		e.t.Fatal(err)
	}
	e.run.Drain(context.Background())
}

// scrobbleAA is scrobble with the album artist a client sent separately.
func (e *env) scrobbleAA(artist, title, album, albumArtist string) {
	e.t.Helper()
	e.ts += 60
	if _, err := e.db.InsertListens(context.Background(), e.user, "listenbrainz", nil,
		[]store.NewListen{{ListenedAt: e.ts, Artist: artist, Title: title, Album: album, AlbumArtist: albumArtist, Payload: []byte(`{}`)}}); err != nil {
		e.t.Fatal(err)
	}
	e.run.Drain(context.Background())
}

func (e *env) q(query string, args ...any) []string {
	e.t.Helper()
	rows, err := e.db.Reader().Query(query, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (e *env) one(query string, args ...any) string {
	e.t.Helper()
	r := e.q(query, args...)
	if len(r) != 1 {
		e.t.Fatalf("%s: got %v", query, r)
	}
	return r[0]
}

// credits lists "artist:via" for the last linked listen's recording.
func (e *env) lastCredits() string {
	e.t.Helper()
	got := e.q(`SELECT a.name || ':' || ra.via FROM listens l
		JOIN recording_artists ra ON ra.recording_id = l.recording_id JOIN artists a ON a.id = ra.artist_id
		WHERE l.listened_at = ?`, e.ts)
	sort.Strings(got)
	return strings.Join(got, ", ")
}

func TestLinksAndReuses(t *testing.T) {
	e := newEnv(t)
	e.scrobble("YOASOBI", "アイドル", "アイドル")
	e.scrobble("ＹＯＡＳＯＢＩ", "アイドル", "")     // width differs, no album
	e.scrobble("yoasobi", "アイドル", "アイドル") // case differs
	if n := e.one(`SELECT count(*) FROM recordings`); n != "1" {
		t.Fatalf("recordings = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM artists`); n != "1" {
		t.Fatalf("artists = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM listens WHERE recording_id IS NULL`); n != "0" {
		t.Fatalf("unlinked listens = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM listens WHERE release_id IS NOT NULL`); n != "2" {
		t.Fatalf("listens with album = %s", n)
	}
	if got := e.lastCredits(); got != "YOASOBI:credited" {
		t.Fatal(got)
	}
	// Automatic links are in the change log, marked automatic.
	if n := e.one(`SELECT count(*) FROM edits WHERE kind = 'link' AND automatic = 1`); n != "3" {
		t.Fatalf("link edits = %s", n)
	}
}

func TestCharacterCredits(t *testing.T) {
	e := newEnv(t)
	e.scrobble("後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "")
	if got := e.lastCredits(); got != "後藤ひとり:credited, 青山吉能:credited" {
		t.Fatal(got)
	}
	labelled := e.q(`SELECT a.name FROM entity_labels el JOIN labels l ON l.id = el.label_id
		JOIN artists a ON a.id = el.entity_id WHERE l.name = 'Character' AND el.entity_type = 'artist'`)
	if strings.Join(labelled, ",") != "後藤ひとり" {
		t.Fatalf("labelled %v", labelled)
	}
	// The same voice actor credited plainly elsewhere is the same artist.
	e.scrobble("青山吉能", "Solo Song", "")
	if n := e.one(`SELECT count(*) FROM artists WHERE name = '青山吉能'`); n != "1" {
		t.Fatalf("voice actor duplicated: %s", n)
	}
	// A list of characters from a band credit.
	e.scrobble("Hitori Gotoh (CV: Yoshino Aoyama), Nijika Ijichi (CV: Sayumi Suzushiro)", "Seisyun Complex", "Kessoku Band")
	if got := e.lastCredits(); got != "Hitori Gotoh:credited, Nijika Ijichi:credited, Sayumi Suzushiro:credited, Yoshino Aoyama:credited" {
		t.Fatal(got)
	}
}

func TestCharacterLabelDeletedByOwner(t *testing.T) {
	e := newEnv(t)
	e.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM labels WHERE name = 'Character'`)
		return err
	})
	e.scrobble("後藤ひとり(CV:青山吉能)", "x", "")
	if got := e.lastCredits(); got != "後藤ひとり:credited, 青山吉能:credited" {
		t.Fatal(got)
	}
}

func TestFeaturedArtists(t *testing.T) {
	e := newEnv(t)
	e.scrobble("LiSA feat. Uru", "Leo", "")
	if got := e.lastCredits(); got != "LiSA:credited, Uru:credited" {
		t.Fatal(got)
	}
	roles := e.q(`SELECT a.name || ':' || rc.role FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id ORDER BY rc.position`)
	if strings.Join(roles, ", ") != "LiSA:main, Uru:featured" {
		t.Fatal(roles)
	}
	// The same song scrobbled without the feat. part is the same recording.
	e.scrobble("LiSA", "Leo", "")
	if n := e.one(`SELECT count(*) FROM recordings`); n != "1" {
		t.Fatalf("recordings = %s", n)
	}
}

func TestSameSongOnSeveralAlbums(t *testing.T) {
	e := newEnv(t)
	// A single, the album it's on and an OST edition with another name are
	// one song on three albums.
	e.scrobble("結束バンド", "Distortion!!", "Distortion!!")
	e.scrobble("結束バンド", "Distortion!!", "結束バンド")
	e.scrobble("結束バンド", "Distortion!!", "")
	if n := e.one(`SELECT count(*) FROM recordings`); n != "1" {
		t.Fatalf("recordings = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM release_tracks`); n != "2" {
		t.Fatalf("release tracks = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM listens WHERE recording_id IS NULL`); n != "0" {
		t.Fatalf("unlinked = %s", n)
	}
}

func TestOwnerSplitSongs(t *testing.T) {
	e := newEnv(t)
	// Two games' "Main Theme" start as one song…
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Star Citizen OST")
	// …until the owner splits them (milestone 3), here done directly.
	err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		artist, err := store.FindArtistTx(context.Background(), tx, e.user, "Pedro Macedo Camacho")
		if err != nil {
			return err
		}
		rec, err := store.CreateRecordingTx(context.Background(), tx, e.user, "Main Theme", []store.Credit{{ArtistID: artist, Role: "main"}})
		if err != nil {
			return err
		}
		rel, err := store.CreateReleaseTx(context.Background(), tx, e.user, "Other Game OST", []int64{artist})
		if err != nil {
			return err
		}
		return store.AddReleaseTrackTx(context.Background(), tx, rel, rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Now the album decides.
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Other Game OST")
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Star Citizen OST")
	if n := e.one(`SELECT count(DISTINCT recording_id) FROM listens`); n != "2" {
		t.Fatalf("distinct recordings in listens = %s", n)
	}
	// Without an album, or on a third album, the owner picks.
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "")
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Best of")
	if n := e.one(`SELECT count(*) FROM listens WHERE recording_id IS NULL`); n != "2" {
		t.Fatalf("unlinked = %s, want 2", n)
	}
}

func TestViewCountsAreNotAlbums(t *testing.T) {
	e := newEnv(t)
	for _, album := range []string{"33M plays", "41M plays", "1,234 views", "2.1万回再生", "12 plays"} {
		e.scrobble("Hoshimachi Suisei", "BIBBIDIBA", album)
	}
	e.scrobble("Hoshimachi Suisei", "BIBBIDIBA", "Specter")
	if n := e.one(`SELECT count(*) FROM recordings`); n != "1" {
		t.Fatalf("recordings = %s", n)
	}
	if got := e.one(`SELECT group_concat(name) FROM releases`); got != "Specter" {
		t.Fatalf("albums: %s", got)
	}
	// Real albums that start with a number are albums.
	for _, album := range []string{"24K Magic", "1989", "100 Plays of Summer"} {
		if albumText(album) != album {
			t.Errorf("%q taken for a view count", album)
		}
	}
	// The received text is kept.
	if n := e.one(`SELECT count(*) FROM sources WHERE album_text = '33M plays'`); n != "1" {
		t.Fatal("received album text not kept")
	}
}

func TestSingleThenAlbum(t *testing.T) {
	e := newEnv(t)
	// First heard without an album, then from the album: same recording.
	e.scrobble("ClariS", "コネクト", "")
	e.scrobble("ClariS", "コネクト", "BIRTHDAY")
	if n := e.one(`SELECT count(*) FROM recordings`); n != "1" {
		t.Fatalf("recordings = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM release_tracks`); n != "1" {
		t.Fatalf("release tracks = %s", n)
	}
}

func TestIncompleteStaysUnlinked(t *testing.T) {
	e := newEnv(t)
	e.scrobble("", "no artist sent", "")
	e.scrobble("Someone", "", "")
	if n := e.one(`SELECT count(*) FROM listens WHERE recording_id IS NULL`); n != "2" {
		t.Fatalf("unlinked = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM artists`); n != "0" {
		t.Fatalf("artists = %s", n)
	}
	if n := e.one(`SELECT count(*) FROM jobs`); n != "0" {
		t.Fatalf("jobs left = %s", n)
	}
}

func TestUndoAutomaticLink(t *testing.T) {
	e := newEnv(t)
	e.scrobble("YOASOBI", "アイドル", "")
	edit := e.one(`SELECT id FROM edits WHERE kind = 'link'`)
	id, _ := strconv.ParseInt(edit, 10, 64)
	if _, err := e.db.UndoEdit(context.Background(), e.user, id); err != nil {
		t.Fatal(err)
	}
	if n := e.one(`SELECT count(*) FROM listens WHERE recording_id IS NULL`); n != "1" {
		t.Fatal("listen still linked after undo")
	}
}

func TestGroupsAndOverrides(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scrobble("結束バンド", "ギターと孤独と蒼い惑星", "結束バンド")
	e.scrobble("後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "")
	e.scrobble("伊地知虹夏(CV:鈴代紗弓)", "Solo", "")
	id := func(name string) string { return e.one(`SELECT id FROM artists WHERE name = ?`, name) }
	band, hitori, nijika := id("結束バンド"), id("後藤ひとり"), id("伊地知虹夏")
	rec := e.one(`SELECT recording_id FROM listens ORDER BY listened_at LIMIT 1`)

	e.db.Write(ctx, func(tx *sql.Tx) error {
		tx.Exec(`INSERT INTO group_members VALUES (?, ?), (?, ?)`, band, hitori, band, nijika)
		return nil
	})
	rebuild := func() {
		e.db.Write(ctx, func(tx *sql.Tx) error {
			var r int64
			tx.QueryRow(`SELECT ?`, rec).Scan(&r)
			return store.RebuildRecordingArtistsTx(ctx, tx, r)
		})
	}
	credits := func() string {
		got := e.q(`SELECT a.name || ':' || ra.via FROM recording_artists ra JOIN artists a ON a.id = ra.artist_id WHERE ra.recording_id = ?`, rec)
		sort.Strings(got)
		return strings.Join(got, ", ")
	}
	rebuild()
	if got := credits(); got != "伊地知虹夏:group, 後藤ひとり:group, 結束バンド:credited, 鈴代紗弓:group, 青山吉能:group" {
		t.Fatal(got)
	}
	// A lazy credit: only Hitori actually sings this one.
	e.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO credit_overrides VALUES ('recording', ?, ?, ?)`, rec, band, hitori)
		return err
	})
	rebuild()
	if got := credits(); got != "後藤ひとり:group, 結束バンド:credited, 青山吉能:group" {
		t.Fatal(got)
	}
}

func TestAlbumArtist(t *testing.T) {
	e := newEnv(t)
	// A soundtrack with tracks by different artists is one album.
	e.scrobbleAA("ClariS", "コネクト", "まどか☆マギカ OST", "Various Artists")
	e.scrobbleAA("Kalafina", "Magia", "まどか☆マギカ OST", "Various Artists")
	if n := e.one(`SELECT count(*) FROM releases`); n != "1" {
		t.Fatalf("releases = %s, want one soundtrack", n)
	}
	// "Various Artists" says nothing about who made it: the album is
	// credited to the artists heard on it (owner, 2026-10-01).
	if by := e.one(`SELECT group_concat(a.name) FROM release_credits rc JOIN artists a ON a.id = rc.artist_id`); by != "ClariS,Kalafina" {
		t.Fatalf("album by %s", by)
	}
	// The album artist isn't credited on the tracks.
	if got := e.lastCredits(); got != "Kalafina:credited" {
		t.Fatalf("track credits %s", got)
	}
	// Without an album artist, albums still go with the track artist.
	e.scrobble("ClariS", "irony", "Single")
	e.scrobble("Kalafina", "Lacrimosa", "Single")
	if n := e.one(`SELECT count(*) FROM releases WHERE name = 'Single'`); n != "2" {
		t.Fatalf("singles = %s, want one per artist", n)
	}
	// Album artists follow the same rules as track artists.
	e.scrobbleAA("Poppin'Party", "ときめきエクスペリエンス！", "BanG Dream! OST", "戸山香澄 (CV: 愛美)")
	if by := e.one(`SELECT a.name FROM releases r JOIN release_credits rc ON rc.release_id = r.id JOIN artists a ON a.id = rc.artist_id WHERE r.name = 'BanG Dream! OST'`); by != "戸山香澄" {
		t.Fatalf("album by %s", by)
	}
	// Nothing usable falls back to the track artist.
	e.scrobbleAA("ClariS", "ひとりごと", "Blank", "   ")
	if by := e.one(`SELECT a.name FROM releases r JOIN release_credits rc ON rc.release_id = r.id JOIN artists a ON a.id = rc.artist_id WHERE r.name = 'Blank'`); by != "ClariS" {
		t.Fatalf("album by %s", by)
	}
}
