package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"chokominto/internal/listenbrainz"
)

func (e *env) playing(t listenbrainz.Track) {
	e.srv.Config.Handler.(*Server).np.Set(e.userID, t, 0)
}

func (e *env) live(parts string) liveParts {
	e.t.Helper()
	code, body, h := e.get("/live?parts=" + parts)
	if code != 200 || h.Get("Content-Type") != "application/json" {
		e.t.Fatalf("live: %d %s", code, body)
	}
	var out liveParts
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestNowPlayingBox(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow([3]string{"YOASOBI", "アイドル", "THE BOOK 3"})
	var album int64
	e.db.Reader().QueryRow(`SELECT id FROM releases`).Scan(&album)
	e.login()
	if code, _ := e.upload(fmt.Sprintf("/album/%d/picture", album), jpegBytes(t, 500, 500)); code != 303 {
		t.Fatalf("upload: %d", code)
	}

	// Nothing playing: an empty place for it, no box.
	_, body, _ := e.get("/")
	if !strings.Contains(body, `<div data-live="playing" aria-live="polite"></div>`) {
		t.Fatal("no place for now playing")
	}

	// Heard before: the song, artist and album link to their pages, with
	// the cover.
	e.playing(listenbrainz.Track{Artist: "YOASOBI", Title: "アイドル", Album: "THE BOOK 3"})
	for _, path := range []string{"/", "/history"} {
		_, body, _ := e.get(path)
		box := regexp.MustCompile(`(?s)<section class="playing".*?</section>`).FindString(body)
		for _, want := range []string{`-440.jpg`, `<a href="/song/`, `<a href="/artist/`, fmt.Sprintf(`<a href="/album/%d">`, album)} {
			if !strings.Contains(box, want) {
				t.Errorf("%s: now playing lacks %s in %s", path, want, box)
			}
		}
	}
	// Another album or none still finds the song.
	e.playing(listenbrainz.Track{Artist: "YOASOBI", Title: "アイドル"})
	if p := *e.live("playing").Playing; !strings.Contains(p, `<a href="/song/`) || strings.Contains(p, "/album/") {
		t.Fatalf("same song without album: %s", p)
	}

	// Never heard: the text as sent, a plain square and no links.
	e.playing(listenbrainz.Track{Artist: "ずっと真夜中でいいのに。", Title: "残機", Album: "沈香学"})
	p := *e.live("playing").Playing
	if !strings.Contains(p, "残機") || !strings.Contains(p, "沈香学") || !strings.Contains(p, `class="no-art"`) || strings.Contains(p, "<a ") {
		t.Fatalf("unknown song: %s", p)
	}
}

func TestLiveParts(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	if out := e.live("playing,recent,history,sorting"); *out.Playing != "" || !strings.Contains(*out.Recent, "No listens yet.") || out.Sorting.Left != 0 {
		t.Fatalf("empty: %+v", out)
	}

	// New listens show up in Recent listens and on the newest History page.
	e.scrobbleNow([3]string{"YOASOBI", "アイドル", ""})
	out := e.live("recent,history")
	if !strings.Contains(*out.Recent, "アイドル") || !strings.Contains(*out.History, "アイドル") || out.Playing != nil || out.Sorting != nil {
		t.Fatalf("after a listen: %+v", out)
	}
	// What /live sends is what the page has, so nothing changes until
	// something does.
	_, body, _ := e.get("/history")
	if !strings.Contains(body, `<div data-live="history">`+*out.History+`</div>`) {
		t.Fatal("history part differs from the page")
	}
	_, body, _ = e.get("/")
	if !strings.Contains(body, `<div data-live="recent">`+*out.Recent+`</div>`) {
		t.Fatal("recent part differs from the page")
	}
	// Older History pages stay as they are.
	if _, body, _ := e.get("/history?before=1.1"); strings.Contains(body, `data-live="history"`) {
		t.Fatal("an older page updates itself")
	}

	// The sorting note counts down to nothing.
	for i := range catchingUpAt {
		e.db.Enqueue(ctx, "resolve", fmt.Sprint("source:", 1000+i), "")
	}
	if _, body, _ := e.get("/top/songs"); !strings.Contains(body, `data-live="sorting"`) {
		t.Fatal("no live sorting note")
	}
	e.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM jobs WHERE key != 'source:1000'`); return err })
	if s := e.live("sorting").Sorting; s.Left != 1 || s.Text != "Still sorting out your history, with 1 song to go. Rankings fill in as that finishes." {
		t.Fatalf("one left: %+v", s)
	}
	e.db.Write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM jobs`); return err })
	if s := e.live("sorting").Sorting; s.Left != 0 {
		t.Fatalf("done: %+v", s)
	}
}
