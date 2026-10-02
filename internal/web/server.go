// Package web serves the website and mounts the ListenBrainz API.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"chokominto/internal/artwork"
	"chokominto/internal/auth"
	"chokominto/internal/config"
	"chokominto/internal/fetch"
	"chokominto/internal/listenbrainz"
	"chokominto/internal/musicbrainz"
	"chokominto/internal/store"
)

var numberPrinter = message.NewPrinter(language.English)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const maxFormBytes = 64 << 10

type Server struct {
	cfg        config.Config
	art        artwork.Store
	finder     *artwork.Finder // downloads candidates the owner chooses
	mb         *musicbrainz.Client
	db         *store.DB
	np         *listenbrainz.NowPlaying
	log        *slog.Logger
	limiter    *auth.Limiter
	pages      map[string]*template.Template
	staticHash map[string]string
	handler    http.Handler
	// otherNames is whether pages show other names, the owner's setting.
	// Read on every name, so it's kept here rather than looked up. While
	// Chokominto is single-user, visitors and the owner see the same.
	otherNames atomic.Bool
}

func New(cfg config.Config, db *store.DB, np *listenbrainz.NowPlaying, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, db: db, np: np, log: log, limiter: auth.NewLoginLimiter(), mb: musicbrainz.New(userAgent(cfg.PublicURL)), art: artwork.Store{Dir: cfg.ArtworkDir()}}
	s.finder = &artwork.Finder{DB: db, Store: s.art, Fetch: fetch.New(userAgent(cfg.PublicURL), artwork.Hosts...), Sources: artwork.Live}
	artwork.PaceLive(s.finder.Fetch)
	if err := s.loadStatic(); err != nil {
		return nil, err
	}
	if owner, err := db.FirstUser(context.Background()); err == nil {
		s.otherNames.Store(owner.ShowOtherNames)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	api := &listenbrainz.API{DB: db, NowPlaying: np, PublicReads: cfg.PublicPages, Log: log}
	api.Register(mux)

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticHandler(http.FileServerFS(sub))))
	// Browsers ask for this on every site. There's no icon, and an empty answer
	// beats rendering the not-found page.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /art/{name}", s.artFile)
	mux.HandleFunc("GET /art/candidate/{id}", s.member(s.candidateThumb))
	mux.HandleFunc("POST /artist/{id}/picture", s.member(s.uploadPicture("artist")))
	mux.HandleFunc("POST /album/{id}/picture", s.member(s.uploadPicture("release")))
	mux.HandleFunc("GET /{$}", s.viewer(s.home))
	mux.HandleFunc("GET /top/songs", s.viewer(s.top("songs")))
	mux.HandleFunc("GET /top/artists", s.viewer(s.top("artists")))
	mux.HandleFunc("GET /top/albums", s.viewer(s.top("albums")))
	mux.HandleFunc("GET /artist/{id}", s.viewer(s.artistPage))
	mux.HandleFunc("GET /song/{id}", s.viewer(s.songPage))
	mux.HandleFunc("GET /album/{id}", s.viewer(s.albumPage))
	mux.HandleFunc("GET /history", s.viewer(s.history))
	mux.HandleFunc("GET /live", s.viewer(s.live))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /settings", s.member(s.settings))
	mux.HandleFunc("POST /settings/tokens", s.member(s.createToken))
	mux.HandleFunc("POST /settings/tokens/{id}/revoke", s.member(s.revokeToken))
	mux.HandleFunc("POST /settings/agents", s.member(s.createAgentToken))
	mux.HandleFunc("POST /settings/agents/{id}/revoke", s.member(s.revokeAgentToken))
	mux.HandleFunc("/mcp", s.mcpEndpoint)
	mux.HandleFunc("POST /settings/time-zone", s.member(s.setTimeZone))
	mux.HandleFunc("POST /settings/password", s.member(s.changePassword))
	mux.HandleFunc("POST /settings/labels/{id}", s.member(s.updateLabel))
	mux.HandleFunc("POST /settings/labels/{id}/delete", s.member(s.deleteLabel))
	mux.HandleFunc("POST /settings/labels", s.member(s.createLabel))
	mux.HandleFunc("GET /settings/readings", s.member(s.previewReadings))
	mux.HandleFunc("POST /settings/readings", s.member(s.saveReadings))
	mux.HandleFunc("POST /settings/pictures", s.member(s.setFindArtwork))
	mux.HandleFunc("POST /settings/names", s.member(s.setShowOtherNames))
	mux.HandleFunc("POST /settings/display-name", s.member(s.setDisplayName))
	mux.HandleFunc("POST /settings/pictures/clean", s.member(s.cleanArtwork))
	mux.HandleFunc("POST /settings/rules/{id}", s.member(s.changeRule))
	mux.HandleFunc("POST /settings/labels/{id}/move", s.member(s.moveLabel))
	mux.HandleFunc("GET /scrobble", s.member(s.scrobblePage))
	mux.HandleFunc("POST /scrobble", s.member(s.scrobble))
	mux.HandleFunc("GET /listen/{id}/fix", s.member(s.fixPage))
	mux.HandleFunc("POST /listen/{id}/link", s.member(s.fixLink))
	mux.HandleFunc("POST /listen/{id}/new-song", s.member(s.fixNewSong))
	mux.HandleFunc("POST /listen/{id}/remember", s.member(s.fixRemember))
	mux.HandleFunc("POST /listen/{id}/delete", s.member(s.fixDelete))
	mux.HandleFunc("GET /artist/{id}/edit", s.member(s.editView("artist")))
	mux.HandleFunc("GET /song/{id}/edit", s.member(s.editView("song")))
	mux.HandleFunc("GET /album/{id}/edit", s.member(s.editView("release")))
	mux.HandleFunc("POST /artist/{id}/edit", s.member(s.itemEdit("artist")))
	mux.HandleFunc("POST /song/{id}/edit", s.member(s.itemEdit("song")))
	mux.HandleFunc("POST /album/{id}/edit", s.member(s.itemEdit("release")))
	for kind, path := range pagePaths {
		mux.HandleFunc("GET "+path+"/{id}/musicbrainz", s.member(s.musicBrainzPage(kind)))
		mux.HandleFunc("POST "+path+"/{id}/musicbrainz", s.member(s.importMusicBrainz(kind)))
	}
	mux.HandleFunc("GET /review", s.member(s.review))
	mux.HandleFunc("POST /review/suggestions", s.member(s.answerSuggestions))
	mux.HandleFunc("POST /review/link", s.member(s.reviewLink))
	mux.HandleFunc("GET /changes", s.member(s.changes))
	mux.HandleFunc("POST /changes/{id}/undo", s.member(s.undo))
	mux.HandleFunc("/", s.notFound)

	cop := http.NewCrossOriginProtection()
	if cfg.PublicURL != "" {
		u, _ := url.Parse(cfg.PublicURL)
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			return nil, err
		}
	}
	s.handler = s.recoverer(s.logRequests(s.protect(cop, mux)))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// protect applies browser protections to the website only. The API and
