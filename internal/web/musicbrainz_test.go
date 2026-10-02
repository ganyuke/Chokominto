package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"chokominto/internal/fetch"
	"chokominto/internal/musicbrainz"
)

// fakeMusicBrainz serves the recorded responses of the musicbrainz package.
func fakeMusicBrainz(t *testing.T) (*musicbrainz.Client, *int) {
	asked := 0
	files := map[string]string{
		"/ws/2/artist/11111111-1111-4111-8111-111111111111": "artist.json",
		"/ws/2/artist": "search.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		f, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, _ := os.ReadFile("../musicbrainz/testdata/" + f)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &musicbrainz.Client{Fetch: fetch.NewLocal("Chokominto/test", u.Hostname()), Base: srv.URL + "/ws/2"}, &asked
}

func TestNamesFromMusicBrainz(t *testing.T) {
	e := newEnv(t, true)
	mb, asked := fakeMusicBrainz(t)
	e.srv.Config.Handler.(*Server).SetMusicBrainz(mb)
	e.scrobbleNow(
		[3]string{"結束バンド", "青春コンプレックス", "結束バンド"},
		[3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド"},
	)
	var band int64
	e.db.Reader().QueryRow(`SELECT id FROM artists WHERE name = '結束バンド'`).Scan(&band)
	path := fmt.Sprintf("/artist/%d", band)
	e.login()

	// Nothing is asked until the page is opened.
	e.get(path)
	if *asked != 0 {
		t.Fatal("asked MusicBrainz without the button")
	}
	_, body, _ := e.get(path + "/musicbrainz")
	if !strings.Contains(body, "from Bocchi the Rock!") || !strings.Contains(body, "?mbid=11111111-1111-4111-8111-111111111111") {
		t.Fatalf("no search results:\n%s", body)
	}
	_, body, _ = e.get(path + "/musicbrainz?mbid=11111111-1111-4111-8111-111111111111")
	if !strings.Contains(body, "Kessoku Bando") || !strings.Contains(body, "(has it already)") || !strings.Contains(body, "Member: 青山吉能") {
		t.Fatalf("no preview:\n%s", body)
	}
	if strings.Contains(body, "结束乐队") {
		t.Fatal("offered a name in a language that isn't used")
	}
	link := strings.SplitN(strings.SplitN(body, `name="link" value="`, 2)[1], `"`, 2)[0]
	code, _, h := e.post(path+"/musicbrainz", url.Values{"mbid": {"11111111-1111-4111-8111-111111111111"}, "link": {link}})
	if code != http.StatusSeeOther {
		t.Fatalf("import: %d", code)
	}
	_, body, _ = e.get(h.Get("Location"))
	if !strings.Contains(body, "Got 2 names for 結束バンド from MusicBrainz.") {
		t.Fatalf("no notice:\n%s", body)
	}
	var n int
	e.db.Reader().QueryRow(`SELECT count(*) FROM group_members WHERE group_id = ?`, band).Scan(&n)
	if n != 1 {
		t.Fatal("member not added")
	}
	var name, mbid string
	e.db.Reader().QueryRow(`SELECT name, mbid FROM artists WHERE id = ?`, band).Scan(&name, &mbid)
	if name != "Kessoku Band" || mbid != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("name %q mbid %q", name, mbid)
	}

	// Made-up MBIDs never reach MusicBrainz.
	before := *asked
	if code, _, _ := e.post(path+"/musicbrainz", url.Values{"mbid": {"../x"}}); code != http.StatusNotFound {
		t.Fatalf("bad mbid: %d", code)
	}
	if *asked != before {
		t.Fatal("asked with a made-up MBID")
	}
}
