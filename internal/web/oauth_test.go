package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// oauthApp plays an app that connects by signing in, like the Claude app.
type oauthApp struct {
	e        *env
	clientID string
	redirect string
}

func (e *env) registerApp(redirect string) (int, *oauthApp) {
	e.t.Helper()
	body := `{"client_name":"Claude","redirect_uris":["` + redirect + `"],"grant_types":["authorization_code","refresh_token"],"token_endpoint_auth_method":"none"}`
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, &oauthApp{e, out.ClientID, redirect}
}

const verifier = "a-verifier-that-is-long-enough-for-pkce-0123456789"

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *oauthApp) authorizeQuery(redirect string) string {
	return "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {a.clientID}, "redirect_uri": {redirect},
		"state": {"xyz"}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"resource": {a.e.srv.URL + "/mcp"},
	}.Encode()
}

// allow approves the app the way the owner does on the page, and returns
// the code the app gets back.
func (a *oauthApp) allow(access string) string {
	a.e.t.Helper()
	code, _, h := a.e.post("/oauth/authorize", url.Values{
		"client_id": {a.clientID}, "redirect_uri": {a.redirect}, "state": {"xyz"},
		"code_challenge": {challenge(verifier)}, "access": {access},
	})
	loc, _ := url.Parse(h.Get("Location"))
	if code != http.StatusSeeOther || !strings.HasPrefix(loc.String(), a.redirect) || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != a.e.srv.URL {
		a.e.t.Fatalf("allow: %d %s", code, loc)
	}
	return loc.Query().Get("code")
}

func (a *oauthApp) token(form url.Values) (int, tokenAnswer, string) {
	a.e.t.Helper()
	form.Set("client_id", a.clientID)
	req, _ := http.NewRequest("POST", a.e.srv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Apps that run in a browser call from their own site.
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var t tokenAnswer
	json.Unmarshal(b, &t)
	return resp.StatusCode, t, string(b)
}

func (a *oauthApp) exchange(code, v string) (int, tokenAnswer, string) {
	return a.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.redirect}, "code_verifier": {v}})
}

func (a *oauthApp) refresh(rt string) (int, tokenAnswer, string) {
	return a.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
}

const toolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func TestAgentsSignIn(t *testing.T) {
	e := newEnv(t, true)

	// /mcp tells an agent without a token where to start signing in.
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(toolsList))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if want := `resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource/mcp"`; resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), want) {
		t.Fatalf("401: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var pr, as map[string]any
	for path, into := range map[string]*map[string]any{"/.well-known/oauth-protected-resource/mcp": &pr, "/.well-known/oauth-authorization-server": &as} {
		code, body, h := e.get(path)
		if code != http.StatusOK || h.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d", path, code)
		}
		json.Unmarshal([]byte(body), into)
	}
	if pr["resource"] != e.srv.URL+"/mcp" || pr["authorization_servers"].([]any)[0] != e.srv.URL || as["issuer"] != e.srv.URL || as["registration_endpoint"] != e.srv.URL+"/oauth/register" {
		t.Fatalf("metadata: %v %v", pr, as)
	}

	// Registering: http only back to the same computer.
	for _, bad := range []string{"http://evil.example/cb", "javascript:alert(1)", "https://claude.ai/cb#x"} {
		if code, _ := e.registerApp(bad); code != http.StatusBadRequest {
			t.Fatalf("registered %s: %d", bad, code)
		}
	}
	code, app := e.registerApp("https://claude.ai/api/mcp/auth_callback")
	if code != http.StatusCreated || app.clientID == "" {
		t.Fatalf("register: %d", code)
	}

	// Logged out, the owner logs in first and comes back.
	code, _, h := e.get(app.authorizeQuery(app.redirect))
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/login?next=%2Foauth%2Fauthorize") {
		t.Fatalf("logged out: %d %s", code, h.Get("Location"))
	}
	e.login()
	code, body, h := e.get(app.authorizeQuery(app.redirect))
	if code != http.StatusOK || !strings.Contains(body, "Let Claude help with your music?") || !strings.Contains(body, "go back to claude.ai") {
		t.Fatalf("page: %d\n%s", code, body)
	}
	if !strings.HasSuffix(h.Get("Content-Security-Policy"), "form-action 'self' https://claude.ai") {
		t.Fatalf("csp: %s", h.Get("Content-Security-Policy"))
	}
	if !strings.Contains(body, `href="https://claude.ai/api/mcp/auth_callback?error=access_denied&amp;iss=`) {
		t.Fatalf("no way to say no:\n%s", body)
	}

	// Never sent to an address the app didn't register.
	code, _, h = e.get(app.authorizeQuery("https://evil.example/cb"))
	if code != http.StatusBadRequest || h.Get("Location") != "" {
		t.Fatalf("other redirect: %d %s", code, h.Get("Location"))
	}
	// Without PKCE, the app hears why.
	code, _, h = e.get(strings.Replace(app.authorizeQuery(app.redirect), "code_challenge_method=S256", "code_challenge_method=plain", 1))
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "error=invalid_request") {
		t.Fatalf("plain pkce: %d %s", code, h.Get("Location"))
	}
	// Another site can't approve for the owner.
	req, _ = http.NewRequest("POST", e.srv.URL+"/oauth/authorize", strings.NewReader(url.Values{"client_id": {app.clientID}, "redirect_uri": {app.redirect}, "code_challenge": {challenge(verifier)}, "access": {"change"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, _ := e.client.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site approval: %d", resp.StatusCode)
	}

	// A wrong verifier spends the code.
	stolen := app.allow("look")
	if code, _, body := app.exchange(stolen, strings.Repeat("x", 50)); code != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("wrong verifier: %d %s", code, body)
	}
	if code, _, _ := app.exchange(stolen, verifier); code != http.StatusBadRequest {
		t.Fatalf("spent code: %d", code)
	}

	// Allowed to look only.
	c := app.allow("look")
	code, tok, body := app.exchange(c, verifier)
	if code != http.StatusOK || tok.AccessToken == "" || tok.RefreshToken == "" || tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 {
		t.Fatalf("exchange: %d %s", code, body)
	}
	if code, _, _ := app.exchange(c, verifier); code != http.StatusBadRequest {
		t.Fatalf("code used twice: %d", code)
	}
	code, body = e.mcpPost(tok.AccessToken, toolsList)
	if code != http.StatusOK || !strings.Contains(body, `"search"`) || strings.Contains(body, `"merge"`) {
		t.Fatalf("look-only tools: %d %s", code, body)
	}
	if _, body, _ := e.get("/settings"); !strings.Contains(body, `<td class="fill">Claude</td>`) {
		t.Fatal("connection not listed in Settings")
	}

	// Refreshing replaces both tokens.
	code, next, body := app.refresh(tok.RefreshToken)
	if code != http.StatusOK || next.AccessToken == tok.AccessToken {
		t.Fatalf("refresh: %d %s", code, body)
	}
	if code, _ := e.mcpPost(tok.AccessToken, toolsList); code != http.StatusUnauthorized {
		t.Fatalf("old access token: %d", code)
	}
	if code, _, _ := app.refresh(tok.RefreshToken); code != http.StatusBadRequest {
		t.Fatalf("old refresh token: %d", code)
	}
	if code, _ := e.mcpPost(next.AccessToken, toolsList); code != http.StatusOK {
		t.Fatalf("new access token: %d", code)
	}

	// A second connection that can change, and changes are named after it.
	_, changer, _ := app.exchange(app.allow("change"), verifier)
	if _, body := e.mcpPost(changer.AccessToken, toolsList); !strings.Contains(body, `"merge"`) {
		t.Fatalf("change tools: %s", body)
	}

	// Revoking in Settings cuts it off, refresh too.
	ts, _ := e.db.Tokens(t.Context(), e.userID)
	for _, tk := range ts {
		if tk.Label == "Claude" && tk.Access == "look" {
			e.post(fmt.Sprintf("/settings/agents/%d/revoke", tk.ID), nil)
		}
	}
	if code, _ := e.mcpPost(next.AccessToken, toolsList); code != http.StatusUnauthorized {
		t.Fatalf("revoked access: %d", code)
	}
	if code, _, _ := app.refresh(next.RefreshToken); code != http.StatusBadRequest {
		t.Fatalf("revoked refresh: %d", code)
	}
	if code, _ := e.mcpPost(changer.AccessToken, toolsList); code != http.StatusOK {
		t.Fatalf("other connection: %d", code)
	}
}

func TestSignInFromThisComputer(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	// Desktop apps listen on a port of their choosing each time.
	_, app := e.registerApp("http://127.0.0.1:33418/callback")
	code, _, h := e.get(app.authorizeQuery("http://127.0.0.1:50123/callback"))
	if code != http.StatusOK || !strings.HasSuffix(h.Get("Content-Security-Policy"), "form-action 'self' http://127.0.0.1:50123") {
		t.Fatalf("other port: %d %s", code, h.Get("Content-Security-Policy"))
	}
	if code, _, _ := e.get(app.authorizeQuery("http://127.0.0.1:50123/elsewhere")); code != http.StatusBadRequest {
		t.Fatalf("other path: %d", code)
	}
	// Apps with their own scheme.
	_, app = e.registerApp("cursor://anysphere.cursor-mcp/oauth/callback")
	code, body, h := e.get(app.authorizeQuery(app.redirect))
	if code != http.StatusOK || !strings.HasSuffix(h.Get("Content-Security-Policy"), "form-action 'self' cursor://anysphere.cursor-mcp") || !strings.Contains(body, `href="cursor://anysphere.cursor-mcp/oauth/callback?error=access_denied`) {
		t.Fatalf("own scheme: %d %s\n%s", code, h.Get("Content-Security-Policy"), body)
	}
}
