package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestReview(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "アイドル", ""},
		[3]string{"YOASOBI", "アイドル", ""},
		[3]string{"YOASOBI", "Aidoru", ""},
		[3]string{"YOASOBI", "Idol", ""},
		[3]string{"YOASOBI", "IDOL (MV)", ""},
		[3]string{"", "no artist sent", ""},
	)
	if err := e.db.FindSuggestions(ctx, e.userID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.get("/review"); code != http.StatusSeeOther {
		t.Fatalf("review logged out: %d", code)
	}
	e.login()
	code, body, _ := e.get("/review")
	if code != 200 || !strings.Contains(body, "One name is the romaji of the other") || !strings.Contains(body, "no artist sent") {
		t.Fatalf("review: %d\n%s", code, body)
	}
	if !strings.Contains(body, `href="/review" aria-current="page">Review</a>`) {
		t.Fatal("no Review link in the top bar")
	}

	// Answer every suggestion "same" at once: one edit.
	ids := regexp.MustCompile(`name="suggestion" value="(\d+)"`).FindAllStringSubmatch(body, -1)
	form := url.Values{"answer": {"same"}}
	for _, m := range ids {
		form.Add("suggestion", m[1])
	}
	code, _, h := e.post("/review/suggestions", form)
	if code != http.StatusSeeOther {
		t.Fatalf("answer: %d", code)
	}
	_, body, _ = e.get(h.Get("Location"))
	if !strings.Contains(body, fmt.Sprintf("Merged %d suggested pairs.", len(ids))) {
		t.Fatalf("no notice for %d pairs", len(ids))
	}
	var songs int
	e.db.Reader().QueryRow(`SELECT count(*) FROM songs WHERE merged_into IS NULL AND EXISTS (SELECT 1 FROM recordings r WHERE r.song_id = songs.id)`).Scan(&songs)
	if songs != 2 { // アイドル with Aidoru, and Idol with IDOL (MV)
		t.Fatalf("%d songs left", songs)
	}

	// Link several spellings at once: find the text, then the song.
	_, body, _ = e.get("/review?t=idol")
	srcs := regexp.MustCompile(`name="s" value="(\d+)"`).FindAllStringSubmatch(body, -1)
	if len(srcs) < 2 {
		t.Fatalf("text search found %d", len(srcs))
	}
	q := url.Values{"t": {"idol"}, "q": {"アイドル"}}
	for _, m := range srcs {
		q.Add("s", m[1])
	}
	_, body, _ = e.get("/review?" + q.Encode())
	if strings.Count(body, " checked ") < len(srcs) {
		t.Fatal("checked text not kept across the song search")
	}
	rec := regexp.MustCompile(`name="recording" value="(\d+)">Link checked`).FindStringSubmatch(body)
	if rec == nil {
		t.Fatal("no song found")
	}
	form = url.Values{"recording": {rec[1]}}
	for _, m := range srcs {
		form.Add("s", m[1])
	}
	if code, _, _ := e.post("/review/link", form); code != http.StatusSeeOther {
		t.Fatalf("bulk link: %d", code)
	}
	var on int
	e.db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE recording_id = ?`, rec[1]).Scan(&on)
	if on != 5 {
		t.Fatalf("%d listens on the song, want 5", on)
	}

	if code, body, _ := e.post("/review/link", url.Values{"recording": {rec[1]}}); code != http.StatusBadRequest || !strings.Contains(body, "Check what to link first.") {
		t.Fatalf("nothing checked: %d", code)
	}
}
