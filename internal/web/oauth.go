package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"chokominto/internal/auth"
	"chokominto/internal/store"
)

// Agents that connect by signing in, like the Claude app's connectors,
// which can't take a token pasted in. This is the OAuth that MCP asks for
// (2025-06-18 and later): the protected resource and authorization server
// metadata, dynamic client registration, the authorization code flow with
// PKCE (S256 only), refresh tokens, and public clients only.
//
// The owner approves an app on a page here, choosing look or change. Each
// approval becomes an agent token like the ones made in Settings, so it's
// listed, revocable and named in Changes the same way. Its access token
// lasts an hour and the app refreshes it, rotating the refresh token.

const (
	accessLifetime = time.Hour
	codeLifetime   = 5 * time.Minute
	maxClientName  = 100
	maxRedirects   = 10
)

// isOAuthAPI is the part apps call directly, from their servers or from a
// browser page elsewhere. No cookies are read there, so it skips the
// website's protections and allows any origin.
func isOAuthAPI(path string) bool {
	return strings.HasPrefix(path, "/.well-known/oauth-") || path == "/oauth/register" || path == "/oauth/token"
}

func (s *Server) registerOAuth(mux *http.ServeMux) {
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		mux.HandleFunc(p, s.oauthAPI(http.MethodGet, s.protectedResource))
	}
	// Some apps add the resource's path, an older reading of the spec.
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp"} {
		mux.HandleFunc(p, s.oauthAPI(http.MethodGet, s.authServerMetadata))
	}
	mux.HandleFunc("/oauth/register", s.oauthAPI(http.MethodPost, s.registerClient))
	mux.HandleFunc("/oauth/token", s.oauthAPI(http.MethodPost, s.issueToken))
	mux.HandleFunc("GET /oauth/authorize", s.member(s.authorizePage))
	mux.HandleFunc("POST /oauth/authorize", s.member(s.authorize))
}

// oauthAPI answers CORS preflights and other methods for the endpoints
// apps call directly.
func (s *Server) oauthAPI(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			hd.Set("Access-Control-Allow-Methods", method)
			hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
			hd.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != method && !(method == http.MethodGet && r.Method == http.MethodHead) {
			hd.Set("Allow", method)
			oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "Use "+method+".")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// resourceMetadataURL is where /mcp's 401 points apps to start signing in.
func (s *Server) resourceMetadataURL(r *http.Request) string {
	return s.baseURL(r) + "/.well-known/oauth-protected-resource/mcp"
}

func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Chokominto",
	})
}

func (s *Server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

var schemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// okRedirect accepts https addresses, http only back to the same computer
// (apps on the desktop listen there), and apps' own schemes like
// cursor://. Schemes a browser would run or read locally are refused.
func okRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Opaque != "" || !schemeRE.MatchString(u.Scheme) {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopback(u.Hostname())
	case "javascript", "data", "vbscript", "file", "blob", "about", "filesystem", "ws", "wss", "ftp":
		return false
	}
	return true
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectMatches compares a requested redirect with a registered one.
// They must be the same, except that apps on this computer may pick any
// port each time (RFC 8252).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, err1 := url.Parse(registered)
	b, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil || a.Scheme != "http" || b.Scheme != "http" || !isLoopback(a.Hostname()) {
		return false
	}
	return a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery && a.User == nil && b.User == nil
}

