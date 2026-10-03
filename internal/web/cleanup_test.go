package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (e *env) itemID(kind, name string) int64 {
	e.t.Helper()
	found, err := e.db.SearchItems(e.t.Context(), e.userID, kind, name, 5)
	if err != nil || len(found) == 0 {
		e.t.Fatalf("no %s %q: %v", kind, name, err)
	}
	return found[0].ID
}

// edit posts one form on an item's Edit tab and returns where it went.
func (e *env) edit(path string, form url.Values) string {
	e.t.Helper()
	code, body, h := e.post(path+"/edit", form)
	if code != http.StatusSeeOther {
		e.t.Fatalf("%s %v: %d\n%s", path, form, code, body)
	}
	if strings.Contains(h.Get("Location"), "err=") {
		e.t.Fatalf("%s %v: %s", path, form, h.Get("Location"))
	}
	return h.Get("Location")
}

func TestEditNamesCreditsAndGraveyard(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.post("/settings/names", url.Values{"on": {"1"}})
	e.scrobbleNow(
		[3]string{"Aniplex", "不可思議のカルテ 桜島麻衣 Ver.", "4:00 AM"},
		[3]string{"Some Channel", "Never Gonna Give You Up", ""},
	)
	song := e.itemID("song", "不可思議のカルテ")
	songPath := fmt.Sprintf("/song/%d", song)

	// Names: one shown on the page and in lists, one hidden.
	e.edit(songPath, url.Values{"do": {"add-name"}, "name": {"Fukashigi no Karte"}})
	e.edit(songPath, url.Values{"do": {"add-name"}, "name": {"Fukashigi No Carte.mp3"}})
	as, _ := e.db.Aliases(t.Context(), "song", song)
	form := url.Values{"do": {"names"}}
	for _, a := range as {
		id := fmt.Sprint(a.ID)
		form.Add("alias", id)
		form.Set("lang-"+id, a.Lang)
		switch a.Name {
		case "Fukashigi no Karte":
			form.Set("lang-"+id, "romaji")
			form.Set("shown-"+id, "1")
			form.Set("in-lists", id)
		case "Fukashigi No Carte.mp3":
			// left unticked
		default:
			form.Set("shown-"+id, "1")
		}
	}
	e.edit(songPath, form)
	_, body, _ := e.get(songPath)
	if strings.Contains(body, ".mp3") || !strings.Contains(body, "Fukashigi no Karte") {
		t.Fatalf("song page names:\n%s", body)
	}
	_, body, _ = e.get(songPath + "/edit")
	for _, want := range []string{"On the page", "In lists", "Every name, shown or not, is used to link new scrobbles", "Fukashigi No Carte.mp3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit tab has no %q", want)
		}
	}

	// Credit the character instead of the channel.
	rec := e.db
	recs, _ := rec.SongRecordingDetails(t.Context(), e.userID, song)
	r := fmt.Sprint(recs[0].ID)
	e.edit(songPath, url.Values{"do": {"credit"}, "recording": {r}, "artist": {"桜島麻衣"}, "role": {"main"}})
	channel := e.itemID("artist", "Aniplex")
	loc := e.edit(songPath, url.Values{"do": {"uncredit"}, "recording": {r}, "artist": {fmt.Sprint(channel)}, "role": {"main"}})
	_, body, _ = e.get(loc)
	if !strings.Contains(body, "Credited 桜島麻衣 on Fukashigi no Karte (桜島麻衣 Ver.) instead of Aniplex, 桜島麻衣") {
		t.Fatalf("credit notice:\n%s", body)
	}
	// The album that's really a time: take the song off, then delete it.
	album := e.itemID("release", "4:00 AM")
	albumPath := fmt.Sprintf("/album/%d", album)
	_, body, _ = e.get(albumPath + "/edit")
	if !strings.Contains(body, "It still has 1 song and 1 listen on it.") || strings.Contains(body, "Delete this album") {
		t.Fatalf("used album:\n%s", body)
	}
	e.edit(albumPath, url.Values{"do": {"take-off"}, "recording": {r}})
	e.edit(albumPath, url.Values{"do": {"delete"}})
	if code, _, _ := e.get(albumPath); code != http.StatusNotFound {
		t.Fatalf("album still there: %d", code)
	}

	// With the album gone, nothing credits the channel.
	artistPath := fmt.Sprintf("/artist/%d", channel)
	_, body, _ = e.get(artistPath + "/edit")
	if !strings.Contains(body, "Delete this artist") {
		t.Fatal("unused artist can't be deleted")
	}
	loc = e.edit(artistPath, url.Values{"do": {"delete"}})
	if !strings.HasPrefix(loc, "/changes?done=") {
		t.Fatal(loc)
	}
	if _, body, _ = e.get(loc); !strings.Contains(body, "Deleted the artist Aniplex") {
		t.Fatal("no notice on Changes")
	}

	// Not music: to the graveyard, then back from Review.
	video := e.itemID("song", "Never Gonna")
	videoPath := fmt.Sprintf("/song/%d", video)
	e.edit(videoPath, url.Values{"do": {"bury"}})
	if _, body, _ = e.get(videoPath); !strings.Contains(body, "This song is in the graveyard") {
		t.Fatal("no graveyard notice")
	}
	_, body, _ = e.get("/review")
	if !strings.Contains(body, `id="graveyard"`) || !strings.Contains(body, "Never Gonna Give You Up") {
		t.Fatalf("review:\n%s", body)
	}
	loc = e.edit(videoPath, url.Values{"do": {"unbury"}, "back": {"review"}})
	if !strings.HasPrefix(loc, "/review?done=") || !strings.HasSuffix(loc, "#graveyard") {
		t.Fatal(loc)
	}
	if _, body, _ = e.get("/review"); strings.Contains(body, `id="graveyard"`) {
		t.Fatal("still in the graveyard")
	}
}
