package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

// Review: merge suggestions and text that still needs a song, biggest
// first, with checkboxes to answer many at once.

const reviewRows = 100

type sideView struct {
	Name    name
	Version string
	Artists []name
	Albums  []name
}

type suggestionRow struct {
	ID      int64
	Kind    string // Artist, Song or Album
	A, B    sideView
	Reason  string
	Listens int
}

type textRow struct {
	SourceID int64
	Fix      string
	Artist   string
	Title    string
	Album    string
	Listens  int
	Checked  bool
	LinkedTo *sideView  // where it's linked now, if anywhere
	Choices  []sideView // Which one?: the songs it fits
	ChoiceID []int64
}

type reviewPage struct {
	Page
	Suggestions     []suggestionRow
	SuggestionCount int
	WhichOne        []textRow
	Texts           []textRow // unlinked text, or text matching TextQuery
	UnlinkedCount   int
	TextQuery       string
	Incomplete      []textRow
	Query           string
	Results         []recordingRow
	Graveyard       []graveRow
	Empty           bool
}

type graveRow struct {
	Song    name
	Artists []name
	Listens int
	Moved   string // when, and by which agent
}

var kindLabel = map[string]string{"artist": "Artist", "recording": "Song", "release": "Album"}

// side shows one side of a suggestion. infos has the page's recordings,
// looked up together.
func (s *Server) side(ctx context.Context, userID int64, kind string, id int64, infos map[int64]store.RecordingInfo) (sideView, error) {
	switch kind {
	case "recording":
		ri, ok := infos[id]
		if !ok {
			one, err := s.db.RecordingInfos(ctx, []int64{id})
			if err != nil {
				return sideView{}, err
			}
			ri = one[id]
		}
		return sideView{Name: songName(ri.Song), Version: ri.Version, Artists: artistNames(ri.Artists)}, nil
	case "release":
		refs, err := s.db.ReleaseRefs(ctx, []int64{id})
		if err != nil {
			return sideView{}, err
		}
		artists, err := s.db.ReleaseArtists(ctx, id)
		return sideView{Name: albumName(refs[id]), Artists: artistNames(artists)}, err
	}
	e, err := s.db.Entity(ctx, userID, "artist", id)
	if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	return sideView{Name: artistName(e.Ref)}, err
}

func (s *Server) reviewData(r *http.Request, u *store.User) (reviewPage, error) {
	ctx := r.Context()
	p := reviewPage{Page: Page{Title: "Review", Nav: "review", User: u}}
	s.doneNotice(r, u, &p.Page)

	sugs, total, err := s.db.OpenSuggestions(ctx, u.ID, reviewRows)
	if err != nil {
		return p, err
	}
	p.SuggestionCount = total
	var recs []int64
	for _, sg := range sugs {
		if sg.Kind == "recording" {
			recs = append(recs, sg.A, sg.B)
		}
	}
	infos, err := s.db.RecordingInfos(ctx, recs)
	if err != nil {
		return p, err
	}
	for _, sg := range sugs {
		row := suggestionRow{ID: sg.ID, Kind: kindLabel[sg.Kind], Reason: sg.Reason, Listens: sg.Listens}
		if row.A, err = s.side(ctx, u.ID, sg.Kind, sg.A, infos); err != nil {
			return p, err
		}
		if row.B, err = s.side(ctx, u.ID, sg.Kind, sg.B, infos); err != nil {
			return p, err
		}
		p.Suggestions = append(p.Suggestions, row)
	}

	rt, err := resolve.Review(ctx, s.db, u.ID)
	if err != nil {
		return p, err
	}
	checked := map[int64]bool{}
	for _, v := range r.URL.Query()["s"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			checked[id] = true
		}
	}
	text := func(src store.UnlinkedSource) textRow {
		return textRow{SourceID: src.ID, Fix: fmt.Sprintf("/listen/%d/fix", src.LatestListen), Artist: src.Artist, Title: src.Title,
			Album: src.Album, Listens: src.Listens, Checked: checked[src.ID]}
	}
	for _, c := range rt.WhichOne[:min(reviewRows, len(rt.WhichOne))] {
		row := text(c.UnlinkedSource)
		for _, id := range c.Candidates {
			sv, err := s.side(ctx, u.ID, "recording", id, nil)
			if err != nil {
				return p, err
			}
			// Same-named songs are told apart by their albums.
			rels, err := s.db.RecordingReleases(ctx, id)
			if err != nil {
				return p, err
			}
			sv.Albums = albumNames(rels)
			row.Choices = append(row.Choices, sv)
			row.ChoiceID = append(row.ChoiceID, id)
		}
		p.WhichOne = append(p.WhichOne, row)
	}
	// Received text to link several at once: what matches the text search,
	// or else what isn't linked.
	p.UnlinkedCount = len(rt.Unlinked)
	texts := rt.Unlinked[:min(reviewRows, len(rt.Unlinked))]
	if t := r.URL.Query().Get("t"); t != "" {
		p.TextQuery = t
		if texts, err = s.db.SearchSources(ctx, u.ID, t, reviewRows); err != nil {
			return p, err
		}
	}
	var linked []int64
	for _, src := range texts {
		if src.RecordingID.Valid {
			linked = append(linked, src.RecordingID.Int64)
		}
	}
	linkedInfos, err := s.db.RecordingInfos(ctx, linked)
	if err != nil {
		return p, err
	}
	for _, src := range texts {
		row := text(src)
		if src.RecordingID.Valid {
			sv, err := s.side(ctx, u.ID, "recording", src.RecordingID.Int64, linkedInfos)
			if err != nil {
				return p, err
			}
			row.LinkedTo = &sv
		}
		p.Texts = append(p.Texts, row)
	}
	for _, src := range rt.Incomplete[:min(reviewRows, len(rt.Incomplete))] {
		p.Incomplete = append(p.Incomplete, text(src))
	}

	// Linking checked text: search for the song to link it to.
	if q := r.URL.Query().Get("q"); q != "" {
		p.Query = q
		found, err := s.db.SearchRecordings(ctx, u.ID, q, 20)
		if err != nil {
			return p, err
		}
		for i, f := range found {
			p.Results = append(p.Results, recordingRow{Rank: i + 1, RecordingID: f.RecordingID, Song: songName(f.Song), Version: f.Version, Artists: artistNames(f.Artists), Listens: f.Listens})
		}
	}
	graves, err := s.db.Graveyard(ctx, u.ID)
	if err != nil {
		return p, err
	}
	for _, g := range graves {
		moved := time.Unix(g.BuriedAt, 0).In(location(*u)).Format("2 Jan 2006")
		if g.By != "" {
			moved += " by " + g.By
		}
		p.Graveyard = append(p.Graveyard, graveRow{songName(g.Ref), artistNames(g.Artists), g.Listens, moved})
	}
	p.Empty = len(p.Suggestions)+len(p.WhichOne)+p.UnlinkedCount+len(p.Incomplete)+len(p.Graveyard) == 0
	return p, nil
}

