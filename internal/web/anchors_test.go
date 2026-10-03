package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"chokominto/internal/store"
)

func TestMarkForms(t *testing.T) {
	page := `<form method="post" action="/logout"><button>Log out</button></form>
<h2 id="labels">Labels</h2><form class="inline" method="post" action="/a"><input id="add-label"></form>
<form method="get" action="/search"></form>
<table><tr id="change-7"><td><form method="post" action="/b"></form></td></tr></table>`
	got := markForms(page)
	for _, want := range []string{
		`action="/logout"><button>`, // above every place: untouched
		`action="/a"><input type="hidden" name="at" value="labels">`,
		`<form method="get" action="/search"></form>`,
		`action="/b"><input type="hidden" name="at" value="change-7">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s in\n%s", want, got)
		}
	}
}

// A button brings the page back to where it was used.
func TestJumpBack(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow([3]string{"YOASOBI", "アイドル", "THE BOOK 3"})
	e.login()
	var artist int64
	e.db.Reader().QueryRow(`SELECT id FROM artists`).Scan(&artist)
	path := fmt.Sprintf("/artist/%d/edit", artist)
	_, body, _ := e.get(path)
	form := regexp.MustCompile(`<form[^>]*method="post"[^>]*><input type="hidden" name="at" value="all-names">`).FindString(body)
	if form == "" {
		t.Fatalf("the add-name form doesn't say where it is:\n%s", body)
	}
	post := func(to, referer string, form url.Values) string {
		t.Helper()
		req, _ := http.NewRequest("POST", e.srv.URL+to, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", e.srv.URL+referer)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.Header.Get("Location")
	}
	loc := post(path, path, url.Values{"do": {"add-name"}, "name": {"ヨアソビ"}, "at": {"all-names"}})
	if !strings.HasPrefix(loc, path+"?done=") || !strings.HasSuffix(loc, "#all-names") {
		t.Errorf("after adding a name: %s", loc)
	}
	// A redirect to another page, or a made-up place, gets none.
	if loc := post(path, "/settings", url.Values{"do": {"add-name"}, "name": {"よあそび"}, "at": {"all-names"}}); strings.Contains(loc, "#") {
		t.Errorf("from another page: %s", loc)
	}
	if loc := post(path, path, url.Values{"do": {"add-name"}, "name": {"夜遊び"}, "at": {`x"><script>`}}); strings.Contains(loc, "#") {
		t.Errorf("made-up place: %s", loc)
	}
}

// Changes pages are full of rows, however many changes a task holds.
func TestChangesPagesCountTasksOnce(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"YOASOBI", "アイドル", "THE BOOK 3"})
	var artist int64
	e.db.Reader().QueryRow(`SELECT id FROM artists`).Scan(&artist)
	rename := func(ctx context.Context, n int) {
		t.Helper()
		if _, err := e.db.Rename(ctx, e.userID, "artist", artist, fmt.Sprintf("YOASOBI %d", n)); err != nil {
			t.Fatal(err)
		}
	}
	rename(ctx, 0) // the oldest change, by itself
	// Three pages' worth of changes in one task.
	agent := store.InAgentTask(ctx)
	task, err := e.db.StartTask(agent, e.userID, "Many renames")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3*changesPageSize; i++ {
		rename(agent, i)
	}

	_, body, _ := e.get("/changes")
	if n := strings.Count(body, `<tr id="`); n != 2 {
		t.Fatalf("%d rows on the first page, want the task and the older change", n)
	}
	if !strings.Contains(body, fmt.Sprintf(`<tr id="task-%d">`, task)) || !strings.Contains(body, fmt.Sprintf("· %d changes", 3*changesPageSize)) {
		t.Error("task row missing or not complete")
	}
	if strings.Contains(body, "Older ›") {
		t.Error("offers an older page with nothing on it")
	}
}