// /mcp are token-authenticated and never read cookies, and Web Scrobbler
// calls the API from an extension origin, so cross-origin checks would
// only break it. /mcp checks Origin itself.
func (s *Server) protect(cop *http.CrossOriginProtection, next http.Handler) http.Handler {
	site := cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			limit := int64(maxFormBytes)
			if strings.HasSuffix(r.URL.Path, "/picture") {
				limit = artwork.MaxSize + 64<<10 // an uploaded picture
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if listenbrainz.IsAPIPath(r.URL.Path) || r.URL.Path == "/mcp" {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs method, path and status. Never the query string, which
// can hold a token on validate-token.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "panic", v)
				http.Error(w, "Something went wrong on the server.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Static files are served with a content hash in the URL, so they can be
// cached forever and still update on a new release.
func (s *Server) loadStatic() error {
	s.staticHash = map[string]string{}
	return fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s.staticHash[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:6])
		return nil
	})
}

func (s *Server) staticHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("v"); v != "" && v == s.staticHash[r.URL.Path] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) loadTemplates() error {
	funcs := template.FuncMap{
		"static": func(name string) (string, error) {
			h, ok := s.staticHash[name]
			if !ok {
				return "", fmt.Errorf("no static file %s", name)
			}
			return "/static/" + name + "?v=" + h, nil
		},
		"add":        func(a, b int) int { return a + b },
		"otherNames": func() bool { return s.otherNames.Load() },
		// A song's name with its recording's version, so the version stays
		// next to the name, above any other names.
		"withVersion": func(n name, version string) name { n.Version = version; return n },
		"sub":         func(a, b int) int { return a - b },
		// Counts use thousands separators: 1,234.
		"num":        func(n int) string { return numberPrinter.Sprintf("%d", n) },
		"albumKinds": func() []option { return albumKindNames },
		"pagePaths":  func() map[string]string { return pagePaths },
		"versionForm": func(path string, r store.SongRecording) map[string]any {
			return map[string]any{"Path": path, "Recording": r}
		},
		// Missing values are an em dash, per the style spec.
		"dash": func(v string) string {
			if strings.TrimSpace(v) == "" {
				return "—"
			}
			return v
		},
	}
	s.pages = map[string]*template.Template{}
	for _, page := range []string{"home", "history", "top", "artist", "song", "album", "scrobble", "fix", "review", "musicbrainz", "itemedit", "login", "settings", "readings", "changes", "error", "setup"} {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/edit.html", "templates/"+page+".html")
		if err != nil {
			return err
		}
		s.pages[page] = t
	}
	return nil
}

