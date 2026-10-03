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
	Scope       string        // "one" or "all", as sent with every form
	Later       string        // "no", "text", "album" or "key", as sent with every form
	Now         []scopeOption // which listens sent so far, the current choice marked
	Laters      []scopeOption // what later listens do, the current choice marked
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

// fixScope reads the two choices a Fix form carries: which of the listens
// sent so far a change moves, and what later listens do ("no" for nothing,
// "text" for the same text, "album" and "key" for a rule that is wider).
// They are independent, and every combination is allowed.
func fixScope(r *http.Request) (sc store.LinkScope, later string) {
	sc.All = r.FormValue("scope") != "one"
	switch later = r.FormValue("later"); later {
	case "no", "album", "key":
	default:
		later = "text"
	}
	sc.Later = later != "no"
	return sc, later
}

// capital starts a sentence with s.
func capital(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

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
		Alone: f.Fixed}
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

	// Scope is two separate choices, worded the same however many listens
	// the text has: which listens sent so far, and what later ones do.
	sc, later := fixScope(r)
	p.One, p.Scope, p.Later = !sc.All, "all", later
	if p.One {
		p.Scope = "one"
	}
	all := p.Same
	if p.Alone {
		all++ // this one too, though it stopped following the text
	}
	moving, what := 1, "this listen"
	p.Move = "Move this listen here"
	if sc.All {
		moving, what = all, "every listen sent with this text so far"
		p.Move = "Move " + listens(all) + " here"
	}
	link := func(scope, later string) string {
		return fmt.Sprintf("%s/fix?scope=%s&later=%s#scope", p.Action, scope, later)
	}
	p.Now = []scopeOption{
		{link("one", later), "Only this listen", "The one from " + p.When + ".", p.One},
		{link("all", later), "Every listen sent with this text", listens(all) + " so far.", !p.One},
	}
	p.Laters = []scopeOption{
		{link(p.Scope, "no"), "Leave future scrobbles alone", "No change to how future scrobbles are linked.", later == "no"},
		{link(p.Scope, "text"), "Match exact title, artist and album in future scrobbles", "", later == "text"},
	}
	if strings.TrimSpace(f.Artist) != "" && strings.TrimSpace(f.Title) != "" {
		p.Laters = append(p.Laters,
			scopeOption{link(p.Scope, "album"), "Match exact title and artist in future scrobbles (disregarding album)",
				"Saved as a rule. Existing scrobbles are not moved.", later == "album"},
			scopeOption{link(p.Scope, "key"), "Match roughly the title and artist in future scrobbles (disregarding spacing, capitalization and album)",
				"Saved as a rule. Existing scrobbles are not moved. Full-width letters are treated as the same letters.", later == "key"})
	}
	p.ScopeHelp = "Changes below are applied to " + what + map[string]string{
		"no":    ". Future scrobbles are left alone.",
		"text":  ", and to future scrobbles with the exact title, artist and album.",
		"album": ", and to future scrobbles with the exact title and artist, disregarding album.",
		"key":   ", and to future scrobbles with roughly the title and artist, disregarding spacing, capitalization and album.",
	}[later]
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
		p.SongHelp = fmt.Sprintf("%s is moved to the song and version picked. ", capital(what))
		if left > 0 {
			p.SongHelp += fmt.Sprintf("The other %s of %s are not moved, and its names are not changed. ", listens(left), current)
		} else {
			p.SongHelp += fmt.Sprintf("No listens are left on %s after that. Its names are not changed. ", current)
		}
		p.SongHelp += "Nothing is merged. To join two songs completely, use Merge on the song's Edit tab."
		p.AlbumHelp = fmt.Sprintf("%s is put on the album picked. The song is not changed.", capital(what))
		if later == "album" || later == "key" {
			p.AlbumHelp += " No rule is saved for a change of album, because a rule is a link to a song."
		}
		p.NewSongHelp = fmt.Sprintf("%s is taken out of %s and linked to a new song with the same name and artists. For two different songs with the same name.", capital(what), current)
	} else {
		p.SongHelp = fmt.Sprintf("%s is linked to the song and version picked.", capital(what))
		p.NewSongHelp = fmt.Sprintf("A new song is made from the received artist and title, and %s is linked to it.", what)
	}
	p.CorrectHelp = fmt.Sprintf("Type what should have been sent, like the real artist in place of a channel name, or a title with its version in brackets. "+
		"%s is linked to the song, version and album that the typed text is read as. Any of them not there yet is created. The received text itself is not changed.",
		capital(what))
	p.DeleteHelp = "Only this listen, from " + p.When + ", is deleted."
	if p.Sent > 1 {
		p.DeleteHelp += numberPrinter.Sprintf(" The other %d sent with the same text are not deleted.", p.Sent-1)
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

	// After a change that later listens follow, offer wider rules.
	target, _ := strconv.ParseInt(r.URL.Query().Get("remember"), 10, 64)
	if target > 0 && sc.Later && f.RecordingID == target && f.Source.LinkedByOwner {
		offers, err := resolve.Offers(ctx, s.db, u.ID, f.Source.ID, target)
		if err != nil {
			return p, err
		}
		p.Target = target
		for _, o := range offers {
			// A rule saved with the move isn't offered again, nor the
			// narrower one it covers.
			if p.Saved != "" && (o.Kind == later || (later == "key" && o.Kind == "album")) {
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
	_, err := s.db.FixListen(r.Context(), u.ID, id)
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
	sc, _ := fixScope(r)
	res, err := s.db.LinkScoped(r.Context(), u.ID, id, store.LinkTarget{RecordingID: rec, Album: store.AlbumChoice{Keep: true}}, sc)
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
	s.log.Info("linked by hand", "user", u.Name, "listen", id, "recording", rec, "all", sc.All, "later", sc.Later)
	s.fixMoved(w, r, u, id, res.EditID)
}

// fixMoved goes back to the Fix page after listens were moved to a song.
// When later listens follow, it saves the rule the scope asks for and
// offers the others. It's the same after Version, Song, Correction and New
// song.
func (s *Server) fixMoved(w http.ResponseWriter, r *http.Request, u *store.User, id, editID int64) {
	sc, later := fixScope(r)
	if !sc.Later || editID == 0 {
		s.fixDone(w, r, id, editID, "linked")
		return
	}
	f, err := s.db.FixListen(r.Context(), u.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rec := f.RecordingID
	q := fixQuery(r)
	q.Set("done", fmt.Sprint(editID))
	q.Set("remember", fmt.Sprint(rec))
	if later == "album" || later == "key" {
		q.Set("saved", "none")
		offers, err := resolve.Offers(r.Context(), s.db, u.ID, f.Source.ID, rec)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, o := range offers {
			if o.Kind != later {
				continue
			}
			ruleEdit, err := resolve.SaveOffer(r.Context(), s.db, u.ID, o, offerText(o, f.Artist, f.Title), true)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			q.Set("saved", fmt.Sprint(ruleEdit))
			s.log.Info("rule saved with a move", "user", u.Name, "listen", id, "kind", later)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/listen/%d/fix?%s#remember", id, q.Encode()), http.StatusSeeOther)
}

// fixQuery carries the scope a form was sent with back to the page.
func fixQuery(r *http.Request) url.Values {
	sc, later := fixScope(r)
	q := url.Values{"later": {later}, "scope": {"all"}}
	if !sc.All {
		q.Set("scope", "one")
	}
	return q
}

// fixDone goes back to the Fix page after a change, in the same scope, to
// the section the button was in.
func (s *Server) fixDone(w http.ResponseWriter, r *http.Request, id, editID int64, section string) {
	q := fixQuery(r)
	if editID != 0 {
		q.Set("done", fmt.Sprint(editID))
	}
	path := fmt.Sprintf("/listen/%d/fix?%s", id, q.Encode())
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
	sc, _ := fixScope(r)
	res, err := s.db.LinkScoped(r.Context(), u.ID, id, store.LinkTarget{RecordingID: f.RecordingID, Album: store.AlbumChoice{ID: album}}, sc)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStale):
		s.fixError(w, r, u, id, "That album isn't there anymore. Search again.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.log.Info("album set by hand", "user", u.Name, "listen", id, "album", album, "all", sc.All, "later", sc.Later)
	s.fixDone(w, r, id, res.EditID, "album")
}

// fixCorrect links the listens to what typed text reads as.
func (s *Server) fixCorrect(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _, ok := s.fixTarget(w, r, u)
	if !ok {
		return
	}
	sc, _ := fixScope(r)
	t := resolve.Text{Artist: strings.TrimSpace(r.PostFormValue("artist")), Title: strings.TrimSpace(r.PostFormValue("title")),
		Album: strings.TrimSpace(r.PostFormValue("album_text"))}
	res, err := resolve.LinkAs(r.Context(), s.db, u.ID, id, t, sc)
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
	s.log.Info("corrected by hand", "user", u.Name, "listen", id, "all", sc.All, "later", sc.Later)
	s.fixMoved(w, r, u, id, res.EditID)
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
	s.fixDone(w, r, id, editID, "linked")
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
	sc, _ := fixScope(r)
	_, res, err := resolve.NewSong(r.Context(), s.db, u.ID, f.Source.ID, id, sc)
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
	s.fixMoved(w, r, u, id, res.EditID)
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
	editID, err := resolve.SaveOffer(r.Context(), s.db, u.ID, *offer, offerText(*offer, f.Artist, f.Title), false)
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