func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Send the app's details as JSON.")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirects {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Send between 1 and 10 redirect_uris.")
		return
	}
	for _, u := range req.RedirectURIs {
		if !okRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Redirect addresses must be https, http on this computer, or an app's own scheme.")
			return
		}
	}
	if len(req.GrantTypes) > 0 && !slices.Contains(req.GrantTypes, "authorization_code") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Only the authorization_code grant is offered.")
		return
	}
	name := strings.Join(strings.Fields(req.ClientName), " ")
	if name == "" {
		name = "An agent"
		if u, err := url.Parse(req.RedirectURIs[0]); err == nil && u.Hostname() != "" && !isLoopback(u.Hostname()) {
			name = u.Hostname()
		}
	}
	for utf8.RuneCountInString(name) > maxClientName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	clientID := auth.NewSecret()
	err := s.db.CreateOAuthClient(r.Context(), clientID, name, req.RedirectURIs)
	if errors.Is(err, store.ErrTooManyClients) {
		oauthError(w, http.StatusTooManyRequests, "invalid_client_metadata", "Too many apps are waiting to connect. Try again tomorrow.")
		return
	}
	if err != nil {
		s.log.Error("registering an app", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "Try again in a moment.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// authRequest is an app's request to connect, as it arrives at the
// authorize page and again in the form the owner sends back.
type authRequest struct {
	Client      store.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
}

// readAuthRequest checks a request to connect. A bad app or redirect gets
// a page here, since sending the owner to an address that isn't the app's
// would be unsafe. Other problems go back to the app.
func (s *Server) readAuthRequest(w http.ResponseWriter, r *http.Request, v url.Values) (authRequest, bool) {
	var a authRequest
	c, err := s.db.OAuthClient(r.Context(), v.Get("client_id"))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return a, false
	}
	a.Client, a.RedirectURI, a.State = c, v.Get("redirect_uri"), v.Get("state")
	if a.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		a.RedirectURI = c.RedirectURIs[0]
	}
	if err != nil || !slices.ContainsFunc(c.RedirectURIs, func(reg string) bool { return redirectMatches(reg, a.RedirectURI) }) {
		s.render(w, http.StatusBadRequest, "error", errorPage{Page{Title: "Can't connect", User: s.sessionUser(r)},
			"This link to connect an agent doesn't work anymore. Start connecting again from the agent."})
		return a, false
	}
	if v.Get("response_type") != "code" {
		s.backToApp(w, r, a, url.Values{"error": {"unsupported_response_type"}})
		return a, false
	}
	a.Challenge = v.Get("code_challenge")
	if a.Challenge == "" || v.Get("code_challenge_method") != "S256" {
		s.backToApp(w, r, a, url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required."}})
		return a, false
	}
	if res := strings.TrimRight(v.Get("resource"), "/"); res != "" && res != s.baseURL(r)+"/mcp" && res != s.baseURL(r) {
		s.backToApp(w, r, a, url.Values{"error": {"invalid_target"}})
		return a, false
	}
	return a, true
}

// appURL is the app's redirect address with the answer added.
func (s *Server) appURL(r *http.Request, a authRequest, answer url.Values) string {
	if a.State != "" {
		answer.Set("state", a.State)
	}
	answer.Set("iss", s.baseURL(r))
	u, _ := url.Parse(a.RedirectURI)
	q := u.Query()
	for k, vs := range answer {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) backToApp(w http.ResponseWriter, r *http.Request, a authRequest, answer url.Values) {
	http.Redirect(w, r, s.appURL(r, a, answer), http.StatusSeeOther)
}

type connectPage struct {
	Page
	App, AppHost string
	Request      authRequest
	// CancelURL may be an app's own scheme, checked by okRedirect when
	// the app registered, which html/template would otherwise blank out.
	CancelURL template.URL
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	a, ok := s.readAuthRequest(w, r, r.URL.Query())
	if !ok {
		return
	}
	ru, _ := url.Parse(a.RedirectURI)
	host := ru.Host
	if ru.Host == "" || isLoopback(ru.Hostname()) {
		host = "the app on your computer"
	}
	// After Allow the browser goes on to the app, which the page's
	// form-action must permit or browsers stop it there.
	target := ru.Scheme + ":"
	if ru.Host != "" {
		target = ru.Scheme + "://" + ru.Host
	}
	w.Header().Set("Content-Security-Policy", strings.Replace(sitePolicy, "form-action 'self'", "form-action 'self' "+target, 1))
	s.render(w, http.StatusOK, "connect", connectPage{
		Page:      Page{Title: "Connect an agent", User: u},
		App:       a.Client.Name,
		AppHost:   host,
		Request:   a,
		CancelURL: template.URL(s.appURL(r, a, url.Values{"error": {"access_denied"}})),
	})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	v := r.PostForm
	v.Set("response_type", "code")
	v.Set("code_challenge_method", "S256")
	a, ok := s.readAuthRequest(w, r, v)
	if !ok {
		return
	}
	access := "look"
	if v.Get("access") == "change" {
		access = "change"
	}
	code := auth.NewSecret()
	err := s.db.CreateOAuthCode(r.Context(), auth.HashSecret(code), store.OAuthCode{
		ClientID: a.Client.ID, UserID: u.ID, RedirectURI: a.RedirectURI, Challenge: a.Challenge,
		Access: access, ExpiresAt: time.Now().Add(codeLifetime).Unix(),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("agent allowed", "app", a.Client.Name, "access", access)
	s.backToApp(w, r, a, url.Values{"code": {code}})
}

type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Send a form.")
		return
	}
	v := r.PostForm
	clientID := v.Get("client_id")
	if id, _, ok := r.BasicAuth(); ok && clientID == "" {
		clientID, _ = url.QueryUnescape(id)
	}
	c, err := s.db.OAuthClient(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "Unknown app. Connect it again.")
		return
	}
	if err != nil {
		s.tokenServerError(w, err)
		return
	}
	newAccess, newRefresh := auth.NewSecret(), auth.NewSecret()
	expires := time.Now().Add(accessLifetime).Unix()
	switch v.Get("grant_type") {
	case "authorization_code":
		code, err := s.db.TakeOAuthCode(r.Context(), auth.HashSecret(v.Get("code")))
		if errors.Is(err, store.ErrNotFound) || err == nil && (code.ClientID != c.ID || code.RedirectURI != v.Get("redirect_uri") || !pkceMatches(v.Get("code_verifier"), code.Challenge)) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "That code doesn't work. Connect again.")
			return
		}
		if err != nil {
			s.tokenServerError(w, err)
			return
		}
		if _, err := s.db.ConnectAgent(r.Context(), code, c.Name, auth.HashSecret(newAccess), auth.HashSecret(newRefresh), expires); err != nil {
			s.tokenServerError(w, err)
			return
		}
		s.log.Info("agent connected", "app", c.Name)
	case "refresh_token":
		_, err := s.db.RefreshAgent(r.Context(), c.ID, auth.HashSecret(v.Get("refresh_token")), auth.HashSecret(newAccess), auth.HashSecret(newRefresh), expires)
		if errors.Is(err, store.ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "This connection was revoked or replaced. Connect again.")
			return
		}
		if err != nil {
			s.tokenServerError(w, err)
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Use authorization_code or refresh_token.")
		return
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, tokenAnswer{AccessToken: newAccess, TokenType: "Bearer", ExpiresIn: int(accessLifetime / time.Second), RefreshToken: newRefresh})
}

func (s *Server) tokenServerError(w http.ResponseWriter, err error) {
	s.log.Error("agent sign-in", "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "Try again in a moment.")
}

// pkceMatches checks a code verifier against its S256 challenge.
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}
