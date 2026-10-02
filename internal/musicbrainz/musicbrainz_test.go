package musicbrainz

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"chokominto/internal/fetch"
)

// fake serves recorded MusicBrainz responses from testdata.
func fake(t *testing.T) (*Client, *[]string) {
	var asked []string
	files := map[string]string{
		"/ws/2/artist/11111111-1111-4111-8111-111111111111": "artist.json",
		"/ws/2/artist/44444444-4444-4444-8444-444444444444": "persona.json",
		"/ws/2/artist": "search.json",
		"/ws/2/release/55555555-5555-4555-8555-555555555555":       "release.json",
		"/ws/2/release-group/66666666-6666-4666-8666-666666666666": "release-group.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		f, ok := files[r.URL.Path]
		if !ok || r.URL.Query().Get("fmt") != "json" {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &Client{Fetch: fetch.NewLocal("Chokominto/test", u.Hostname()), Base: srv.URL + "/ws/2"}, &asked
}

func TestLookupArtist(t *testing.T) {
	c, asked := fake(t)
	e, err := c.Lookup(context.Background(), "artist", "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.Names); got != "[{結束バンド } {Kessoku Band en} {Kessoku Bando romaji}]" {
		t.Errorf("names %s", got)
	}
	if len(e.Members) != 1 || e.Members[0].Name != "青山吉能" {
		t.Errorf("members %+v", e.Members)
	}
	if !strings.Contains((*asked)[0], "inc=aliases%2Bartist-rels") {
		t.Errorf("asked %v", *asked)
	}
	p, err := c.Lookup(context.Background(), "artist", "44444444-4444-4444-8444-444444444444")
	if err != nil || len(p.PersonOf) != 1 {
		t.Fatalf("persona %+v %v", p, err)
	}
}

func TestLookupRelease(t *testing.T) {
	c, _ := fake(t)
	e, err := c.Lookup(context.Background(), "release", "55555555-5555-4555-8555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if e.Released != "2022-12-28" || fmt.Sprint(e.Names) != "[{結束バンド } {Kessoku Band en}]" {
		t.Errorf("%+v", e)
	}
}

func TestSearch(t *testing.T) {
	c, asked := fake(t)
	ms, err := c.Search(context.Background(), "artist", `結束"バンド`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Disambiguation != "from Bocchi the Rock!" || ms[0].Details != "Group, JP" {
		t.Errorf("%+v", ms)
	}
	q, _ := url.ParseQuery(strings.SplitN((*asked)[0], "?", 2)[1])
	if q.Get("query") != `artist:"結束\"バンド"` {
		t.Errorf("query %q", q.Get("query"))
	}
	if _, err := c.Lookup(context.Background(), "artist", "../../etc"); err == nil {
		t.Error("looked up a path that isn't an MBID")
	}
}

func TestLang(t *testing.T) {
	for in, want := range map[string]string{"ja": "original", "ja-Latn": "romaji", "ja_Latn": "romaji", "en": "en", "en-GB": "en", "zh": "skip"} {
		if got := Lang(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
