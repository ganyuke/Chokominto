package web

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestItemEditPages(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "アイドル", "THE BOOK 3"},
		[3]string{"結束バンド", "青春コンプレックス", "結束バンド"},
		[3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド"},
		[3]string{"Kessoku Band", "Seishun Complex", ""},
	)
	id := func(q string, args ...any) int64 {
		var n int64
		if err := e.db.Reader().QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	song := id(`SELECT id FROM songs WHERE name = 'アイドル'`)
	band := id(`SELECT id FROM artists WHERE name = '結束バンド'`)
	bandEN := id(`SELECT id FROM artists WHERE name = 'Kessoku Band'`)
	album := id(`SELECT id FROM releases WHERE name = 'THE BOOK 3'`)

	// Visitors see no Edit tab, can't open the Edit view and can't post.
	if _, body, _ := e.get(fmt.Sprintf("/song/%d", song)); strings.Contains(body, `/edit"`) || strings.Contains(body, "Rename") {
		t.Fatal("edit shown to a visitor")
	}
	if code, _, _ := e.get(fmt.Sprintf("/song/%d/edit", song)); code != http.StatusSeeOther {
		t.Fatalf("visitor opened the Edit view: %d", code)
	}
	if code, _, _ := e.post(fmt.Sprintf("/song/%d/edit", song), url.Values{"do": {"rename"}, "name": {"x"}}); code != http.StatusSeeOther {
		t.Fatal("visitor edit not sent to login")
	}
	if n := id(`SELECT count(*) FROM songs WHERE name = 'x'`); n != 0 {
		t.Fatal("visitor renamed a song")
	}

	e.login()
	edit := func(path string, form url.Values) string {
		t.Helper()
		code, _, h := e.post(path+"/edit", form)
		if code != http.StatusSeeOther {
			t.Fatalf("%s %v: %d", path, form, code)
		}
		return h.Get("Location")
	}
	songPath := fmt.Sprintf("/song/%d", song)
	loc := edit(songPath, url.Values{"do": {"rename"}, "name": {"Idol"}})
	_, body, _ := e.get(loc)
	if !strings.Contains(body, "<h1>Idol") || !strings.Contains(body, "Renamed アイドル to Idol.") || !strings.Contains(body, `name="back" value="`+songPath+`/edit"`) {
		t.Fatalf("rename not shown:\n%s", body)
	}
	loc = edit(songPath, url.Values{"do": {"rename"}, "name": {" "}})
	if _, body, _ := e.get(loc); !strings.Contains(body, "Give it a name.") {
		t.Fatal("empty name not refused")
	}

	// Artist: counts for, a loop, members.
	bandPath := fmt.Sprintf("/artist/%d", band)
	edit(bandPath, url.Values{"do": {"member"}, "artist": {"後藤ひとり"}})
	if id(`SELECT count(*) FROM group_members WHERE group_id = ?`, band) != 1 {
		t.Fatal("member not added")
	}
	loc = edit(fmt.Sprintf("/artist/%d", id(`SELECT id FROM artists WHERE name = '後藤ひとり'`)), url.Values{"do": {"member"}, "artist": {"結束バンド"}})
	if _, body, _ := e.get(loc); !strings.Contains(body, "go round in a circle") {
		t.Fatal("loop not refused")
	}
	loc = edit(bandPath, url.Values{"do": {"counts-for"}, "artist": {"nobody at all"}})
	if _, body, _ := e.get(loc); !strings.Contains(body, "no artist by that name") {
		t.Fatal("unknown artist not refused")
	}

	// The read view has the tabs, and editing is on its own page.
	_, body, _ = e.get(songPath)
	if !strings.Contains(body, `href="`+songPath+`" aria-current="page">Read</a>`) || !strings.Contains(body, `href="`+songPath+`/edit">Edit</a>`) || strings.Contains(body, `name="do"`) {
		t.Fatal("read view isn't just the page with tabs")
	}

	// Merge from the Edit view, found by the other name.
	_, body, _ = e.get(fmt.Sprintf("/artist/%d/edit?merge=kessoku", band))
	if !strings.Contains(body, fmt.Sprintf(`name="into" value="%d"`, bandEN)) {
		t.Fatal("merge search didn't find Kessoku Band")
	}
	loc = edit(bandPath, url.Values{"do": {"merge"}, "into": {fmt.Sprint(bandEN)}})
	if !strings.HasPrefix(loc, fmt.Sprintf("/artist/%d?done=", bandEN)) {
		t.Fatalf("merge went to %s", loc)
	}
	if code, _, h := e.get(bandPath); code != http.StatusFound || h.Get("Location") != fmt.Sprintf("/artist/%d", bandEN) {
		t.Fatalf("merged page: %d %s", code, h.Get("Location"))
	}

	// Album details, and a bad date.
	albumPath := fmt.Sprintf("/album/%d", album)
	edit(albumPath, url.Values{"do": {"details"}, "kind": {"album"}, "released": {"2023-10-04"}, "context": {""}})
	if id(`SELECT count(*) FROM releases WHERE released = '2023-10-04'`) != 1 {
		t.Fatal("details not saved")
	}
	loc = edit(albumPath, url.Values{"do": {"details"}, "kind": {"album"}, "released": {"4 Oct"}})
	if _, body, _ := e.get(loc); !strings.Contains(body, "That value isn") {
		t.Fatal("bad date not refused")
	}

	// Labels: create in Settings, reorder, put on from the page.
	code, _, h := e.post("/settings/labels", url.Values{"name": {"Anime"}})
	if code != http.StatusSeeOther {
		t.Fatalf("create label: %d", code)
	}
	if _, body, _ := e.get(h.Get("Location")); !strings.Contains(body, "Created label Anime.") {
		t.Fatal("no notice")
	}
	anime := id(`SELECT id FROM labels WHERE name = 'Anime'`)
	e.post(fmt.Sprintf("/settings/labels/%d/move", anime), url.Values{"by": {"-1"}})
	_, body, _ = e.get("/settings")
	if strings.Index(body, `value="Anime"`) > strings.Index(body, `value="Character"`) {
		t.Fatal("Anime not moved up")
	}
	edit(songPath, url.Values{"do": {"label"}, "label": {fmt.Sprint(anime)}})
	_, body, _ = e.get(songPath)
	if !regexp.MustCompile(`Labels</th><td>Anime`).MatchString(body) {
		t.Fatal("label not shown on the song")
	}
}
