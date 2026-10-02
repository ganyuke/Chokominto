package resolve

import (
	"context"
	"strconv"
	"testing"

	"chokominto/internal/jobs"
	"chokominto/internal/store"
)

func str(s string) *string { return &s }

func TestClean(t *testing.T) {
	yt := Text{"YOASOBI", "【MV】YOASOBI「アイドル」Official Music Video", ""}
	for _, c := range []struct {
		rule store.Rule
		in   Text
		want string
	}{
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "regex", Pattern: `^【MV】YOASOBI「(.*)」Official Music Video$`, ArtistMatch: str("YOASOBI")}, yt, "アイドル"},
		// Scoped to another artist: left alone.
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "regex", Pattern: `^【MV】YOASOBI「(.*)」Official Music Video$`, ArtistMatch: str("Ado")}, yt, yt.Title},
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "exact", Pattern: "【MV】"}, yt, "YOASOBI「アイドル」Official Music Video"},
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "suffix", Pattern: "Official Music Video"}, yt, "【MV】YOASOBI「アイドル」"},
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "prefix", Pattern: "Official"}, yt, yt.Title},
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "regex", Pattern: `\s*\(Official[^)]*\)`}, Text{"Ado", "唱 (Official Video)", ""}, "唱"},
		// A broken pattern does nothing.
		{store.Rule{Kind: "clean", Field: "title", MatchMode: "regex", Pattern: `(`}, yt, yt.Title},
	} {
		if got := Clean(c.in, []store.Rule{c.rule}).Title; got != c.want {
			t.Errorf("%+v: %q, want %q", c.rule, got, c.want)
		}
	}
}

func TestLinkRule(t *testing.T) {
	exact := store.Rule{ID: 1, Kind: "link", MatchMode: "exact", ArtistMatch: str("YOASOBI"), TitleMatch: str("Idol"), RecordingID: 7}
	key := exact
	key.MatchMode = "key"
	for _, c := range []struct {
		rule store.Rule
		in   Text
		want bool
	}{
		{exact, Text{"YOASOBI", "Idol", "THE BOOK 3"}, true},
		{exact, Text{"YOASOBI", "IDOL", ""}, false},
		{key, Text{"ＹＯＡＳＯＢＩ", "IDOL", ""}, true},
		{key, Text{"YOASOBI", "Idol!", ""}, true},
		{key, Text{"YOASOBI", "Idola", ""}, false},
	} {
		if _, got := LinkRule(c.in, []store.Rule{c.rule}); got != c.want {
			t.Errorf("%s %+v: %v", c.rule.MatchMode, c.in, got)
		}
	}
}

// withReparse lets the env's job runner run reparse jobs too.
func (e *env) withReparse() {
	r := &Resolver{DB: e.db}
	e.run = &jobs.Runner{DB: e.db, Handlers: map[string]jobs.Handler{"resolve": r.Job, "reparse": r.ReparseJob}, Log: e.run.Log}
}

func (e *env) id(query string, args ...any) int64 {
	e.t.Helper()
	n, err := strconv.ParseInt(e.one(query, args...), 10, 64)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) sourceOf(artist, title, album string) int64 {
	return e.id(`SELECT id FROM sources WHERE artist_text = ? AND title_text = ? AND album_text = ?`, artist, title, album)
}

func TestRememberAnyAlbum(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.withReparse()
	e.scrobble("YOASOBI", "アイドル", "THE BOOK 3")
	e.scrobble("YOASOBI", "Idol", "")
	e.scrobble("YOASOBI", "Idol", "Idol - Single")
	idol := e.id(`SELECT recording_id FROM sources WHERE title_text = 'アイドル'`)
	en := e.sourceOf("YOASOBI", "Idol", "")
	single := e.sourceOf("YOASOBI", "Idol", "Idol - Single")
	enRec := e.id(`SELECT recording_id FROM sources WHERE id = ?`, en)

	res, err := e.db.LinkSources(ctx, e.user, []int64{en}, idol)
	if err != nil {
		t.Fatal(err)
	}
	if res.Listens != 1 || res.Name != "アイドル" {
		t.Errorf("link result %+v", res)
	}
	offers, err := Offers(ctx, e.db, e.user, en, idol)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 || offers[0].Kind != "album" || offers[1].Kind != "key" {
		t.Fatalf("offers %+v", offers)
	}
	if offers[0].Listens != 1 {
		t.Errorf("any album would move %d other listens, want 1", offers[0].Listens)
	}
	edit, err := SaveOffer(ctx, e.db, e.user, offers[0], "Always link Idol by YOASOBI to アイドル")
	if err != nil {
		t.Fatal(err)
	}
	e.run.Drain(ctx)
	if got := e.id(`SELECT recording_id FROM sources WHERE id = ?`, single); got != idol {
		t.Fatalf("single's text went to %d, want %d", got, idol)
	}
	if got := e.id(`SELECT count(*) FROM listens WHERE recording_id = ?`, idol); got != 3 {
		t.Errorf("%d listens on アイドル", got)
	}
	// New text by the rule goes there straight away.
	e.scrobble("YOASOBI", "Idol", "THE BOOK 3")
	if got := e.id(`SELECT recording_id FROM sources WHERE id = ?`, e.sourceOf("YOASOBI", "Idol", "THE BOOK 3")); got != idol {
		t.Errorf("new text went to %d", got)
	}

	// Undoing the rule moves what it moved back. Text linked by hand stays.
	if _, err := Undo(ctx, e.db, e.user, edit); err != nil {
		t.Fatal(err)
	}
	e.run.Drain(ctx)
	if got := e.id(`SELECT recording_id FROM sources WHERE id = ?`, single); got != enRec {
		t.Errorf("after undo the single's text is on %d, want %d", got, enRec)
	}
	if got := e.id(`SELECT recording_id FROM sources WHERE id = ?`, en); got != idol {
		t.Errorf("text linked by hand moved to %d", got)
	}
}

