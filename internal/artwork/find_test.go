package artwork

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chokominto/internal/fetch"
	"chokominto/internal/store"
)

type fakeServices struct {
	t      *testing.T
	srv    *httptest.Server
	down   bool // iTunes and Deezer answer 500
	asked  []string
	itunes string // JSON for iTunes searches
	deezer string // JSON for Deezer album searches
	artist string // JSON for Deezer artist searches
}

func newFake(t *testing.T) *fakeServices {
	f := &fakeServices{t: t,
		itunes: `{"results":[]}`, deezer: `{"data":[]}`, artist: `{"data":[]}`}
	jpg := encoded(t, picture(600, 600, 255), "jpeg")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.asked = append(f.asked, r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case strings.HasPrefix(r.URL.Path, "/release/") && strings.HasSuffix(r.URL.Path, "/front-1200"):
			http.Redirect(w, r, "/img/caa.jpg", http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/img/broken"):
			w.Write([]byte("<html>not a picture</html>"))
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Write(jpg)
		case f.down:
			http.Error(w, "down", 500)
		case r.URL.Path == "/search":
			w.Write([]byte(f.itunes))
		case r.URL.Path == "/search/album":
			w.Write([]byte(f.deezer))
		case r.URL.Path == "/search/artist":
			w.Write([]byte(f.artist))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type env struct {
	t      *testing.T
	db     *store.DB
	f      *Finder
	fake   *fakeServices
	user   int64
	now    time.Time
	artist int64
}

func newEnv(t *testing.T) *env {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "a.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(ctx, "elaina", "h")
	fake := newFake(t)
	host, _ := url.Parse(fake.srv.URL)
	e := &env{t: t, db: db, fake: fake, user: u, now: time.Unix(1_800_000_000, 0)}
	e.f = &Finder{DB: db, Store: Store{Dir: filepath.Join(dir, "artwork")}, Fetch: fetch.NewLocal("test", host.Hostname()),
		Sources: Sources{fake.srv.URL, fake.srv.URL, fake.srv.URL}, Now: func() time.Time { return e.now }}
	db.Write(ctx, func(tx *sql.Tx) error {
		e.artist, err = store.CreateArtistTx(ctx, tx, u, "YOASOBI")
		return err
	})
	return e
}

func (e *env) release(title, mbid string) int64 {
	e.t.Helper()
	ctx := context.Background()
	var id int64
	err := e.db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = store.CreateReleaseTx(ctx, tx, e.user, title, []int64{e.artist}); err != nil {
			return err
		}
		if mbid != "" {
			_, err = tx.Exec(`UPDATE releases SET mbid = ? WHERE id = ?`, mbid, id)
		}
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) run(kind string, id int64, try int) {
	e.t.Helper()
	if err := e.f.Job(context.Background(), store.Job{Kind: "artwork", Key: fmt.Sprintf("%s:%d:%d", kind, id, try)}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state(kind string, id int64) string {
	s, _ := e.db.Lookup(context.Background(), kind, id)
	return s
}

func (e *env) nextTry(kind string, id int64) time.Duration {
	var at int64
	e.db.Reader().QueryRow(`SELECT max(run_after) FROM jobs WHERE kind = 'artwork' AND key LIKE ?`, fmt.Sprintf("%s:%d:%%", kind, id)).Scan(&at)
	return time.Unix(at, 0).Sub(e.now).Round(time.Minute)
}

func TestCoverArtArchiveByMBID(t *testing.T) {
	e := newEnv(t)
	rel := e.release("THE BOOK 3", "11111111-1111-4111-8111-111111111111")
	e.run("release", rel, 0)
	a, pinned, err := e.db.ItemArtwork(context.Background(), "release", rel)
	if err != nil || a == nil || pinned || a.Origin != "coverartarchive" {
		t.Fatalf("artwork %+v %v %v", a, pinned, err)
	}
	if !e.f.Store.Has(a.SHA256, a.Format) || e.state("release", rel) != "found" {
		t.Fatal("not stored")
	}
	// Found pictures are never looked for again.
	n := len(e.fake.asked)
	e.run("release", rel, 1)
	if len(e.fake.asked) != n {
		t.Fatal("looked again after finding one")
	}
}

func TestITunesMatchAndCandidates(t *testing.T) {
	e := newEnv(t)
	img := e.fake.srv.URL + "/img/"
	// Japan has a different album first and then this one in full-width
	// letters, which still matches.
	e.fake.itunes = `{"results":[
		{"collectionName":"THE BOOK 2","artistName":"YOASOBI","artworkUrl100":"` + img + `book2/100x100bb.jpg"},
		{"collectionName":"ＴＨＥ ＢＯＯＫ ３","artistName":"ＹＯＡＳＯＢＩ","artworkUrl100":"` + img + `book3/100x100bb.jpg"}]}`
	rel := e.release("THE BOOK 3", "")
	e.run("release", rel, 0)
	if a, _, _ := e.db.ItemArtwork(context.Background(), "release", rel); a == nil || a.Origin != "itunes" {
		t.Fatalf("not found on iTunes: %+v", a)
	}
	if !strings.Contains(strings.Join(e.fake.asked, " "), "1200x1200bb") {
		t.Errorf("didn't ask for the large picture: %v", e.fake.asked)
	}

	// Nothing matches: the results are kept to choose from, and it's looked
	// for again in a month.
	other := e.release("Unrelated Album", "")
	e.run("release", other, 0)
	if e.state("release", other) != "candidates" {
		t.Fatalf("state %q", e.state("release", other))
	}
	cs, _ := e.db.Candidates(context.Background(), "release", other)
	if len(cs) != 2 || cs[0].Title != "THE BOOK 2" || !strings.HasSuffix(cs[0].Thumb, "100x100bb.jpg") {
		t.Fatalf("candidates %+v", cs)
	}
	if got := e.nextTry("release", other); got != 30*24*time.Hour {
		t.Errorf("next try in %v", got)
	}
}

func TestBrokenPictureSkipped(t *testing.T) {
	e := newEnv(t)
	img := e.fake.srv.URL + "/img/"
	e.fake.itunes = `{"results":[{"collectionName":"THE BOOK 3","artistName":"YOASOBI","artworkUrl100":"` + img + `broken/100x100bb.jpg"}]}`
	e.fake.deezer = `{"data":[{"title":"THE BOOK 3","artist":{"name":"YOASOBI"},"cover_xl":"` + img + `deezer.jpg"}]}`
	rel := e.release("THE BOOK 3", "")
	e.run("release", rel, 0)
	if a, _, _ := e.db.ItemArtwork(context.Background(), "release", rel); a == nil || a.Origin != "deezer" {
		t.Fatalf("broken picture not skipped: %+v", a)
	}
}

func TestOutageRetried(t *testing.T) {
	e := newEnv(t)
	e.fake.down = true
	rel := e.release("THE BOOK 3", "")
	for try, want := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour} {
		e.run("release", rel, try)
		if e.state("release", rel) != "error" {
			t.Fatalf("state %q", e.state("release", rel))
		}
		if got := e.nextTry("release", rel); got != want {
			t.Errorf("after %d tries, next in %v, want %v", try+1, got, want)
		}
	}
}

func TestArtistPicture(t *testing.T) {
	e := newEnv(t)
	img := e.fake.srv.URL + "/img/"
	e.fake.artist = `{"data":[
		{"name":"YOASOBI","picture_xl":"https://e-cdns-images.dzcdn.net/images/artist//1000x1000-000000-80-0-0.jpg"},
		{"name":"YOASOBI","picture_xl":"` + img + `yoasobi.jpg"}]}`
	e.run("artist", e.artist, 0)
	if a, _, _ := e.db.ItemArtwork(context.Background(), "artist", e.artist); a == nil || a.Origin != "deezer" {
		t.Fatalf("artist picture %+v", a)
	}
}

func TestChoiceAndSwitchOffRespected(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	img := e.fake.srv.URL + "/img/"
	e.fake.deezer = `{"data":[{"title":"THE BOOK 3","artist":{"name":"YOASOBI"},"cover_xl":"` + img + `x.jpg"}]}`

	off := e.release("THE BOOK 3", "")
	e.db.SetFindArtwork(ctx, e.user, false)
	e.run("release", off, 0)
	if len(e.fake.asked) != 0 {
		t.Fatalf("asked with lookups off: %v", e.fake.asked)
	}
	e.db.SetFindArtwork(ctx, e.user, true)

	// A picture the owner chose is never replaced.
	a, err := e.f.Download(ctx, "upload", img+"mine.jpg")
	if err != nil {
		t.Fatal(err)
	}
	e.db.Write(ctx, func(tx *sql.Tx) error {
		id, err := store.AddArtworkTx(ctx, tx, a, "")
		if err != nil {
			return err
		}
		_, err = store.SetArtworkTx(ctx, tx, e.user, "release", off, id, true, false)
		return err
	})
	n := len(e.fake.asked)
	e.run("release", off, 0)
	if len(e.fake.asked) != n {
		t.Fatal("looked for a picture the owner chose")
	}
}

func TestMatching(t *testing.T) {
	item := store.ArtworkItem{
		NameKeys:   NameKeysOf([]string{"リテラチュア", "Genshin Impact - City of Winds and Idylls", "煌めく浜辺", "Doki Doki Literature Club! Official Soundtrack"}),
		ArtistKeys: map[string]bool{"上田麗奈": true, "yupengchen": true, "大原ゆい子": true},
	}
	item.NameKeys["kawakioameku"] = true // romaji of カワキヲアメク, を as "o"
	item.ArtistKeys["minami"] = true
	item.ArtistKeys["dansalvato"] = true
	for _, c := range []struct {
		title, artist string
		want          bool
	}{
		{"リテラチュア - Single", "上田麗奈", true},
		{"Genshin Impact - City of Winds and Idylls (Original Game Soundtrack)", "Yu-Peng Chen & HOYO-MiX", true},
		{"煌めく浜辺(アーティスト盤)", "大原ゆい子", true},
		{"Kawakiwoameku - EP", "minami", true},
		{"Doki Doki Literature Club! (Original Soundtrack)", "Dan Salvato", true},
		{"リテラチュア", "Someone Else", false},
		{"リテラチュア 2", "上田麗奈", false},
		{"Literature", "上田麗奈", false},
	} {
		if got := matches("release", item, Found{Origin: "itunes", Title: c.title, Artist: c.artist}); got != c.want {
			t.Errorf("%q by %q: %v", c.title, c.artist, got)
		}
	}
}
