package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chokominto/internal/store"
)

type scrobblePage struct {
	Page
	Query   string
	Results []recordingRow
	Now     string
}

const scrobbleResults = 30

func (s *Server) scrobbleData(r *http.Request, u *store.User, query string) (scrobblePage, error) {
	p := scrobblePage{
		Page:  Page{Title: "Scrobble", Nav: "scrobble", User: u, Notice: notices[r.URL.Query().Get("notice")]},
		Query: query,
		Now:   time.Now().In(location(*u)).Format("2006-01-02T15:04"),
	}
	if strings.TrimSpace(query) == "" {
		return p, nil
	}
	found, err := s.db.SearchRecordings(r.Context(), u.ID, query, scrobbleResults)
	if err != nil {
		return p, err
	}
	for i, f := range found {
		p.Results = append(p.Results, recordingRow{Rank: i + 1, RecordingID: f.RecordingID, Song: songName(f.Song), Version: f.Version, Artists: artistNames(f.Artists), Listens: f.Listens})
	}
	return p, nil
}

func (s *Server) scrobblePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	p, err := s.scrobbleData(r, u, r.URL.Query().Get("q"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "scrobble", p)
}

func (s *Server) scrobble(w http.ResponseWriter, r *http.Request, u *store.User) {
	query := r.PostFormValue("q")
	fail := func(msg string) {
		p, err := s.scrobbleData(r, u, query)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		p.Error = msg
		s.render(w, http.StatusBadRequest, "scrobble", p)
	}
	id, err := strconv.ParseInt(r.PostFormValue("recording"), 10, 64)
	if err != nil {
		fail("Pick a song first.")
		return
	}
	at := time.Now()
	if v := strings.TrimSpace(r.PostFormValue("at")); v != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", v, location(*u))
		if err != nil {
			fail("That time isn't recognized.")
			return
		}
		if t.After(time.Now().Add(time.Minute)) {
			fail("That time is in the future.")
			return
		}
		at = t
	}
	_, err = s.db.ScrobbleRecording(r.Context(), u.ID, id, at.Unix())
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail("That song doesn't exist anymore. Search again.")
		return
	case errors.Is(err, store.ErrNameClash):
		fail("Another song has exactly the same name and artist, so this one can't be scrobbled by hand yet.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("scrobbled by hand", "user", u.Name, "recording", id)
	http.Redirect(w, r, "/scrobble?"+url.Values{"q": {query}, "notice": {"scrobbled"}}.Encode(), http.StatusSeeOther)
}