func TestRememberTitlesLikeThis(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.withReparse()
	e.scrobble("YOASOBI", "アイドル", "")
	e.scrobble("YOASOBI", "【MV】YOASOBI「アイドル」Official Music Video", "")
	idol := e.id(`SELECT recording_id FROM sources WHERE title_text = 'アイドル'`)
	mv := e.sourceOf("YOASOBI", "【MV】YOASOBI「アイドル」Official Music Video", "")
	if _, err := e.db.LinkSources(ctx, e.user, []int64{mv}, idol); err != nil {
		t.Fatal(err)
	}
	offers, err := Offers(ctx, e.db, e.user, mv, idol)
	if err != nil {
		t.Fatal(err)
	}
	var clean *Offer
	for i := range offers {
		if offers[i].Kind == "clean" {
			clean = &offers[i]
		}
	}
	if clean == nil || clean.Prefix != "【MV】YOASOBI「" || clean.Suffix != "」Official Music Video" {
		t.Fatalf("offers %+v", offers)
	}
	if _, err := SaveOffer(ctx, e.db, e.user, *clean, "Remove the video title"); err != nil {
		t.Fatal(err)
	}
	e.scrobble("YOASOBI", "【MV】YOASOBI「群青」Official Music Video", "")
	if got := e.one(`SELECT s.name FROM listens l JOIN recordings r ON r.id = l.recording_id JOIN songs s ON s.id = r.song_id WHERE l.listened_at = ?`, e.ts); got != "群青" {
		t.Errorf("cleaned title %q", got)
	}
}

// Two songs share a title: rules on the title alone aren't offered.
func TestNoTitleRulesWhenAmbiguous(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Star Citizen")
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Other Game")
	song := e.id(`SELECT song_id FROM recordings LIMIT 1`)
	other := e.id(`SELECT recording_id FROM sources WHERE album_text = 'Other Game'`)
	if n := e.id(`SELECT count(*) FROM recordings`); n != 1 {
		t.Fatalf("%d recordings before the split", n)
	}
	// Split: the other game's listens get a song of their own.
	src := e.sourceOf("Pedro Macedo Camacho", "Main Theme", "Other Game")
	rec, _, err := NewSong(ctx, e.db, e.user, src)
	if err != nil {
		t.Fatal(err)
	}
	if rec == other || e.id(`SELECT song_id FROM recordings WHERE id = ?`, rec) == song {
		t.Fatal("no new song")
	}
	offers, err := Offers(ctx, e.db, e.user, src, rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offers {
		if o.Kind == "album" || o.Kind == "key" {
			t.Errorf("offered %s", o.Kind)
		}
	}
}

func TestNewSongUndo(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scrobble("Ayase", "アイドル", "")
	src := e.sourceOf("Ayase", "アイドル", "")
	before := e.q(`SELECT id || ' ' || recording_id FROM sources`)
	songs := e.id(`SELECT count(*) FROM songs`)
	_, res, err := NewSong(ctx, e.db, e.user, src)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.id(`SELECT count(*) FROM songs`); n != songs+1 {
		t.Fatalf("%d songs", n)
	}
	if _, err := Undo(ctx, e.db, e.user, res.EditID); err != nil {
		t.Fatal(err)
	}
	if n := e.id(`SELECT count(*) FROM songs`); n != songs {
		t.Errorf("%d songs after undo", n)
	}
	if got := e.q(`SELECT id || ' ' || recording_id FROM sources`); len(got) != len(before) || got[0] != before[0] {
		t.Errorf("sources %v, want %v", got, before)
	}
}
