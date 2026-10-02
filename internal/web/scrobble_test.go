package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestManualScrobble(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "アイドル", "アイドル"},
		[3]string{"Pedro Macedo Camacho", "Main Theme", "Star Citizen OST"},
		[3]string{"Pedro Macedo Camacho", "Main Theme", "Other Game OST"},
	)
	if code, _, _ := e.get("/scrobble"); code != http.StatusSeeOther {
		t.Fatalf("scrobble page without login: %d", code)
	}
	e.login()

	// Romaji finds kana, and so do kana and the artist.
	for _, q := range []string{"aidoru", "アイドル", "あいどる", "yoasobi"} {
		_, body, _ := e.get("/scrobble?q=" + url.QueryEscape(q))
		if !strings.Contains(body, ">アイドル</a>") {
			t.Errorf("search %q didn't find アイドル", q)
		}
	}
	_, body, _ := e.get("/scrobble?q=zzzz")
	if !strings.Contains(body, "Nothing matches") {
		t.Error("no-results message missing")
	}

	_, body, _ = e.get("/scrobble?q=aidoru")
	rec := regexp.MustCompile(`name="recording" value="(\d+)"`).FindStringSubmatch(body)[1]
	code, _, h := e.post("/scrobble", url.Values{"q": {"aidoru"}, "recording": {rec}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "notice=scrobbled") {
		t.Fatalf("scrobble now: %d %s", code, h.Get("Location"))
	}
	past := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04")
	if code, _, _ := e.post("/scrobble", url.Values{"recording": {rec}, "at": {past}}); code != http.StatusSeeOther {
		t.Fatalf("scrobble at a time: %d", code)
	}
	future := time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02T15:04")
	if code, body, _ := e.post("/scrobble", url.Values{"recording": {rec}, "at": {future}}); code != 400 || !strings.Contains(body, "in the future") {
		t.Fatalf("future time: %d", code)
	}
	if code, _, _ := e.post("/scrobble", url.Values{"recording": {"999999"}}); code != 400 {
		t.Fatalf("missing recording: %d", code)
	}

	// Both manual listens are on the recording that was picked.
	var n int
	e.db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE origin = 'manual' AND recording_id = ?`, rec).Scan(&n)
	if n != 2 {
		t.Fatalf("manual listens on the recording: %d", n)
	}

	// A song on two albums is one result, and its listen goes to that song.
	_, body, _ = e.get("/scrobble?q=main+theme")
	recs := regexp.MustCompile(`name="recording" value="(\d+)"`).FindAllStringSubmatch(body, -1)
	if len(recs) != 1 {
		t.Fatalf("expected one Main Theme, got %d", len(recs))
	}
	if code, _, _ := e.post("/scrobble", url.Values{"recording": {recs[0][1]}}); code != http.StatusSeeOther {
		t.Fatalf("main theme: %d", code)
	}
	e.db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE recording_id = ?`, recs[0][1]).Scan(&n)
	if n != 3 {
		t.Fatalf("main theme has %d listens, want 3", n)
	}
	// Totals include manual listens.
	if total, _, _, _ := e.db.ListenStats(context.Background(), e.userID); total != 6 {
		t.Fatalf("listen count %d", total)
	}
}

func TestSettingsWeekStartAndLabels(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", ""})

	// Week start changes the Week tab.
	e.post("/settings/time-zone", url.Values{"time_zone": {"UTC"}, "week_start": {"0"}})
	u, _ := e.db.UserByID(context.Background(), e.userID)
	if u.WeekStart != 0 {
		t.Fatal("week start not saved")
	}

	_, body, _ := e.get("/settings")
	id := regexp.MustCompile(`action="/settings/labels/(\d+)"`).FindStringSubmatch(body)[1]
	// Show characters by default: they appear in Top artists without unticking.
	code, _, _ := e.post("/settings/labels/"+id, url.Values{"name": {"Character"}})
	if code != http.StatusSeeOther {
		t.Fatalf("label save: %d", code)
	}
	_, body, _ = e.get("/top/artists")
	if !strings.Contains(body, ">後藤ひとり<") {
		t.Fatal("character still hidden after changing the default")
	}
	if code, _, _ := e.post("/settings/labels/"+id, url.Values{"name": {" "}}); code != 400 {
		t.Fatalf("empty name: %d", code)
	}
	// The change is undoable from Changes.
	_, body, _ = e.get("/changes")
	if !strings.Contains(body, "Label Character now shown in rankings by default") {
		t.Fatal("label change not in Changes")
	}
}

func TestDeleteLabelAndUndo(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", ""})
	shown := func() bool {
		_, body, _ := e.get("/top/artists")
		return strings.Contains(body, ">後藤ひとり<")
	}
	if shown() {
		t.Fatal("character shown before deleting the label")
	}

	_, body, _ := e.get("/settings")
	id := regexp.MustCompile(`action="/settings/labels/(\d+)/delete"`).FindStringSubmatch(body)[1]
	code, _, h := e.post("/settings/labels/"+id+"/delete", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	_, body, _ = e.get(h.Get("Location"))
	undo := regexp.MustCompile(`Label deleted\. <form class="inline" method="post" action="(/changes/\d+/undo)"`).FindStringSubmatch(body)
	if undo == nil {
		t.Fatal("no Undo after deleting")
	}
	if strings.Contains(body, `action="/settings/labels/`+id+`"`) {
		t.Fatal("deleted label still listed")
	}
	if !shown() {
		t.Fatal("character still hidden with its label gone")
	}
	if code, _, _ := e.post("/settings/labels/"+id+"/delete", nil); code != http.StatusNotFound {
		t.Fatalf("deleting twice: %d", code)
	}

	if code, _, _ := e.post(undo[1], nil); code != http.StatusSeeOther {
		t.Fatalf("undo: %d", code)
	}
	if shown() {
		t.Fatal("character shown after the label came back")
	}

	// Once the name is taken again, the deletion can't be undone.
	e.post("/settings/labels/"+id+"/delete", nil)
	es, _ := e.db.Edits(context.Background(), e.userID, 0, 1)
	e.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO labels (user_id, name) VALUES (?, 'Character')`, e.userID)
		return err
	})
	code, body, _ = e.post(fmt.Sprintf("/changes/%d/undo", es[0].ID), nil)
	if code != http.StatusConflict || !strings.Contains(body, "This can&#39;t be undone anymore.") {
		t.Fatalf("undo onto a taken name: %d", code)
	}
}
