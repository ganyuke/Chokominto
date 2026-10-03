package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// versionRow is one version of the song a listen is linked to.
type versionRow struct {
	RecordingID int64
	Version     string
	Artists     []name
	Listens     int
	Current     bool
}

type fixPage struct {
	Page
	ID          int64
	Action      string // base path for the page's forms
	Received    []field
	PageURL     string
	When        string        // when this listen was heard
	Sent        int           // listens sent with exactly this text
	SentHref    string        // those listens in History
	Same        int           // of those, the ones that follow the text
	One         bool          // changes apply to this listen only
	Alone       bool          // this listen was linked on its own already
	Others      string        // alone: what doing the same for the other listens would move
	Scope       string        // "", "one", "album" or "key", as sent with every form
	Scopes      []scopeOption // the choices, the current one marked
	Saved       string        // the rule saved with the move just made
	SavedUndo   int64
	NoRule      bool // the move was made, but its rule couldn't be saved
	ScopeHelp   string
	Move        string // what the move buttons say
	Linked      bool
	Song        name
	Version     string
	Artists     []name
	Album       *name
	By          linkedBy
	SongEdit    string
	AlbumEdit   string
	Scrobbles   string
	Versions    []versionRow
	Query       string
	Results     []recordingRow
	SongHelp    string
	AlbumQuery  string
	Albums      []albumResult
	AlbumHelp   string
	Correct     resolve.Text
	CorrectHelp string
	NewSongHelp string
	DeleteHelp  string
	Offers      []offerRow
	Target      int64 // the recording the offers are for
}

const fixResults = 20

func listenID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// scopeOption is one choice of what a change on the Fix page applies to.
type scopeOption struct {
	Href    string
	Label   string
	Help    string
	Current bool
}

// scopeRule is the rule to save together with a move, when the scope says
// so: "album" (the same artist and title on any album) or "key" (also in
// other spellings).
func scopeRule(r *http.Request) string {
	if v := r.FormValue("scope"); v == "album" || v == "key" {
		return v
	}
	return ""
}

// scopeOne reports whether a request is about one listen only, not every
// listen sent with the same text.
func scopeOne(r *http.Request) bool {
	return r.FormValue("scope") == "one"
}

func listens(n int) string {
	if n == 1 {
		return "1 listen"
	}
	return numberPrinter.Sprintf("%d listens", n)
}

