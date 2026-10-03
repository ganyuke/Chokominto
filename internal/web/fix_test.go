package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"chokominto/internal/resolve"
)

func (e *env) listenID(title, album string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.Reader().QueryRow(`SELECT l.id FROM listens l JOIN sources s ON s.id = l.source_id WHERE s.title_text = ? AND s.album_text = ?`, title, album).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) recordingOf(listen int64) int64 {
	e.t.Helper()
	var id int64
	e.db.Reader().QueryRow(`SELECT coalesce(recording_id, 0) FROM listens WHERE id = ?`, listen).Scan(&id)
	return id
}

func TestFixPage(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "アイドル", ""},
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Idol", "Idol - Single"},
	)
	idol := e.listenID("アイドル", "")
	en := e.listenID("Idol", "THE BOOK 3")
	target := e.recordingOf(idol)

	// Logged out: no Fix links, and the page needs a login.
	if _, body, _ := e.get("/history"); strings.Contains(body, "/fix") {
		t.Fatal("Fix link shown to a visitor")
	}
	if code, _, _ := e.get(fmt.Sprintf("/listen/%d/fix", en)); code != http.StatusSeeOther {
		t.Fatalf("fix page logged out: %d", code)
	}
	e.login()
	if _, body, _ := e.get("/history"); !strings.Contains(body, fmt.Sprintf(`href="/listen/%d/fix">Fix</a>`, en)) {
		t.Fatal("no Fix link in History")
	}
	if _, body, _ := e.get("/"); !strings.Contains(body, ">Fix</a>") {
		t.Fatal("no Fix link on Home")
	}

	code, body, _ := e.get(fmt.Sprintf("/listen/%d/fix", en))
	if code != 200 || !strings.Contains(body, "THE BOOK 3") {
		t.Fatalf("fix page: %d", code)
	}
	// The search starts from the received title. アイドル is found by its
	// romaji only if the owner types it, so search for it.
	_, body, _ = e.get(fmt.Sprintf("/listen/%d/fix?q=aidoru", en))
	if !strings.Contains(body, fmt.Sprintf(`name="recording" value="%d"`, target)) {
		t.Fatal("search didn't find アイドル")
	}

	code, _, h := e.post(fmt.Sprintf("/listen/%d/link", en), url.Values{"recording": {fmt.Sprint(target)}})
	if code != http.StatusSeeOther {
		t.Fatalf("link: %d", code)
	}
	if got := e.recordingOf(en); got != target {
		t.Fatalf("listen on %d, want %d", got, target)
	}
	_, body, _ = e.get(h.Get("Location"))
	if !strings.Contains(body, "Moved 1 listen from Idol to アイドル.") || !strings.Contains(body, `name="back"`) {
		t.Fatal("no notice with Undo")
	}
	if !strings.Contains(body, "Rules for similar listens") || !strings.Contains(body, "Always link “Idol” by YOASOBI here, whatever the album") ||
		!strings.Contains(body, "(moves 1 more listen)") {
		t.Fatalf("no offers:\n%s", body)
	}

	code, _, h = e.post(fmt.Sprintf("/listen/%d/remember", en), url.Values{"kind": {"album"}, "recording": {fmt.Sprint(target)}})
	if code != http.StatusSeeOther {
		t.Fatalf("remember: %d", code)
	}
	var rules int
	e.db.Reader().QueryRow(`SELECT count(*) FROM rules WHERE kind = 'link'`).Scan(&rules)
	if rules != 1 {
		t.Fatalf("%d link rules", rules)
	}
	// A kind that wasn't offered is refused.
	if code, _, _ := e.post(fmt.Sprintf("/listen/%d/remember", en), url.Values{"kind": {"regex"}, "recording": {fmt.Sprint(target)}}); code != http.StatusBadRequest {
		t.Fatalf("made-up offer: %d", code)
	}

	// Undo from the notice comes back to the Fix page.
	undo := regexp.MustCompile(`action="/changes/(\d+)/undo"`).FindStringSubmatch(func() string { _, b, _ := e.get(h.Get("Location")); return b }())
	if undo == nil {
		t.Fatal("no undo button")
	}
	code, _, h = e.post("/changes/"+undo[1]+"/undo", url.Values{"back": {fmt.Sprintf("/listen/%d/fix", en)}})
	if code != http.StatusSeeOther || h.Get("Location") != fmt.Sprintf("/listen/%d/fix?notice=undone", en) {
		t.Fatalf("undo went to %s", h.Get("Location"))
	}
	// Only local paths.
	_, _, h = e.post("/changes/"+undo[1]+"/undo", url.Values{"back": {"//evil.example/"}})
	if loc := h.Get("Location"); strings.Contains(loc, "evil") {
		t.Fatalf("undo redirected to %s", loc)
	}

	// New song, then delete.
	if code, _, _ := e.post(fmt.Sprintf("/listen/%d/new-song", idol), nil); code != http.StatusSeeOther {
		t.Fatalf("new song: %d", code)
	}
	if got := e.recordingOf(idol); got == target || got == 0 {
		t.Fatal("not split off")
	}
	code, _, h = e.post(fmt.Sprintf("/listen/%d/delete", idol), nil)
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/history?done=") {
		t.Fatalf("delete: %d %s", code, h.Get("Location"))
	}
	_, body, _ = e.get(h.Get("Location"))
	if !strings.Contains(body, "Deleted listen: アイドル by YOASOBI.") {
		t.Fatal("no delete notice")
	}
	if code, _, _ := e.get(fmt.Sprintf("/listen/%d/fix", idol)); code != 404 {
		t.Fatalf("deleted listen's fix page: %d", code)
	}
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 2 {
		t.Fatalf("%d listens left", n)
	}
}

func TestOfferTextHasNoSemicolons(t *testing.T) {
	for _, s := range []string{
		offerText(offerFor("album"), "A", "T"), offerText(offerFor("key"), "A", "T"), offerText(offerFor("clean"), "A", "T"),
	} {
		if strings.Contains(s, ";") {
			t.Errorf("%q", s)
		}
	}
}

func offerFor(kind string) resolve.Offer {
	return resolve.Offer{Kind: kind, Prefix: "【MV】", Suffix: "(Official)"}
}
