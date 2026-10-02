package listenbrainz

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"chokominto/internal/auth"
	"chokominto/internal/ingest"
	"chokominto/internal/store"
)

// Prefixes the API is served under. /1 is standard. The others are where
// Maloja served it, so scrobblers set up for Maloja only need a new host.
var Prefixes = []string{"/1", "/apis/listenbrainz/1", "/apis/lbrnz/1"}

type API struct {
	DB          *store.DB
	NowPlaying  *NowPlaying
	PublicReads bool // read endpoints work without a token, like public pages
	Log         *slog.Logger
}

// Register adds every route under each prefix.
func (a *API) Register(mux *http.ServeMux) {
	for _, p := range Prefixes {
		handle := func(pattern string, h http.HandlerFunc) {
			method, path, _ := strings.Cut(pattern, " ")
			if path == "" {
				method, path = "", method
			} else {
				method += " "
			}
			mux.Handle(method+p+path, withCORS(h))
		}
		handle("POST /submit-listens", a.submit)
		handle("GET /validate-token", a.validateToken)
		handle("GET /user/{name}/listens", a.listens)
		handle("GET /user/{name}/playing-now", a.playingNow)
		handle("GET /user/{name}/listen-count", a.listenCount)
		handle("POST /delete-listen", a.deleteListen)

		handle("GET /stats/user/{name}/{kind}", a.stats)
		handle("GET /stats/user/{name}/listening-activity", a.listeningActivity)

		// Not supported yet. These answer the way ListenBrainz does when
		// there's nothing to show, so Pano Scrobbler shows an empty screen
		// instead of an error.
		handle("GET /feedback/user/{name}/get-feedback", a.noContent)
		handle("GET /user/{name}/following", a.noContent)
		handle("GET /user/{name}/followers", a.noContent)
		handle("GET /metadata/lookup", a.emptyObject)
		handle("POST /feedback/recording-feedback", a.lovesUnsupported)

		handle("OPTIONS /", a.preflight)
		handle("/", a.notFound)
	}
}

// withCORS allows any origin. The API authenticates with tokens and never
// reads cookies, so another site can't do anything with it that it couldn't
// do with the token directly.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h.ServeHTTP(w, r)
	})
}

// IsAPIPath reports whether a request path belongs to the API. The web
// server uses it to skip cookie-related protections.
func IsAPIPath(path string) bool {
	for _, p := range Prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError uses ListenBrainz's error shape. Every reply is JSON, because
// Web Scrobbler parses every response body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": status, "error": msg})
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.Error("listenbrainz api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "Something went wrong on the server.")
}

// tokenFrom reads "Authorization: Token <t>" with any capitalization of the
// scheme. Pano Scrobbler sends "token", Web Scrobbler sends "Token".
func tokenFrom(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, rest, ok := strings.Cut(h, " ")
	if ok && (strings.EqualFold(scheme, "token") || strings.EqualFold(scheme, "bearer")) {
		return strings.TrimSpace(rest)
	}
	return ""
}

type caller struct {
	userID  int64
	tokenID int64
	user    store.User
}

// authenticate returns the token's user, or writes a 401.
func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (caller, bool) {
	t := tokenFrom(r)
	if t == "" {
		writeError(w, http.StatusUnauthorized, "You need to provide an Authorization header.")
		return caller{}, false
	}
	tokenID, userID, err := a.DB.TokenUser(r.Context(), auth.HashSecret(t))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "Invalid authorization token.")
		return caller{}, false
	}
	if err != nil {
		a.internalError(w, r, err)
		return caller{}, false
	}
	u, err := a.DB.UserByID(r.Context(), userID)
	if err != nil {
		a.internalError(w, r, err)
		return caller{}, false
	}
	return caller{userID, tokenID, u}, true
}

func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	c, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "Payload too large.")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Could not read the request.")
		return
	}
	sub, err := DecodeSubmit(body)
	var de *DecodeError
	if errors.As(err, &de) {
		writeError(w, http.StatusBadRequest, de.Msg)
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}

	if sub.Type == "playing_now" {
		l := sub.Listens[0]
		a.NowPlaying.Set(c.userID, Track{Artist: l.Artist, Title: l.Title, Album: l.Album, AlbumArtist: l.AlbumArtist}, sub.DurationMS)
		resp := map[string]any{"status": "ok"}
		if r.URL.Query().Get("return_msid") == "true" {
			if msid, err := a.DB.SourceMSID(r.Context(), c.userID, store.SourceText{Artist: l.Artist, Title: l.Title, Album: l.Album, AlbumArtist: l.AlbumArtist}); err == nil {
				resp["recording_msid"] = msid
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	res, err := ingest.Store(r.Context(), a.DB, c.userID, "listenbrainz", &c.tokenID, sub.Listens)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.Log.Info("listens received", "user", c.user.Name, "type", sub.Type, "stored", res.Stored, "duplicates", res.Duplicates)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *API) validateToken(w http.ResponseWriter, r *http.Request) {
	t := tokenFrom(r)
	if t == "" {
		t = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if t == "" {
		writeError(w, http.StatusBadRequest, "You need to provide an Authorization token.")
		return
	}
	_, userID, err := a.DB.TokenUser(r.Context(), auth.HashSecret(t))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 200, "message": "Token invalid.", "valid": false})
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	u, err := a.DB.UserByID(r.Context(), userID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "message": "Token valid.", "valid": true, "user_name": u.Name})
}

