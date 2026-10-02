package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

// The Fix page: one listen's received text, what it's linked to, relinking
// by search or "New song", "remember this?" rules and deleting the listen.
// See docs/architecture.md, "Review and fixing".

type field struct {
	Label, Value string
}

type offerRow struct {
	Kind    string
	Text    string
	Listens int
}

type fixPage struct {
	Page
	ID       int64
	Action   string // base path for the page's forms
	Received []field
	PageURL  string
	Linked   bool
	Song     name
	Version  string
	Artists  []name
	Album    *name
	Same     int // listens with exactly this text
	Query    string
	Results  []recordingRow
	Offers   []offerRow
	Target   int64 // the recording the offers are for
}

const fixResults = 20

func listenID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) fixData(r *http.Request, u *store.User, id int64, query string, searched bool) (fixPage, error) {
	ctx := r.Context()
	f, err := s.db.FixListen(ctx, u.ID, id)
	if err != nil {
		return fixPage{}, err
	}
	p := fixPage{Page: Page{Title: "Fix listen", User: u}, ID: id, Action: fmt.Sprintf("/listen/%d", id), Same: f.Source.Listens}
	s.doneNotice(r, u, &p.Page)
	loc := location(*u)
	p.Received = []field{
		{"When", time.Unix(f.ListenedAt, 0).In(loc).Format("2 Jan 2006, 15:04")},
		{"Artist", f.Artist},
		{"Title", f.Title},
		{"Album", f.Album},
	}
	if f.Source.AlbumArtist != "" {
		p.Received = append(p.Received, field{"Album artist", f.Source.AlbumArtist})
	}
	if f.Client != "" {
		p.Received = append(p.Received, field{"Sent by", f.Client})
	}
	if u, err := url.Parse(f.Page); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		p.PageURL = f.Page
	}

	rows, err := s.listenRows(ctx, []store.Listen{f.Listen}, loc, false)
	if err != nil {
		return p, err
	}
	if row := rows[0]; row.Linked {
		p.Linked, p.Song, p.Version, p.Artists, p.Album = true, row.Song, row.Version, row.Artists, row.Album
	}

	// The search starts from the title as received.
	if !searched {
		query = f.Title
	}
	p.Query = query
	found, err := s.db.SearchRecordings(ctx, u.ID, query, fixResults)
	if err != nil {
		return p, err
	}
	for i, fr := range found {
		if fr.RecordingID == f.RecordingID {
			continue // linked there already
		}
		p.Results = append(p.Results, recordingRow{Rank: i + 1, RecordingID: fr.RecordingID, Song: songName(fr.Song), Version: fr.Version, Artists: artistNames(fr.Artists), Listens: fr.Listens})
	}

	// After linking by hand, offer to remember it.
	target, _ := strconv.ParseInt(r.URL.Query().Get("remember"), 10, 64)
	if target > 0 && f.RecordingID == target && f.Source.LinkedByOwner {
		offers, err := resolve.Offers(ctx, s.db, u.ID, f.Source.ID, target)
		if err != nil {
			return p, err
		}
		p.Target = target
		for _, o := range offers {
			p.Offers = append(p.Offers, offerRow{o.Kind, offerText(o, f.Artist, f.Title), o.Listens})
		}
	}
	return p, nil
}

// offerText says what a rule does, in the owner's words.
func offerText(o resolve.Offer, artist, title string) string {
	switch o.Kind {
	case "album":
		return fmt.Sprintf("Always link “%s” by %s here, whatever the album", title, artist)
	case "key":
		return fmt.Sprintf("Always link “%s” by %s here, also when it's written with other capitals, full-width letters or spacing", title, artist)
	}
	switch {
	case o.Prefix == "":
		return fmt.Sprintf("Remove “%s” from the end of titles by %s", o.Suffix, artist)
	case o.Suffix == "":
		return fmt.Sprintf("Remove “%s” from the start of titles by %s", o.Prefix, artist)
	}
	return fmt.Sprintf("Remove “%s” and “%s” from titles by %s", o.Prefix, o.Suffix, artist)
}

func (s *Server) fixPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	_, searched := r.URL.Query()["q"]
	p, err := s.fixData(r, u, id, r.URL.Query().Get("q"), searched)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "fix", p)
}

func (s *Server) fixError(w http.ResponseWriter, r *http.Request, u *store.User, id int64, msg string) {
	p, err := s.fixData(r, u, id, r.PostFormValue("q"), r.PostFormValue("q") != "")
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Error = msg
	s.render(w, http.StatusBadRequest, "fix", p)
}

func (s *Server) fixLink(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rec, err := strconv.ParseInt(r.PostFormValue("recording"), 10, 64)
	if err != nil {
		s.fixError(w, r, u, id, "Pick a song first.")
		return
	}
	res, err := s.db.LinkSources(r.Context(), u.ID, []int64{f.Source.ID}, rec)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "That song isn't there anymore. Search again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("linked by hand", "user", u.Name, "listen", id, "recording", rec)
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?done=%d&remember=%d#remember", id, res.EditID, rec), http.StatusSeeOther)
}

func (s *Server) fixNewSong(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_, res, err := resolve.NewSong(r.Context(), s.db, u.ID, f.Source.ID)
	switch {
	case errors.Is(err, resolve.ErrNothingToGoOn):
		s.fixError(w, r, u, id, "This listen has no artist or title to make a song from.")
		return
	case errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "Something changed at the same time. Try again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("new song by hand", "user", u.Name, "listen", id)
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?done=%d", id, res.EditID), http.StatusSeeOther)
}

func (s *Server) fixRemember(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	target, _ := strconv.ParseInt(r.PostFormValue("recording"), 10, 64)
	kind := r.PostFormValue("kind")
	// Offers are worked out again here, so nothing about the rule comes
	// from the form except which one was picked.
	var offer *resolve.Offer
	if target > 0 && f.RecordingID == target {
		offers, err := resolve.Offers(r.Context(), s.db, u.ID, f.Source.ID, target)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for i := range offers {
			if offers[i].Kind == kind {
				offer = &offers[i]
			}
		}
	}
	if offer == nil {
		s.fixError(w, r, u, id, "That can't be remembered anymore, because the listen was linked somewhere else since.")
		return
	}
	editID, err := resolve.SaveOffer(r.Context(), s.db, u.ID, *offer, offerText(*offer, f.Artist, f.Title))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fixError(w, r, u, id, "That song isn't there anymore.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("rule saved", "user", u.Name, "listen", id, "kind", kind)
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?done=%d", id, editID), http.StatusSeeOther)
}

func (s *Server) fixDelete(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	editID, err := s.db.DeleteListen(r.Context(), u.ID, f.Listen)
	if err != nil && !errors.Is(err, store.ErrStale) {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("listen deleted", "user", u.Name, "listen", id)
	http.Redirect(w, r, fmt.Sprintf("/history?done=%d", editID), http.StatusSeeOther)
}
