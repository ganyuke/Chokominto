package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"chokominto/internal/auth"
	"chokominto/internal/config"
	"chokominto/internal/listenbrainz"
	"chokominto/internal/resolve"
	"chokominto/internal/store"

	_ "time/tzdata"
)

type env struct {
	t      *testing.T
	db     *store.DB
	srv    *httptest.Server
	client *http.Client
	userID int64
	token  string
}

func newEnv(t *testing.T, public bool) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hash, _ := auth.HashPassword("correct horse")
	u, _ := db.CreateUser(ctx, "elaina", hash)
	token := auth.NewSecret()
	db.CreateToken(ctx, u, "Pano Scrobbler", auth.HashSecret(token))

	cfg := config.Defaults()
	cfg.PublicPages = public
	cfg.DataDir = t.TempDir()
	s, err := New(cfg, db, listenbrainz.NewNowPlaying(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &env{t, db, srv, client, u, token}
}

func (e *env) get(path string) (int, string, http.Header) {
	e.t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// post submits a form the way a browser on this site would.
func (e *env) post(path string, form url.Values) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (e *env) login() {
	e.t.Helper()
	code, _, _ := e.post("/login", url.Values{"name": {"elaina"}, "password": {"correct horse"}})
	if code != http.StatusSeeOther {
		e.t.Fatalf("login: %d", code)
	}
}

func (e *env) addListens(n int) {
	var ls []store.NewListen
	for i := range n {
		ls = append(ls, store.NewListen{ListenedAt: int64(1_759_190_400 + i*60), Artist: "結束バンド", Title: fmt.Sprintf("Song %03d", i), Payload: []byte(`{}`)})
	}
	e.db.InsertListens(context.Background(), e.userID, "listenbrainz", nil, ls)
}

func TestHistoryPublic(t *testing.T) {
	e := newEnv(t, true)
	code, body, h := e.get("/")
	if code != 200 || !strings.Contains(body, "No listens yet.") {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("no CSP on pages")
	}
	// Logged-out visitors aren't pointed at settings.
	if strings.Contains(body, "Set up a scrobbler") {
		t.Fatal("setup link shown to a visitor")
	}

	e.addListens(150)
	code, body, _ = e.get("/history")
	if code != 200 || !strings.Contains(body, "Song 149") || strings.Contains(body, "Song 049") {
		t.Fatalf("first page wrong: %d", code)
	}
	if !strings.Contains(body, "Tuesday, 30 September 2025") {
		t.Fatal("no day heading")
	}
	older := regexp.MustCompile(`href="(/history\?before=[0-9.]+)"`).FindStringSubmatch(body)
	if older == nil || strings.Contains(body, "Newer") {
		t.Fatal("pager wrong on first page")
	}
	_, body, _ = e.get(older[1])
	if !strings.Contains(body, "Song 049") || strings.Contains(body, "Song 050") || strings.Contains(body, "Older") {
		t.Fatal("second page wrong")
	}
	newer := regexp.MustCompile(`href="(/history\?after=[0-9.]+)"`).FindStringSubmatch(body)
	_, body, _ = e.get(newer[1])
	if !strings.Contains(body, "Song 050") || !strings.Contains(body, "Song 149") {
		t.Fatal("newer page wrong")
	}
}

func TestHistoryEscapesText(t *testing.T) {
	e := newEnv(t, true)
	e.db.InsertListens(context.Background(), e.userID, "listenbrainz", nil, []store.NewListen{
		{ListenedAt: 1_759_190_400, Artist: `<script>alert(1)</script>`, Title: "", Payload: []byte(`{}`), Incomplete: true},
	})
	_, body, _ := e.get("/history")
	if strings.Contains(body, "<script>alert") {
		t.Fatal("artist not escaped")
	}
	if !strings.Contains(body, `class="unlinked"`) || !strings.Contains(body, "—") {
		t.Fatal("incomplete listen not marked")
	}
}

func TestPrivatePages(t *testing.T) {
	e := newEnv(t, false)
	code, _, h := e.get("/history")
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Fatalf("private history: %d %s", code, h.Get("Location"))
	}
	e.login()
	if code, _, _ := e.get("/history"); code != 200 {
		t.Fatalf("after login: %d", code)
	}
}

func TestLoginFlow(t *testing.T) {
	e := newEnv(t, true)
	if code, _, _ := e.get("/settings"); code != http.StatusSeeOther {
		t.Fatalf("settings without login: %d", code)
	}
	code, body, _ := e.post("/login", url.Values{"name": {"elaina"}, "password": {"wrong horse"}})
	if code != 401 || !strings.Contains(body, "That name or password is wrong.") {
		t.Fatalf("wrong password: %d", code)
	}
	code, _, _ = e.post("/login", url.Values{"name": {"nobody"}, "password": {"x"}})
	if code != 401 {
		t.Fatalf("unknown name: %d", code)
	}
	// An outside next is ignored.
	code, _, h := e.post("/login", url.Values{"name": {"elaina"}, "password": {"correct horse"}, "next": {"//evil.example/"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("login: %d %s", code, h.Get("Location"))
	}
	if code, body, _ := e.get("/settings"); code != 200 || !strings.Contains(body, "Pano Scrobbler") {
		t.Fatalf("settings: %d", code)
	}
	e.post("/logout", nil)
	if code, _, _ := e.get("/settings"); code != http.StatusSeeOther {
		t.Fatal("still logged in after logout")
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, true)
	for range 5 {
		e.post("/login", url.Values{"name": {"elaina"}, "password": {"nope"}})
	}
	code, body, _ := e.post("/login", url.Values{"name": {"elaina"}, "password": {"correct horse"}})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "Too many tries") {
		t.Fatalf("not limited: %d", code)
	}
}

func TestCrossOriginFormRejected(t *testing.T) {
	e := newEnv(t, true)
	req, _ := http.NewRequest("POST", e.srv.URL+"/login", strings.NewReader("name=elaina&password=correct+horse"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site form: %d", resp.StatusCode)
	}
}

func TestAPINotBlockedByExtensionOrigin(t *testing.T) {
	e := newEnv(t, true)
	body := `{"listen_type":"single","payload":[{"listened_at":1759230000,"track_metadata":{"artist_name":"A","track_name":"B"}}]}`
	req, _ := http.NewRequest("POST", e.srv.URL+"/1/submit-listens", strings.NewReader(body))
	req.Header.Set("Authorization", "Token "+e.token)
	req.Header.Set("Origin", "chrome-extension://hhinaapppaileiechjoiifaancjggfjm")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("extension submit: %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Fatal("page headers on API")
	}
}

func TestTokensInSettings(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	code, body, _ := e.post("/settings/tokens", url.Values{"label": {"Web Scrobbler"}})
	if code != 200 || !strings.Contains(body, "It won't be shown again.") {
		t.Fatalf("create: %d", code)
	}
	secret := regexp.MustCompile(`<code class="secret">([^<]+)</code>`).FindStringSubmatch(body)[1]
	if _, uid, err := e.db.TokenUser(context.Background(), auth.HashSecret(secret)); err != nil || uid != e.userID {
		t.Fatal("new token doesn't work")
	}
	id := regexp.MustCompile(`/settings/tokens/(\d+)/revoke`).FindStringSubmatch(body)[1]
	code, _, _ = e.post("/settings/tokens/"+id+"/revoke", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", code)
	}
	code, body, _ = e.post("/settings/tokens", url.Values{"label": {"  "}})
	if code != 400 || !strings.Contains(body, "Name the token") {
		t.Fatalf("empty label: %d", code)
	}
}

func TestTimeZone(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.addListens(1) // 2025-09-30 00:00 UTC
	if code, _, _ := e.post("/settings/time-zone", url.Values{"time_zone": {"Mars/Olympus"}}); code != 400 {
		t.Fatalf("bad zone: %d", code)
	}
	if code, _, _ := e.post("/settings/time-zone", url.Values{"time_zone": {"America/Los_Angeles"}}); code != http.StatusSeeOther {
		t.Fatalf("good zone: %d", code)
	}
	_, body, _ := e.get("/history")
	if !strings.Contains(body, "Monday, 29 September 2025") || !strings.Contains(body, "17:00") {
		t.Fatal("history not in the chosen time zone")
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	code, body, _ := e.post("/settings/password", url.Values{"current": {"wrong"}, "new": {"new password"}, "again": {"new password"}})
	if code != 400 || !strings.Contains(body, "current password is wrong") {
		t.Fatalf("wrong current: %d", code)
	}
	code, _, _ = e.post("/settings/password", url.Values{"current": {"correct horse"}, "new": {"new password"}, "again": {"new password"}})
	if code != http.StatusSeeOther {
		t.Fatalf("change: %d", code)
	}
	// Still logged in here.
	if code, _, _ := e.get("/settings"); code != 200 {
		t.Fatalf("logged out after own password change: %d", code)
	}
}

func TestChangesUndo(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.addListens(1)
	ls, _ := e.db.Listens(context.Background(), e.userID, store.ListenRange{Limit: 1})
	id, _ := e.db.DeleteListen(context.Background(), e.userID, ls[0])

	_, body, _ := e.get("/changes")
	if !strings.Contains(body, "Deleted listen: Song 000 by 結束バンド") {
		t.Fatal("change not listed")
	}
	code, _, h := e.post(fmt.Sprintf("/changes/%d/undo", id), nil)
	if code != http.StatusSeeOther || h.Get("Location") != "/changes?notice=undone" {
		t.Fatalf("undo: %d %s", code, h.Get("Location"))
	}
	if n, _, _, _ := e.db.ListenStats(context.Background(), e.userID); n != 1 {
		t.Fatal("listen not restored")
	}
	_, body, _ = e.get("/changes?notice=undone")
	if !strings.Contains(body, "Undone.") || !strings.Contains(body, "Undo: Deleted listen") {
		t.Fatal("undo not shown")
	}
}

func TestStaticAndErrors(t *testing.T) {
	e := newEnv(t, true)
	_, body, _ := e.get("/")
	css := regexp.MustCompile(`href="(/static/style\.css\?v=[0-9a-f]+)"`).FindStringSubmatch(body)
	if css == nil {
		t.Fatal("no stylesheet link")
	}
	code, _, h := e.get(css[1])
	if code != 200 || !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("css: %d %s", code, h.Get("Cache-Control"))
	}
	code, body, _ = e.get("/nope")
	if code != 404 || !strings.Contains(body, "Page not found") {
		t.Fatalf("404: %d", code)
	}
	if code, _, _ := e.get("/healthz"); code != 200 {
		t.Fatal("healthz")
	}
}

// No semicolons in anything a person reads, per the style spec.
func TestNoSemicolonsInCopy(t *testing.T) {
	entries, _ := templateFS.ReadDir("templates")
	tag := regexp.MustCompile(`(?s)<[^>]*>|\{\{.*?\}\}`)
	for _, en := range entries {
		b, _ := templateFS.ReadFile("templates/" + en.Name())
		text := tag.ReplaceAllString(string(b), " ")
		text = strings.ReplaceAll(text, "&#39;", "'")
		if strings.Contains(text, ";") {
			t.Errorf("%s has a semicolon in its text", en.Name())
		}
	}
	for k, v := range notices {
		if strings.Contains(v, ";") {
			t.Errorf("notice %s has a semicolon", k)
		}
	}
	for _, rd := range resolve.Readings {
		if strings.Contains(rd.Label+rd.Example, ";") {
			t.Errorf("reading %s has a semicolon", rd.Key)
		}
	}
	for k, v := range editErrors {
		if strings.Contains(v, ";") {
			t.Errorf("edit error %s has a semicolon", k)
		}
	}
}

func TestArtFiles(t *testing.T) {
	e := newEnv(t, true)
	sha := strings.Repeat("ab", 32)
	dir := filepath.Join(e.srv.Config.Handler.(*Server).art.Dir, sha[:2])
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, sha+"-64.jpg"), []byte("jpeg bytes"), 0o644)
	code, body, h := e.get("/art/" + sha + "-64.jpg")
	if code != 200 || body != "jpeg bytes" || h.Get("Content-Type") != "image/jpeg" || !strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("art: %d %q %v", code, body, h)
	}
	for _, bad := range []string{"/art/" + sha + "-440.jpg", "/art/..%2f..%2fetc%2fpasswd", "/art/" + sha + ".gif"} {
		if code, _, _ := e.get(bad); code != 404 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// Private sites don't show pictures to visitors.
	p := newEnv(t, false)
	if code, _, _ := p.get("/art/" + sha + "-64.jpg"); code != 404 {
		t.Errorf("private art: %d", code)
	}
}

func TestDisplayName(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	if code, _, h := e.post("/settings/display-name", url.Values{"display_name": {"  Elaina  the   Witch "}}); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "notice=display-name") {
		t.Fatalf("save: %d %v", code, h)
	}
	_, body, _ := e.get("/")
	if !strings.Contains(body, "Elaina the Witch’s listening") || !strings.Contains(body, `<span class="account">Elaina the Witch <form`) {
		t.Fatal("display name not shown")
	}
	// Logging in still takes the account name.
	e.post("/logout", nil)
	e.login()

	if code, body, _ := e.post("/settings/display-name", url.Values{"display_name": {strings.Repeat("あ", displayNameMax+1)}}); code != http.StatusBadRequest || !strings.Contains(body, "too long") {
		t.Fatalf("too long: %d", code)
	}
	// Empty goes back to the account name.
	e.post("/settings/display-name", url.Values{"display_name": {""}})
	if _, body, _ := e.get("/"); !strings.Contains(body, "elaina’s listening") {
		t.Fatal("empty name didn't go back to the account name")
	}
}