// readUser resolves {name} for read endpoints. When pages aren't public, the
// caller's token must belong to that user.
func (a *API) readUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	name := r.PathValue("name")
	u, err := a.DB.UserByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Cannot find user: "+name)
		return u, false
	}
	if err != nil {
		a.internalError(w, r, err)
		return u, false
	}
	if !a.PublicReads {
		c, ok := a.authenticate(w, r)
		if !ok {
			return u, false
		}
		if c.userID != u.ID {
			writeError(w, http.StatusForbidden, "You can only read your own listens.")
			return u, false
		}
	}
	return u, true
}

type trackMetadata struct {
	ArtistName     string         `json:"artist_name"`
	TrackName      string         `json:"track_name"`
	ReleaseName    *string        `json:"release_name"`
	AdditionalInfo map[string]any `json:"additional_info,omitempty"`
}

type listenJSON struct {
	ListenedAt    int64         `json:"listened_at,omitempty"`
	InsertedAt    int64         `json:"inserted_at,omitempty"`
	RecordingMSID string        `json:"recording_msid,omitempty"`
	UserName      string        `json:"user_name"`
	PlayingNow    bool          `json:"playing_now,omitempty"`
	TrackMetadata trackMetadata `json:"track_metadata"`
}

func meta(artist, title, album, msid string) trackMetadata {
	m := trackMetadata{ArtistName: artist, TrackName: title}
	if album != "" {
		m.ReleaseName = &album
	}
	if msid != "" {
		m.AdditionalInfo = map[string]any{"recording_msid": msid}
	}
	return m
}

func queryInt(r *http.Request, key string) (int64, bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false, errors.New(key + " must be a positive whole number")
	}
	return n, true, nil
}

func (a *API) listens(w http.ResponseWriter, r *http.Request) {
	u, ok := a.readUser(w, r)
	if !ok {
		return
	}
	count, has, err := queryInt(r, "count")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !has {
		count = 25
	}
	count = min(max(count, 1), 1000)
	rng := store.ListenRange{Limit: int(count)}
	maxTS, hasMax, err := queryInt(r, "max_ts")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	minTS, hasMin, err := queryInt(r, "min_ts")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if hasMax {
		c := store.CursorAt(maxTS)
		rng.Before = &c
	}
	if hasMin {
		c := store.CursorAfter(minTS)
		rng.After = &c
		// Only a lower bound: return the listens just after it, like
		// ListenBrainz does.
		rng.Oldest = !hasMax
	}
	ls, err := a.DB.Listens(r.Context(), u.ID, rng)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	_, oldest, newest, err := a.DB.ListenStats(r.Context(), u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]listenJSON, len(ls))
	for i, l := range ls {
		out[i] = listenJSON{
			ListenedAt:    l.ListenedAt,
			InsertedAt:    l.ReceivedAt,
			RecordingMSID: l.MSID,
			UserName:      u.Name,
			TrackMetadata: meta(l.Artist, l.Title, l.Album, l.MSID),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{
		"count":            len(out),
		"user_id":          u.Name,
		"listens":          out,
		"latest_listen_ts": newest,
		"oldest_listen_ts": oldest,
	}})
}

func (a *API) playingNow(w http.ResponseWriter, r *http.Request) {
	u, ok := a.readUser(w, r)
	if !ok {
		return
	}
	listens := []listenJSON{}
	if t, ok := a.NowPlaying.Get(u.ID); ok {
		listens = append(listens, listenJSON{UserName: u.Name, PlayingNow: true, TrackMetadata: meta(t.Artist, t.Title, t.Album, "")})
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{
		"count":       len(listens),
		"user_id":     u.Name,
		"playing_now": true,
		"listens":     listens,
	}})
}

func (a *API) listenCount(w http.ResponseWriter, r *http.Request) {
	u, ok := a.readUser(w, r)
	if !ok {
		return
	}
	n, _, _, err := a.DB.ListenStats(r.Context(), u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{"count": n}})
}

func (a *API) deleteListen(w http.ResponseWriter, r *http.Request) {
	c, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Could not read the request.")
		return
	}
	var req struct {
		ListenedAt    json.RawMessage `json:"listened_at"`
		RecordingMSID string          `json:"recording_msid"`
	}
	if err := unmarshalObject(body, &req); err != nil || req.ListenedAt == nil || req.RecordingMSID == "" {
		writeError(w, http.StatusBadRequest, "listened_at and recording_msid are required.")
		return
	}
	at, err := decodeTime(req.ListenedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	l, err := a.DB.FindListen(r.Context(), c.userID, at, req.RecordingMSID)
	if errors.Is(err, store.ErrNotFound) {
		// Already gone. Deleting is idempotent, like ListenBrainz.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if _, err := a.DB.DeleteListen(r.Context(), c.userID, l); err != nil && !errors.Is(err, store.ErrStale) {
		a.internalError(w, r, err)
		return
	}
	a.Log.Info("listen deleted", "user", c.user.Name, "listen", l.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *API) noContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) emptyObject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *API) lovesUnsupported(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "Loves aren't supported.")
}

func (a *API) preflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "No such method.")
}