// Page is what every template receives.
type Page struct {
	Title  string
	Nav    string
	User   *store.User // logged in, or nil
	Notice string
	Undo   int64  // an edit the notice offers to undo
	Back   string // where Undo returns to, when not Changes
	// Sorting says rankings are still filling in after an import. Pages
	// count it down by themselves.
	Sorting string
	Error   string
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var buf strings.Builder
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "Something went wrong on the server.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	fmt.Fprint(w, buf.String())
}

type errorPage struct {
	Page
	Message string
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusNotFound, "error", errorPage{Page{Title: "Page not found", User: s.sessionUser(r)}, "There's nothing here."})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("page", "path", r.URL.Path, "err", err)
	s.render(w, http.StatusInternalServerError, "error", errorPage{Page{Title: "Something went wrong"}, "Try again in a moment."})
}

// Sessions

func (s *Server) cookieName() string {
	if s.cfg.HTTPS {
		return "__Host-chokominto"
	}
	return "chokominto"
}

type ctxKey struct{}

// sessionUser returns the logged-in user, looked up once per request.
func (s *Server) sessionUser(r *http.Request) *store.User {
	if u, ok := r.Context().Value(ctxKey{}).(*store.User); ok {
		return u
	}
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	id, err := s.db.SessionUser(r.Context(), auth.HashSecret(c.Value))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("session", "err", err)
		}
		return nil
	}
	u, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		return nil
	}
	return &u
}

func withUser(r *http.Request, u *store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	secret := auth.NewSecret()
	if err := s.db.CreateSession(r.Context(), userID, auth.HashSecret(secret)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    secret,
		Path:     "/",
		MaxAge:   int(store.SessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.HTTPS,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.db.DeleteSession(r.Context(), auth.HashSecret(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.HTTPS, SameSite: http.SameSiteLaxMode})
}

// member wraps pages that need a login.
func (s *Server) member(h func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		h(w, withUser(r, u), u)
	}
}

// viewer wraps pages anyone can see when pages are public. They show the
// logged-in user's listens, or the site owner's for visitors.
func (s *Server) viewer(h func(http.ResponseWriter, *http.Request, *store.User, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil && !s.cfg.PublicPages {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		owner := u
		if owner == nil {
			first, err := s.db.FirstUser(r.Context())
			if errors.Is(err, store.ErrNotFound) {
				s.render(w, http.StatusOK, "setup", Page{Title: "Almost there"})
				return
			}
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			owner = &first
		}
		h(w, withUser(r, u), u, *owner)
	}
}

// SetMusicBrainz replaces the MusicBrainz client, for tests.
func (s *Server) SetMusicBrainz(c *musicbrainz.Client) { s.mb = c }

// artFile serves a stored picture. Files are named by their content and
// never change, so browsers keep them for good.
func (s *Server) artFile(w http.ResponseWriter, r *http.Request) {
	m := artwork.FileName.FindStringSubmatch(r.PathValue("name"))
	if m == nil {
		s.notFound(w, r)
		return
	}
	if !s.cfg.PublicPages && s.sessionUser(r) == nil {
		s.notFound(w, r)
		return
	}
	format, size := "jpeg", 0
	if m[3] == "png" {
		format = "png"
	}
	fmt.Sscan(m[2], &size)
	f, err := os.Open(s.art.File(m[1], format, size))
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", map[string]string{"jpeg": "image/jpeg", "png": "image/png"}[format])
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, f)
}

// SetFinder replaces the picture finder, for tests.
func (s *Server) SetFinder(f *artwork.Finder) { s.finder = f }