func albumNames(rs []store.Ref) []name {
	out := make([]name, len(rs))
	for i, r := range rs {
		out[i] = albumName(r)
	}
	return out
}

func (s *Server) review(w http.ResponseWriter, r *http.Request, u *store.User) {
	p, err := s.reviewData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "review", p)
}

func (s *Server) reviewError(w http.ResponseWriter, r *http.Request, u *store.User, msg string) {
	p, err := s.reviewData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Error = msg
	s.render(w, http.StatusBadRequest, "review", p)
}

func formIDs(r *http.Request, key string) []int64 {
	r.ParseForm()
	var out []int64
	for _, v := range r.PostForm[key] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) answerSuggestions(w http.ResponseWriter, r *http.Request, u *store.User) {
	ids := formIDs(r, "suggestion")
	if len(ids) == 0 {
		s.reviewError(w, r, u, "Check at least one suggestion first.")
		return
	}
	editID, n, err := s.db.AnswerSuggestions(r.Context(), u.ID, ids, r.PostFormValue("answer"))
	switch {
	case errors.Is(err, store.ErrAnswer):
		s.reviewError(w, r, u, "Same song, different version only fits songs. Check only songs for that.")
		return
	case errors.Is(err, store.ErrNotFound):
		s.reviewError(w, r, u, "Those suggestions were already answered.")
		return
	case errors.Is(err, store.ErrStale):
		s.reviewError(w, r, u, "Something changed at the same time. Try again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("suggestions answered", "user", u.Name, "count", n, "answer", r.PostFormValue("answer"))
	http.Redirect(w, r, fmt.Sprintf("/review?done=%d", editID), http.StatusSeeOther)
}

func (s *Server) reviewLink(w http.ResponseWriter, r *http.Request, u *store.User) {
	sources := formIDs(r, "s")
	rec, err := strconv.ParseInt(r.PostFormValue("recording"), 10, 64)
	if len(sources) == 0 {
		s.reviewError(w, r, u, "Check what to link first.")
		return
	}
	if err != nil {
		s.reviewError(w, r, u, "Pick a song first.")
		return
	}
	res, err := s.db.LinkSources(r.Context(), u.ID, sources, rec)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.reviewError(w, r, u, "That song or text isn't there anymore. Try again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("linked from review", "user", u.Name, "texts", len(sources), "recording", rec)
	http.Redirect(w, r, fmt.Sprintf("/review?done=%d", res.EditID), http.StatusSeeOther)
}
