package listenbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chokominto/internal/auth"
	"chokominto/internal/store"
)

// Request bodies below follow the two target clients' serializers:
// Pano Scrobbler's ListenBrainzDataClasses.kt (kotlinx, explicitNulls off,
// so null fields are left out) and Web Scrobbler's
// listenbrainz-scrobbler.ts makeTrackMetadata.

type env struct {
	t      *testing.T
	db     *store.DB
	srv    *httptest.Server
	api    *API
	token  string
	userID int64
}

func newEnv(t *testing.T, public bool) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(ctx, "elaina", "h")
	token := auth.NewSecret()
	db.CreateToken(ctx, u, "Pano Scrobbler", auth.HashSecret(token))
	api := &API{DB: db, NowPlaying: NewNowPlaying(), PublicReads: public, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t, db, srv, api, token, u}
}

func (e *env) do(method, path, auth, body string) (int, map[string]any, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: response is not JSON: %q", method, path, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

func panoSingle(ts int64) string {
	return fmt.Sprintf(`{"listen_type":"single","payload":[{"listened_at":%d,"track_metadata":{"artist_name":"YOASOBI","release_name":"アイドル","track_name":"アイドル","additional_info":{"duration_ms":213000,"submission_client":"Pano Scrobbler","submission_client_version":"4.9"}}}]}`, ts)
}

func webScrobblerImport(n int, start int64) string {
	var ls []string
	for i := range n {
		artist := "結束バンド"
		if i == 0 {
			artist = "" // Web Scrobbler sends song.getArtist() ?? ''
		}
		ls = append(ls, fmt.Sprintf(`{"listened_at":%d,"track_metadata":{"artist_name":%q,"track_name":"ギターと孤独と蒼い惑星 %d","additional_info":{"submission_client":"Web Scrobbler","submission_client_version":"3.14.0","music_service_name":"YouTube","origin_url":"https://www.youtube.com/watch?v=abc%d","duration":229}}}`, start+int64(i), artist, i, i))
	}
	return `{"listen_type":"import","payload":[` + strings.Join(ls, ",") + `]}`
}

func TestValidateToken(t *testing.T) {
	e := newEnv(t, true)
	// Pano Scrobbler: lowercase scheme.
	code, out, _ := e.do("GET", "/1/validate-token", "token "+e.token, "")
	if code != 200 || out["valid"] != true || out["user_name"] != "elaina" {
		t.Fatalf("%d %v", code, out)
	}
	code, out, _ = e.do("GET", "/1/validate-token?token="+e.token, "", "")
	if code != 200 || out["valid"] != true {
		t.Fatalf("query token: %d %v", code, out)
	}
	code, out, _ = e.do("GET", "/1/validate-token", "Token wrong", "")
	if code != 200 || out["valid"] != false || out["user_name"] != nil {
		t.Fatalf("bad token: %d %v", code, out)
	}
}

func TestSubmitFromBothClients(t *testing.T) {
	e := newEnv(t, true)
	code, out, h := e.do("POST", "/1/submit-listens", "token "+e.token, panoSingle(1_759_230_000))
	if code != 200 || out["status"] != "ok" {
		t.Fatalf("pano: %d %v", code, out)
	}
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("no CORS header")
	}
	// Web Scrobbler posts to the full URL it was given, here Maloja's path.
	code, out, _ = e.do("POST", "/apis/listenbrainz/1/submit-listens", "Token "+e.token, webScrobblerImport(50, 1_759_231_000))
	if code != 200 || out["status"] != "ok" {
		t.Fatalf("web scrobbler: %d %v", code, out)
	}
	n, _, _, _ := e.db.ListenStats(context.Background(), e.userID)
	if n != 51 {
		t.Fatalf("stored %d listens, want 51", n)
	}
	var incomplete int
	e.db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE incomplete = 1`).Scan(&incomplete)
	if incomplete != 1 {
		t.Fatalf("incomplete = %d, want 1 (the empty artist)", incomplete)
	}
	// The payload is kept exactly, including fields Chokominto doesn't use yet.
	var payload string
	e.db.Reader().QueryRow(`SELECT payload FROM listen_payloads ORDER BY listen_id DESC LIMIT 1`).Scan(&payload)
	if !strings.Contains(payload, `"origin_url":"https://www.youtube.com/watch?v=abc49"`) {
		t.Fatalf("payload not kept: %s", payload)
	}
	// Retrying (Web Scrobbler resends its queue on failure) stores nothing new.
	e.do("POST", "/1/submit-listens", "Token "+e.token, webScrobblerImport(50, 1_759_231_000))
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 51 {
		t.Fatalf("retry changed count to %d", n)
	}
}

func TestSubmitErrors(t *testing.T) {
	e := newEnv(t, true)
	auth := "Token " + e.token
	cases := []struct {
		name, auth, body string
		code             int
	}{
		{"no auth", "", panoSingle(1), 401},
		{"bad token", "Token nope", panoSingle(1), 401},
		{"not json", auth, `{"listen_type":`, 400},
		{"array", auth, `[]`, 400},
		{"no type", auth, `{"payload":[]}`, 400},
		{"bad type", auth, `{"listen_type":"loved","payload":[{}]}`, 400},
		{"two singles", auth, `{"listen_type":"single","payload":[{},{}]}`, 400},
		{"empty import", auth, `{"listen_type":"import","payload":[]}`, 400},
		{"artist is a number", auth, `{"listen_type":"single","payload":[{"listened_at":1,"track_metadata":{"artist_name":5,"track_name":"x"}}]}`, 400},
		{"time is text", auth, `{"listen_type":"single","payload":[{"listened_at":"soon","track_metadata":{}}]}`, 400},
	}
	for _, c := range cases {
		code, out, _ := e.do("POST", "/1/submit-listens", c.auth, c.body)
		if code != c.code {
			t.Errorf("%s: got %d, want %d (%v)", c.name, code, c.code, out)
			continue
		}
		if out["code"] != float64(c.code) || out["error"] == "" || out["error"] == nil {
			t.Errorf("%s: not a ListenBrainz error: %v", c.name, out)
		}
	}
	code, _, _ := e.do("POST", "/1/submit-listens", auth, webScrobblerImport(MaxImport+1, 1_759_231_000))
	if code != 400 {
		t.Errorf("too many listens: %d", code)
	}
	code, _, _ = e.do("POST", "/1/submit-listens", auth, `{"listen_type":"import","payload":["`+strings.Repeat("x", MaxBodyBytes)+`"]}`)
	if code != 413 {
		t.Errorf("huge body: %d", code)
	}
	code, out, _ := e.do("GET", "/1/no-such-thing", "", "")
	if code != 404 || out["code"] != float64(404) {
		t.Errorf("unknown method: %d %v", code, out)
	}
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 0 {
		t.Fatalf("rejected requests stored %d listens", n)
	}
}

func TestSubmitLenientContent(t *testing.T) {
	e := newEnv(t, true)
	// Numeric string time, missing track_metadata, float time: all stored.
	body := `{"listen_type":"import","payload":[
		{"listened_at":"1759230000","track_metadata":{"artist_name":"A","track_name":"B"}},
		{"listened_at":1759230001},
		{"listened_at":1759230002.0,"track_metadata":{"artist_name":"C","track_name":"D","release_name":null}}]}`
	code, out, _ := e.do("POST", "/1/submit-listens", "Token "+e.token, body)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 3 {
		t.Fatalf("stored %d", n)
	}
}

func TestPlayingNow(t *testing.T) {
	e := newEnv(t, true)
	base := time.Unix(1_800_000_000, 0)
	e.api.NowPlaying.now = func() time.Time { return base }
	e.do("POST", "/1/submit-listens", "token "+e.token, panoSingle(1_759_230_000))

	body := `{"listen_type":"playing_now","payload":[{"track_metadata":{"artist_name":"YOASOBI","release_name":"アイドル","track_name":"アイドル","additional_info":{"duration_ms":213000,"submission_client":"Pano Scrobbler"}}}]}`
	code, out, _ := e.do("POST", "/1/submit-listens?return_msid=true", "token "+e.token, body)
	if code != 200 || out["status"] != "ok" || out["recording_msid"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	code, out, _ = e.do("GET", "/1/user/elaina/playing-now", "", "")
	p := out["payload"].(map[string]any)
	ls := p["listens"].([]any)
	if code != 200 || p["count"] != float64(1) || len(ls) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	l := ls[0].(map[string]any)
	if l["playing_now"] != true || l["track_metadata"].(map[string]any)["track_name"] != "アイドル" {
		t.Fatalf("%v", l)
	}
	// Playing now isn't a listen.
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 1 {
		t.Fatalf("playing_now stored a listen")
	}
	// It expires after the track's length.
	e.api.NowPlaying.now = func() time.Time { return base.Add(214 * time.Second) }
	_, out, _ = e.do("GET", "/1/user/elaina/playing-now", "", "")
	if out["payload"].(map[string]any)["count"] != float64(0) {
		t.Fatalf("still playing after the track ended: %v", out)
	}
}

func listenTimes(t *testing.T, out map[string]any) []int64 {
	t.Helper()
	var ts []int64
	for _, l := range out["payload"].(map[string]any)["listens"].([]any) {
		ts = append(ts, int64(l.(map[string]any)["listened_at"].(float64)))
	}
	return ts
}

func TestReadListens(t *testing.T) {
	e := newEnv(t, true)
	e.do("POST", "/1/submit-listens", "Token "+e.token, webScrobblerImport(10, 1000_000_000+100))

	code, out, _ := e.do("GET", "/1/user/elaina/listens?count=3", "", "")
	p := out["payload"].(map[string]any)
	if code != 200 || p["count"] != float64(3) || p["latest_listen_ts"] != float64(1000000109) || p["oldest_listen_ts"] != float64(1000000100) {
		t.Fatalf("%d %v", code, p)
	}
	first := p["listens"].([]any)[0].(map[string]any)
	if first["recording_msid"] == "" || first["inserted_at"] == nil || first["user_name"] != "elaina" {
		t.Fatalf("listen shape: %v", first)
	}
	if got := fmt.Sprint(listenTimes(t, out)); got != "[1000000109 1000000108 1000000107]" {
		t.Fatalf("newest first: %s", got)
	}
	// Pano pages backwards with max_ts = the oldest time it has.
	_, out, _ = e.do("GET", "/1/user/elaina/listens?count=3&max_ts=1000000107", "", "")
	if got := fmt.Sprint(listenTimes(t, out)); got != "[1000000106 1000000105 1000000104]" {
		t.Fatalf("max_ts: %s", got)
	}
	_, out, _ = e.do("GET", "/1/user/elaina/listens?count=2&min_ts=1000000102", "", "")
	if got := fmt.Sprint(listenTimes(t, out)); got != "[1000000104 1000000103]" {
		t.Fatalf("min_ts: %s", got)
	}
	_, out, _ = e.do("GET", "/1/user/elaina/listen-count", "", "")
	if out["payload"].(map[string]any)["count"] != float64(10) {
		t.Fatalf("count %v", out)
	}
	code, _, _ = e.do("GET", "/1/user/nobody/listens", "", "")
	if code != 404 {
		t.Fatalf("unknown user: %d", code)
	}
	code, _, _ = e.do("GET", "/1/user/elaina/listens?count=many", "", "")
	if code != 400 {
		t.Fatalf("bad count: %d", code)
	}
}

func TestPrivateReads(t *testing.T) {
	e := newEnv(t, false)
	code, _, _ := e.do("GET", "/1/user/elaina/listens", "", "")
	if code != 401 {
		t.Fatalf("private read without token: %d", code)
	}
	code, _, _ = e.do("GET", "/1/user/elaina/listens", "token "+e.token, "")
	if code != 200 {
		t.Fatalf("private read with token: %d", code)
	}
}

func TestDeleteListen(t *testing.T) {
	e := newEnv(t, true)
	e.do("POST", "/1/submit-listens", "token "+e.token, panoSingle(1_759_230_000))
	_, out, _ := e.do("GET", "/1/user/elaina/listens", "", "")
	msid := out["payload"].(map[string]any)["listens"].([]any)[0].(map[string]any)["recording_msid"].(string)

	body := fmt.Sprintf(`{"listened_at":1759230000,"recording_msid":%q}`, msid)
	code, _, _ := e.do("POST", "/1/delete-listen", "", body)
	if code != 401 {
		t.Fatalf("delete without token: %d", code)
	}
	code, out, _ = e.do("POST", "/1/delete-listen", "token "+e.token, body)
	if code != 200 || out["status"] != "ok" {
		t.Fatalf("%d %v", code, out)
	}
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 0 {
		t.Fatal("listen still there")
	}
	// Deleting again is fine.
	code, _, _ = e.do("POST", "/1/delete-listen", "token "+e.token, body)
	if code != 200 {
		t.Fatalf("repeat delete: %d", code)
	}
	// And it can be undone from the change history.
	es, _ := e.db.Edits(context.Background(), e.userID, 0, 5)
	if len(es) != 1 {
		t.Fatalf("edits %v", es)
	}
	e.db.UndoEdit(context.Background(), e.userID, es[0].ID)
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 1 {
		t.Fatal("undo didn't restore")
	}
}

func TestStubsAndAliases(t *testing.T) {
	e := newEnv(t, true)
	for _, p := range []string{"/1", "/apis/listenbrainz/1", "/apis/lbrnz/1"} {
		code, out, _ := e.do("GET", p+"/validate-token", "token "+e.token, "")
		if code != 200 || out["valid"] != true {
			t.Errorf("%s: %d", p, code)
		}
	}
	for path, want := range map[string]int{
		"/1/stats/user/elaina/artists":            204,
		"/1/stats/user/elaina/listening-activity": 200,
		"/1/feedback/user/elaina/get-feedback":    204,
		"/1/user/elaina/following":                204,
		"/1/metadata/lookup?artist_name=a":        200,
	} {
		if code, _, _ := e.do("GET", path, "", ""); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	code, _, h := e.do("OPTIONS", "/1/submit-listens", "", "")
	if code != 204 || !strings.Contains(h.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight: %d %v", code, h)
	}
}

func FuzzDecodeSubmit(f *testing.F) {
	f.Add([]byte(panoSingle(1_759_230_000)))
	f.Add([]byte(webScrobblerImport(3, 1_759_230_000)))
	f.Add([]byte(`{"listen_type":"playing_now","payload":[{"track_metadata":{"additional_info":{"duration":"12"}}}]}`))
	f.Add([]byte(`{"listen_type":"single","payload":[{"listened_at":1e30}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := DecodeSubmit(b)
		if err != nil {
			return
		}
		switch s.Type {
		case "single", "playing_now":
			if len(s.Listens) != 1 {
				t.Fatalf("%s with %d listens", s.Type, len(s.Listens))
			}
		case "import":
			if len(s.Listens) < 1 || len(s.Listens) > MaxImport {
				t.Fatalf("import with %d listens", len(s.Listens))
			}
		default:
			t.Fatalf("type %q accepted", s.Type)
		}
	})
}

func TestDecodeAlbumArtist(t *testing.T) {
	listen := func(info string) string {
		return `{"listen_type":"single","payload":[{"listened_at":1759230000,"track_metadata":{"artist_name":"ClariS","track_name":"コネクト","release_name":"まどか☆マギカ OST","additional_info":` + info + `}}]}`
	}
	for _, c := range []struct{ info, want string }{
		{`{"release_artist_name":"Various Artists"}`, "Various Artists"},
		{`{"release_artist_name":" Various Artists "}`, " Various Artists "}, // kept as sent
		{`{}`, ""},
		{`{"release_artist_name":null}`, ""},
		{`{"release_artist_name":7}`, ""}, // ignored, the listen still counts
	} {
		s, err := DecodeSubmit([]byte(listen(c.info)))
		if err != nil {
			t.Fatalf("%s: %v", c.info, err)
		}
		if got := s.Listens[0].AlbumArtist; got != c.want {
			t.Errorf("%s: album artist %q, want %q", c.info, got, c.want)
		}
	}
}

func TestAlbumArtistKeepsTextApart(t *testing.T) {
	e := newEnv(t, true)
	body := func(ts int64, albumArtist string) string {
		return fmt.Sprintf(`{"listen_type":"single","payload":[{"listened_at":%d,"track_metadata":{"artist_name":"ClariS","track_name":"コネクト","release_name":"OST","additional_info":{"release_artist_name":%q}}}]}`, ts, albumArtist)
	}
	e.do("POST", "/1/submit-listens", "Token "+e.token, body(1_759_230_000, "Various Artists"))
	e.do("POST", "/1/submit-listens", "Token "+e.token, body(1_759_230_300, "ClariS"))
	var n int
	e.db.Reader().QueryRow(`SELECT count(*) FROM sources`).Scan(&n)
	if n != 2 {
		t.Fatalf("sources = %d, want one per album artist", n)
	}
	code, out, _ := e.do("POST", "/1/submit-listens?return_msid=true", "Token "+e.token,
		strings.Replace(strings.Replace(body(0, "Various Artists"), `"single"`, `"playing_now"`, 1), `"listened_at":0,`, "", 1))
	if code != 200 || out["recording_msid"] == nil {
		t.Fatalf("playing now msid: %d %v", code, out)
	}
}
