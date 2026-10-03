package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"chokominto/internal/store"
)

// Pages with JavaScript keep a few parts current while they're open: what's
// playing, the newest listens and the note about sorting out an import.
// static/live.js asks /live for the parts the page has and swaps them in.
// Without script the pages are the same, just as of when they loaded.

// playingBox is what's playing, with its song, artists, album and cover
// when the text was linked before. Otherwise it's the text as sent.
type playingBox struct {
	listenRow
	Cover string // 440 px, or "" for a plain square
}

// nowPlaying looks up what the owner is playing. It never stores anything,
// so text that was never received shows as sent until it's scrobbled.
func (s *Server) nowPlaying(ctx context.Context, owner store.User) (*playingBox, error) {
	t, ok := s.np.Get(owner.ID)
	if !ok {
		return nil, nil
	}
	rec, rel, err := s.db.SourceLink(ctx, owner.ID, store.SourceText{Artist: t.Artist, Title: t.Title, Album: t.Album, AlbumArtist: t.AlbumArtist})
	if err != nil {
		return nil, err
	}
	l := store.Listen{Artist: t.Artist, Title: t.Title, Album: t.Album, RecordingID: rec, ReleaseID: rel}
	rows, err := s.listenRows(ctx, []store.Listen{l}, location(owner), false)
	if err != nil {
		return nil, err
	}
	b := &playingBox{listenRow: rows[0]}
	b.Cover = strings.Replace(b.Art, "-64.", "-440.", 1)
	return b, nil
}

// recentListens is the Recent listens table on Home.
func (s *Server) recentListens(ctx context.Context, owner store.User, viewer *store.User) ([]listenRow, error) {
	ls, err := s.db.Listens(ctx, owner.ID, store.ListenRange{Limit: 10})
	if err != nil {
		return nil, err
	}
	return s.listenRows(ctx, ls, location(owner), viewer != nil)
}

// liveParts is the answer to /live. Each part is filled in only when asked
// for. Sorting is how many songs are left to sort out, and its note.
type liveParts struct {
	Playing *string      `json:"playing,omitempty"`
	Recent  *string      `json:"recent,omitempty"`
	History *string      `json:"history,omitempty"`
	Picture *string      `json:"picture,omitempty"`
	Sorting *liveSorting `json:"sorting,omitempty"`
}

type liveSorting struct {
	Left int    `json:"left"`
	Text string `json:"text"`
}

// live answers with the parts listed in "parts", each rendered the same way
// as on the page.
func (s *Server) live(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	ctx := r.Context()
	var out liveParts
	render := func(name string, data any) (*string, error) {
		var b strings.Builder
		err := s.pages["home"].ExecuteTemplate(&b, name, data)
		v := b.String()
		return &v, err
	}
	var err error
	for _, part := range strings.Split(r.URL.Query().Get("parts"), ",") {
		// The Picture part of an Edit view names its item: picture:artist:12.
		if kind, num, ok := strings.Cut(strings.TrimPrefix(part, "picture:"), ":"); ok && strings.HasPrefix(part, "picture:") && viewer != nil {
			id, _ := strconv.ParseInt(num, 10, 64)
			var p *pictureData
			if p, err = s.loadPicture(ctx, viewer, kind, id); err == nil && p != nil {
				out.Picture, err = render("picture-found", p)
			}
		}
		switch part {
		case "playing":
			var p *playingBox
			if p, err = s.nowPlaying(ctx, owner); err == nil {
				out.Playing, err = render("now-playing-box", p)
			}
		case "recent":
			var rows []listenRow
			if rows, err = s.recentListens(ctx, owner, viewer); err == nil {
				out.Recent, err = render("recent-listens", homePage{Page: Page{User: viewer}, Recent: rows})
			}
		case "history":
			p := historyPage{Page: Page{User: viewer}}
			if err = s.historyDays(ctx, nil, viewer, owner, &p); err == nil {
				out.History, err = render("history-days", p)
			}
		case "sorting":
			var n int
			if n, err = s.db.PendingJobs(ctx, "resolve"); err == nil {
				out.Sorting = &liveSorting{n, sortingText(n)}
			}
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
