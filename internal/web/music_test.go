package web

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"chokominto/internal/jobs"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

// scrobbleNow stores listens at recent times and links them.
func (e *env) scrobbleNow(entries ...[3]string) {
	e.t.Helper()
	var ls []store.NewListen
	now := time.Now().Unix()
	for i, en := range entries {
		ls = append(ls, store.NewListen{ListenedAt: now - int64(60*(i+1)), Artist: en[0], Title: en[1], Album: en[2], Payload: []byte(`{}`)})
	}
	ctx := context.Background()
	if _, err := e.db.InsertListens(ctx, e.userID, "listenbrainz", nil, ls); err != nil {
		e.t.Fatal(err)
	}
	r := &resolve.Resolver{DB: e.db}
	(&jobs.Runner{DB: e.db, Handlers: map[string]jobs.Handler{"resolve": r.Job}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Drain(ctx)
}

func TestMusicPages(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド"},
		[3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド"},
		[3]string{"YOASOBI", "アイドル", ""},
		[3]string{"", "no artist sent", ""},
	)

	// History links linked listens and shows the rest as received.
	_, body, _ := e.get("/history")
	if !regexp.MustCompile(`<a href="/song/\d+">ひとりぼっち東京</a>`).MatchString(body) {
		t.Fatal("history doesn't link songs")
	}
	if !strings.Contains(body, `class="unlinked"`) || !strings.Contains(body, "no artist sent") {
		t.Fatal("unlinked listen not shown as received")
	}

	// Home.
	code, body, _ := e.get("/")
	if code != 200 || !strings.Contains(body, "elaina’s listening") || !strings.Contains(body, "Top songs") {
		t.Fatalf("home: %d", code)
	}

	// Rankings, each with the period tabs.
	for _, p := range []string{"/top/songs", "/top/artists", "/top/albums", "/top/songs?period=all", "/top/artists?period=year", "/top/albums?period=custom&from=2020-01-01&to=2030-12-31"} {
		code, body, _ := e.get(p)
		if code != 200 || !strings.Contains(body, `aria-label="Period"`) {
			t.Errorf("%s: %d", p, code)
		}
	}
	// Characters are hidden from artists by default, their voice actor isn't.
	_, body, _ = e.get("/top/artists")
	if strings.Contains(body, ">後藤ひとり<") || !strings.Contains(body, ">青山吉能<") {
		t.Fatal("character filter default wrong")
	}
	_, body, _ = e.get("/top/artists?f=1")
	if !strings.Contains(body, ">後藤ひとり<") {
		t.Fatal("unticking Character doesn't show characters")
	}
	// Sorting by a column marks it.
	_, body, _ = e.get("/top/artists?sort=credited")
	if !strings.Contains(body, `aria-sort="descending"><a href="/top/artists?sort=credited"><strong>Credited ▼</strong>`) {
		t.Fatal("sorted column not marked")
	}

	// Entity pages.
	_, body, _ = e.get("/top/artists?f=1")
	id := regexp.MustCompile(`href="/artist/(\d+)">後藤ひとり`).FindStringSubmatch(body)[1]
	code, body, _ = e.get("/artist/" + id)
	if code != 200 || !strings.Contains(body, "Also counts for") || !strings.Contains(body, "青山吉能") || !strings.Contains(body, "Character") {
		t.Fatalf("artist page: %d", code)
	}
	songID := regexp.MustCompile(`href="/song/(\d+)"`).FindStringSubmatch(body)[1]
	if code, _, _ := e.get("/song/" + songID); code != 200 {
		t.Fatalf("song page: %d", code)
	}
	_, body, _ = e.get("/top/albums?period=all")
	albumID := regexp.MustCompile(`href="/album/(\d+)"`).FindStringSubmatch(body)[1]
	if code, body, _ := e.get("/album/" + albumID); code != 200 || !strings.Contains(body, "Tracks") {
		t.Fatalf("album page: %d", code)
	}
	for _, p := range []string{"/artist/99999", "/song/abc", "/album/0"} {
		if code, _, _ := e.get(p); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestCatchingUpNote(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	for i := range catchingUpAt {
		e.db.Enqueue(ctx, "resolve", fmt.Sprint("source:", 1000+i), "")
	}
	for _, path := range []string{"/", "/top/songs"} {
		if _, body, _ := e.get(path); !strings.Contains(body, "Still sorting out your history, with 25 songs to go.") {
			t.Fatalf("%s: no note while catching up", path)
		}
	}
	e.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM jobs WHERE key = 'source:1000'`); return err })
	if _, body, _ := e.get("/"); strings.Contains(body, "Still sorting") {
		t.Fatal("note shown for a few new listens")
	}
}