func (s *Server) fixData(r *http.Request, u *store.User, id int64, query string, searched bool) (fixPage, error) {
	ctx := r.Context()
	f, err := s.db.FixListen(ctx, u.ID, id)
	if err != nil {
		return fixPage{}, err
	}
	src, err := s.db.LinkedSourceInfo(ctx, u.ID, f.Source.ID)
	if err != nil {
		return fixPage{}, err
	}
	loc := location(*u)
	p := fixPage{Page: Page{Title: "Fix listen", User: u}, ID: id, Action: fmt.Sprintf("/listen/%d", id),
		When: time.Unix(f.ListenedAt, 0).In(loc).Format("2 Jan 2006, 15:04"),
		Sent: f.Source.Listens, SentHref: fmt.Sprintf("/history?text=%d", f.Source.ID), Same: src.Listens,
		Alone: f.Fixed, One: f.Fixed || scopeOne(r)}
	s.doneNotice(r, u, &p.Page)
	p.Received = []field{
		{"When", p.When},
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
	p.Correct = resolve.Text{Artist: f.Artist, Title: f.Title, Album: f.Album}

	// Scope: every listen with this text, or this one alone.
	p.Scope = scopeRule(r)
	if p.One {
		p.Scope = "one"
	}
	others := p.Same - 1
	if p.Alone {
		others = p.Same
	}
	if p.Alone && f.RecordingID != 0 && (!src.RecordingID.Valid || src.RecordingID.Int64 != f.RecordingID) {
		p.Others = "later listens sent with the same text"
		if others > 0 {
			p.Others = "the other " + listens(others) + " sent with the same text, and later ones"
		}
	}
	moving, what := 1, "this listen"
	switch {
	case p.One:
		p.Move = "Move this listen here"
		p.ScopeHelp = "Changes below move only this listen, from " + p.When + "."
		if others > 0 {
			p.ScopeHelp += numberPrinter.Sprintf(" The other %d sent with the same text stay as they are, and so do later ones.", others)
		} else {
			p.ScopeHelp += " Later listens sent with the same text are not affected."
		}
	case p.Same > 1:
		moving, what = p.Same, numberPrinter.Sprintf("these %d listens", p.Same)
		p.Move = numberPrinter.Sprintf("Move %d listens here", p.Same)
		p.ScopeHelp = numberPrinter.Sprintf("Changes below move all %d listens sent with this text, and later listens sent with the same text.", p.Same)
	default:
		p.Move = "Move this listen here"
		p.ScopeHelp = "Changes below move this listen, and later listens sent with the same text."
	}

	// Every choice is listed before anything is picked, widest last.
	these := "this listen and later ones with this text"
	if p.Same > 1 {
		these = numberPrinter.Sprintf("all %d listens with this text", p.Same)
	}
	p.Scopes = []scopeOption{
		{p.Action + "/fix?scope=one#scope", "Only this listen", "The one from " + p.When + ". Nothing else moves, now or later.", p.Scope == "one"},
		{p.Action + "/fix#scope", strings.ToUpper(these[:1]) + these[1:], "Every listen sent with exactly this artist, title and album, now and later.", p.Scope == ""},
	}
	if strings.TrimSpace(f.Artist) != "" && strings.TrimSpace(f.Title) != "" {
		p.Scopes = append(p.Scopes,
			scopeOption{p.Action + "/fix?scope=album#scope", strings.ToUpper(these[:1]) + these[1:] + ", and the same on any album",
				fmt.Sprintf("Also saves a rule: “%s” by %s always goes where you move it, whatever album is sent. Works with Version and Song below.", f.Title, f.Artist), p.Scope == "album"},
			scopeOption{p.Action + "/fix?scope=key#scope", strings.ToUpper(these[:1]) + these[1:] + ", and the same in any spelling or album",
				"Also saves a rule that covers other capitals, full-width letters and spacing too. Works with Version and Song below.", p.Scope == "key"})
	}
	if p.Scope == "album" || p.Scope == "key" {
		p.ScopeHelp += " A rule is saved with the move, and listed in Settings under Rules."
	}
	if id, err := strconv.ParseInt(r.URL.Query().Get("saved"), 10, 64); err == nil && id > 0 {
		if sums, err := s.db.EditSummaries(ctx, u.ID, []int64{id}); err == nil && len(sums) == 1 {
			p.Saved, p.SavedUndo = sums[0], id
		}
	}
	p.NoRule = r.URL.Query().Get("saved") == "none"

	rows, err := s.listenRows(ctx, []store.Listen{f.Listen}, loc, false)
	if err != nil {
		return p, err
	}
	current := ""
	if row := rows[0]; row.Linked {
		p.Linked, p.Song, p.Version, p.Artists, p.Album = true, row.Song, row.Version, row.Artists, row.Album
		p.By = linkedBy{Text: "Fixed alone"}
		if !p.Alone {
			rules, err := resolve.RuleTexts(ctx, s.db, u.ID)
			if err != nil {
				return p, err
			}
			p.By = linkedByText(src, rules, loc)
		}
		p.SongEdit, p.Scrobbles = row.Song.Href+"/edit", row.Song.Href+"/scrobbles"
		if row.Album != nil {
			p.AlbumEdit = row.Album.Href + "/edit"
		}
		current = row.Song.Name
		if row.Version != "" {
			current += " (" + row.Version + ")"
		}
		// Every version of the song, the most listened first.
		infos, err := s.db.RecordingInfos(ctx, []int64{f.RecordingID})
		if err != nil {
			return p, err
		}
		versions, err := s.db.SongRecordings(ctx, u.ID, infos[f.RecordingID].Song.ID)
		if err != nil {
			return p, err
		}
		left := 0
		for _, v := range versions {
			p.Versions = append(p.Versions, versionRow{v.RecordingID, v.Version, artistNames(v.Artists), v.Listens, v.RecordingID == f.RecordingID})
			if v.RecordingID == f.RecordingID {
				left = v.Listens - moving
			}
		}
		if len(p.Versions) < 2 {
			p.Versions = nil
		}
		p.SongHelp = fmt.Sprintf("Moves %s to the song and version you pick. ", what)
		if left > 0 {
			p.SongHelp += fmt.Sprintf("%s keeps its other %s and all its names. ", current, listens(left))
		} else {
			p.SongHelp += fmt.Sprintf("%s then has no listens left, and keeps its names. ", current)
		}
		p.SongHelp += "Nothing is merged. To join two songs completely, use Merge on the song's Edit tab."
		p.AlbumHelp = fmt.Sprintf("Puts %s on the album you pick. The song stays the same.", what)
		p.NewSongHelp = fmt.Sprintf("Takes %s out of %s and into a new song with the same name and artists. For two different songs that share a name.", what, current)
	} else {
		p.SongHelp = fmt.Sprintf("Links %s to the song and version you pick.", what)
		p.NewSongHelp = fmt.Sprintf("Makes a new song from the received artist and title, and links %s to it.", what)
	}
	p.CorrectHelp = fmt.Sprintf("Type what should have been sent, like the real artist in place of a channel name, or a title with its version in brackets. "+
		"%s to the song, version and album this reads as, which are made when they aren't there yet. The received text itself is kept.",
		strings.ToUpper(what[:1])+what[1:]+map[bool]string{true: " goes", false: " go"}[moving == 1])
	p.DeleteHelp = "Deletes only this listen, from " + p.When + "."
	if p.Sent > 1 {
		p.DeleteHelp += numberPrinter.Sprintf(" The other %d sent with the same text stay.", p.Sent-1)
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
	if p.AlbumQuery = strings.TrimSpace(r.FormValue("album")); p.AlbumQuery != "" && p.Linked {
		albums, err := s.db.SearchItems(ctx, u.ID, "release", p.AlbumQuery, fixResults)
		if err != nil {
			return p, err
		}
		for _, a := range albums {
			if a.ID == f.ReleaseID {
				continue // on it already
			}
			d, err := s.db.Album(ctx, a.ID)
			if err != nil {
				return p, err
			}
			p.Albums = append(p.Albums, albumResult{a.ID, albumName(a), d.Context})
		}
	}

	// After linking every listen with the text by hand, offer to remember it.
	target, _ := strconv.ParseInt(r.URL.Query().Get("remember"), 10, 64)
	if target > 0 && !p.One && f.RecordingID == target && f.Source.LinkedByOwner {
		offers, err := resolve.Offers(ctx, s.db, u.ID, f.Source.ID, target)
		if err != nil {
			return p, err
		}
		p.Target = target
		for _, o := range offers {
			// A rule saved with the move isn't offered again, nor the
			// narrower one it covers.
			if p.Saved != "" && (o.Kind == p.Scope || (p.Scope == "key" && o.Kind == "album")) {
				continue
			}
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
	var res store.LinkResult
	one := f.Fixed || scopeOne(r)
	if one {
		res, err = s.db.LinkListen(r.Context(), u.ID, id, rec, store.AlbumChoice{Keep: true})
	} else {
		res, err = s.db.LinkSources(r.Context(), u.ID, []int64{f.Source.ID}, rec)
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "That song isn't there anymore. Search again.")
		return
	case errors.Is(err, store.ErrBuried):
		s.fixError(w, r, u, id, "That song is in the graveyard. Bring it back first.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("linked by hand", "user", u.Name, "listen", id, "recording", rec, "one", one)
	if one {
		s.fixDone(w, r, id, res.EditID, true, "")
		return
	}
	// The rule the scope asked for is saved with the move.
	q := url.Values{"done": {fmt.Sprint(res.EditID)}, "remember": {fmt.Sprint(rec)}}
	if kind := scopeRule(r); kind != "" {
		q.Set("scope", kind)
		q.Set("saved", "none")
		offers, err := resolve.Offers(r.Context(), s.db, u.ID, f.Source.ID, rec)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, o := range offers {
			if o.Kind != kind {
				continue
			}
			ruleEdit, err := resolve.SaveOffer(r.Context(), s.db, u.ID, o, offerText(o, f.Artist, f.Title))
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			q.Set("saved", fmt.Sprint(ruleEdit))
			s.log.Info("rule saved with a move", "user", u.Name, "listen", id, "kind", kind)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?%s#remember", id, q.Encode()), http.StatusSeeOther)
}

// fixDone goes back to the Fix page after a change, in the same scope, to
// the section the button was in.
func (s *Server) fixDone(w http.ResponseWriter, r *http.Request, id, editID int64, one bool, section string) {
	q := url.Values{}
	if editID != 0 {
		q.Set("done", fmt.Sprint(editID))
	}
	if one {
		q.Set("scope", "one")
	}
	path := fmt.Sprintf("/listen/%d/fix", id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if section != "" {
		path += "#" + section
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// fixTarget loads the listen a Fix form is about.
func (s *Server) fixTarget(w http.ResponseWriter, r *http.Request, u *store.User) (int64, store.FixListen, bool) {
	id, ok := listenID(r)
	if !ok {
		s.notFound(w, r)
		return 0, store.FixListen{}, false
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return id, f, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return id, f, false
	}
	return id, f, true
}

// fixAlbum puts the listens on another album, or on none.
func (s *Server) fixAlbum(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, f, ok := s.fixTarget(w, r, u)
	if !ok {
		return
	}
	album, _ := strconv.ParseInt(r.PostFormValue("to"), 10, 64)
	if f.RecordingID == 0 || (album == 0 && r.PostFormValue("to") != "none") {
		s.fixError(w, r, u, id, "Pick an album first.")
		return
	}
	one := f.Fixed || scopeOne(r)
	var res store.LinkResult
	var err error
	if one {
		res, err = s.db.LinkListen(r.Context(), u.ID, id, f.RecordingID, store.AlbumChoice{ID: album})
	} else {
		res, err = s.db.SetSourcesRelease(r.Context(), u.ID, []int64{f.Source.ID}, album)
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "That album isn't there anymore. Search again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("album set by hand", "user", u.Name, "listen", id, "album", album, "one", one)
	s.fixDone(w, r, id, res.EditID, one, "album")
}

// fixCorrect links the listens to what typed text reads as.
func (s *Server) fixCorrect(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, f, ok := s.fixTarget(w, r, u)
	if !ok {
		return
	}
	one := f.Fixed || scopeOne(r)
	var listen int64
	if one {
		listen = id
	}
	t := resolve.Text{Artist: strings.TrimSpace(r.PostFormValue("artist")), Title: strings.TrimSpace(r.PostFormValue("title")),
		Album: strings.TrimSpace(r.PostFormValue("album_text"))}
	res, err := resolve.LinkAs(r.Context(), s.db, u.ID, f.Source.ID, listen, t)
	switch {
	case errors.Is(err, resolve.ErrNothingToGoOn):
		s.fixError(w, r, u, id, "Type an artist and a title.")
		return
	case errors.Is(err, resolve.ErrAmbiguous):
		s.fixError(w, r, u, id, "That could be more than one song. Pick the right one under Song.")
		return
	case errors.Is(err, store.ErrBuried):
		s.fixError(w, r, u, id, "That song is in the graveyard. Bring it back first.")
		return
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "Something changed at the same time. Try again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("corrected by hand", "user", u.Name, "listen", id, "one", one)
	s.fixDone(w, r, id, res.EditID, one, "linked")
}

// fixSame does for every listen with the text what was done for this one
// alone, and then offers rules for similar listens.
func (s *Server) fixSame(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _, ok := s.fixTarget(w, r, u)
	if !ok {
		return
	}
	rec, res, err := s.db.LinkTextOfListen(r.Context(), u.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale), errors.Is(err, store.ErrBuried):
		s.fixError(w, r, u, id, "Something changed at the same time. Try again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("one listen's fix applied to its text", "user", u.Name, "listen", id)
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?done=%d&remember=%d#remember", id, res.EditID, rec), http.StatusSeeOther)
}

// fixFollow makes a listen linked on its own follow its text again.
func (s *Server) fixFollow(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _, ok := s.fixTarget(w, r, u)
	if !ok {
		return
	}
	editID, err := s.db.UnfixListen(r.Context(), u.ID, id)
	if err != nil && !errors.Is(err, store.ErrStale) {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("listen follows its text again", "user", u.Name, "listen", id)
	s.fixDone(w, r, id, editID, false, "linked")
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
	one := f.Fixed || scopeOne(r)
	var listen int64
	if one {
		listen = id
	}
	_, res, err := resolve.NewSong(r.Context(), s.db, u.ID, f.Source.ID, listen)
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
	s.fixDone(w, r, id, res.EditID, one, "linked")
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
