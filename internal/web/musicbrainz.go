package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"chokominto/internal/musicbrainz"
	"chokominto/internal/store"
)

// "Get names from MusicBrainz" on artist, song and album pages. Nothing is
// asked of MusicBrainz until the owner opens this page from the button.

// Version is the running version, for the User-Agent MusicBrainz requires.
var Version = "dev"

func userAgent(publicURL string) string {
	ua := "Chokominto/" + Version
	if publicURL != "" {
		ua += " ( " + publicURL + " )"
	}
	return ua
}

type mbName struct {
	Name  string
	Kind  string // English, romaji, original script, or "" for a guess
	There bool   // the item has it already
}

type mbLink struct {
	Value string // "<table>:<artist id>"
	Label string // what it adds
	Name  store.Ref
}

type mbPage struct {
	Page
	Item        name
	Path        string
	Noun        string
	Query       string
	Matches     []musicbrainz.Match
	Found       *musicbrainz.Entity
	Names       []mbName
	Links       []mbLink
	Missing     []string // related artists that aren't in the owner's listens
	WebURL      string   // the entry on musicbrainz.org
	Released    string
	NewReleased bool
}

var mbWebPaths = map[string]string{"artist": "artist", "song": "work", "release": "release"}

func (s *Server) mbData(r *http.Request, u *store.User, kind string, id int64) (mbPage, error) {
	ctx := r.Context()
	e, err := s.db.Entity(ctx, u.ID, kind, id)
	if err != nil {
		return mbPage{}, err
	}
	if e.MergedInto != 0 {
		return mbPage{}, store.ErrNotFound
	}
	p := mbPage{Page: Page{Title: "Names from MusicBrainz", User: u}, Item: name{Name: e.Name, Other: e.OtherNames},
		Path: fmt.Sprintf("%s/%d", pagePaths[kind], id), Noun: nouns[kind]}
	p.Item.Href = p.Path

	q := r.URL.Query()
	mbid := q.Get("mbid")
	if mbid == "" && q.Get("q") == "" {
		if mbid, err = s.db.ItemMBID(ctx, kind, id); err != nil {
			return p, err
		}
	}
	if mbid == "" || !musicbrainz.ValidMBID(mbid) {
		p.Query = q.Get("q")
		if p.Query == "" {
			p.Query = e.Name
		}
		p.Matches, err = s.mb.Search(ctx, kind, p.Query)
		return p, err
	}
	found, err := s.mb.Lookup(ctx, kind, mbid)
	if err != nil {
		return p, err
	}
	p.Found = &found
	p.WebURL = fmt.Sprintf("https://%s/%s/%s", musicbrainz.Host, mbWebPaths[kind], mbid)
	have, err := s.db.Aliases(ctx, kind, id)
	if err != nil {
		return p, err
	}
	for _, n := range found.Names {
		there := slices.ContainsFunc(have, func(a store.Alias) bool { return a.Name == n.Name })
		p.Names = append(p.Names, mbName{n.Name, langLabel[n.Lang], there})
	}
	if kind == "release" && found.Released != "" {
		d, err := s.db.Album(ctx, id)
		if err != nil {
			return p, err
		}
		p.Released, p.NewReleased = found.Released, d.Released == ""
	}
	// Members and the person behind a persona, when they're in the
	// owner's listens.
	add := func(rels []musicbrainz.Relation, table, label string) error {
		for _, rel := range rels {
			a, err := s.db.FindArtist(ctx, u.ID, rel.Name)
			if errors.Is(err, store.ErrNoArtist) {
				p.Missing = append(p.Missing, rel.Name)
				continue
			}
			if err != nil {
				return err
			}
			if a.ID != id {
				p.Links = append(p.Links, mbLink{fmt.Sprintf("%s:%d", table, a.ID), label, a})
			}
		}
		return nil
	}
	if err := add(found.Members, "group_members", "Member"); err != nil {
		return p, err
	}
	if err := add(found.PersonOf, "artist_counts_for", "Also counts for"); err != nil {
		return p, err
	}
	return p, nil
}

var langLabel = map[string]string{"en": "English", "romaji": "Romaji", "original": "Original script", "": ""}

const mbUnreachable = "MusicBrainz didn't answer. Try again in a minute."

func (s *Server) musicBrainzPage(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		p, err := s.mbData(r, u, kind, id)
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		if err != nil {
			if p.Path == "" {
				s.serverError(w, r, err)
				return
			}
			s.log.Warn("musicbrainz", "err", err)
			p.Error = mbUnreachable
			p.Found, p.Matches = nil, nil
			s.render(w, http.StatusBadGateway, "musicbrainz", p)
			return
		}
		s.render(w, http.StatusOK, "musicbrainz", p)
	}
}

// importMusicBrainz adds what the page showed. The names are looked up
// again here, so nothing but the choice of links comes from the form.
func (s *Server) importMusicBrainz(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		mbid := r.PostFormValue("mbid")
		if err != nil || !musicbrainz.ValidMBID(mbid) {
			s.notFound(w, r)
			return
		}
		ctx := r.Context()
		found, err := s.mb.Lookup(ctx, kind, mbid)
		if err != nil {
			s.log.Warn("musicbrainz", "err", err)
			http.Redirect(w, r, fmt.Sprintf("%s/%d/musicbrainz?mbid=%s", pagePaths[kind], id, mbid), http.StatusSeeOther)
			return
		}
		var names []store.ImportName
		for _, n := range found.Names {
			names = append(names, store.ImportName{Name: n.Name, Lang: n.Lang})
		}
		links, err := s.chosenLinks(ctx, r, u, found)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		editID, err := s.db.ImportNames(ctx, u.ID, kind, id, mbid, names, links, found.Released)
		path := fmt.Sprintf("%s/%d/edit", pagePaths[kind], id)
		if err != nil {
			code := editErrorCode(err)
			if code == "" {
				s.serverError(w, r, err)
				return
			}
			http.Redirect(w, r, path+"?err="+code, http.StatusSeeOther)
			return
		}
		s.log.Info("names from musicbrainz", "user", u.Name, "kind", kind, "id", id)
		http.Redirect(w, r, fmt.Sprintf("%s?done=%d", path, editID), http.StatusSeeOther)
	}
}

// chosenLinks keeps the checked links that MusicBrainz really gave.
func (s *Server) chosenLinks(ctx context.Context, r *http.Request, u *store.User, found musicbrainz.Entity) ([]store.ImportLink, error) {
	r.ParseForm()
	checked := map[string]bool{}
	for _, v := range r.PostForm["link"] {
		checked[v] = true
	}
	var out []store.ImportLink
	for table, rels := range map[string][]musicbrainz.Relation{"group_members": found.Members, "artist_counts_for": found.PersonOf} {
		for _, rel := range rels {
			a, err := s.db.FindArtist(ctx, u.ID, rel.Name)
			if errors.Is(err, store.ErrNoArtist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if checked[fmt.Sprintf("%s:%d", table, a.ID)] {
				note := ""
				if table == "artist_counts_for" {
					note = "person"
				}
				out = append(out, store.ImportLink{Table: table, To: a.ID, Note: note})
			}
		}
	}
	return out, nil
}
