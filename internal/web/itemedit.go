package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chokominto/internal/store"
)

// Editing artists, songs and albums on their own pages, logged in. Every
// form posts to /<page>/<id>/edit with "do" saying what to change, and
// comes back to the page with the change and its Undo.

type creditOption struct {
	Value string // "<recording>:<artist>" on songs, "<artist>" on albums
	Label string
}

type overrideRow struct {
	ScopeID int64
	On      string // the recording, on song pages
	From    store.Ref
	To      store.Ref
}

type editData struct {
	Kind         string // artist, song or release
	Path         string // /artist/1
	Noun         string // artist, song or album
	Aliases      []store.Alias
	InLists      bool          // one of the names shows under it in lists
	Labels       []store.Label // on it
	OtherLabels  []store.Label // not on it
	AnyLabels    bool
	ArtistKind   string
	CountsFor    []store.ArtistLink
	Members      []store.Ref
	Recordings   []store.SongRecording
	Credits      []creditOption
	Album        store.AlbumDetails
	Overrides    []overrideRow
	MergeQuery   string
	MergeResults []store.Ref
	Picture      *pictureData // artists and albums

	RecordingCredits []recordingCredits     // songs
	Buried           bool                   // songs: in the graveyard
	AlbumArtists     []store.Ref            // albums
	Tracks           []store.RecordingCount // albums
	Related          []store.AlbumCount     // albums: others holding the same songs
	SongAlbums       []songAlbum            // songs: the albums the song is on
	MergeMoves       string                 // what a merge moves, in words
	Usage            *store.Usage           // artists and albums, for deleting
	InUse            string                 // what still uses it, when something does
}

// songAlbum is an album a song is on, on the song's Edit tab.
type songAlbum struct {
	store.AlbumCount
	Art string
}

type recordingCredits struct {
	ID      int64
	Label   string
	Credits []store.CreditRef
}

type option struct{ Value, Label string }

var albumKindNames = []option{{"album", "Album"}, {"single", "Single"}, {"ep", "EP"}, {"soundtrack", "Soundtrack"}, {"video", "Video"}, {"other", "Other"}}

var pagePaths = map[string]string{"artist": "/artist", "song": "/song", "release": "/album"}
var nouns = map[string]string{"artist": "artist", "song": "song", "release": "album"}

// Errors after an edit, keyed so no text comes from the URL.
var editErrors = map[string]string{
	"name":          "Type a name first.",
	"last-name":     "That's the only name, so it stays. Add another first.",
	"loop":          "That would go round in a circle, so it wasn't saved.",
	"no-artist":     "There's no artist by that name. Check the spelling, or scrobble something by them first.",
	"value":         "That value isn't allowed.",
	"gone":          "That isn't there anymore. The page has been reloaded.",
	"stale":         "Something changed at the same time. Try again.",
	"split":         "Check some of the recordings to split off, not all of them.",
	"merge":         "That's the same one. Pick another to merge into.",
	"picture-none":  "Choose a picture file first.",
	"picture-small": "That picture is too small. Pick one at least 64 pixels on each side.",
	"picture-big":   "That picture is too big. Pick one under 10 MB and 8,000 pixels on each side.",
	"picture-bad":   "That file isn't a picture Chokominto can read. Try a JPEG, PNG, WebP or GIF.",
	"picture-fetch": "That picture couldn't be downloaded. Try another one, or look again later.",
	"in-use":        "Something still uses this, so nothing was deleted. Fix that first.",
	"buried":        "That song is in the graveyard. Bring it back first.",
	"no-main":       "A recording needs at least one main artist. Credit the right one before taking this one off.",
	"pick-tracks":   "Check some songs first.",
	"pick-rows":     "Check some rows first.",
	"pick-albums":   "Check the albums to merge, and pick which one to keep.",
}

func (s *Server) loadEdit(ctx context.Context, r *http.Request, u *store.User, kind string, id int64) (*editData, error) {
	if u == nil {
		return nil, nil
	}
	d := &editData{Kind: kind, Path: fmt.Sprintf("%s/%d", pagePaths[kind], id), Noun: nouns[kind]}
	var err error
	if d.Aliases, err = s.db.Aliases(ctx, kind, id); err != nil {
		return nil, err
	}
	for _, a := range d.Aliases {
		d.InLists = d.InLists || a.InLists
	}
	if d.Labels, err = s.db.ItemLabels(ctx, kind, id); err != nil {
		return nil, err
	}
	all, err := s.db.Labels(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	d.AnyLabels = len(all) > 0
	on := map[int64]bool{}
	for _, l := range d.Labels {
		on[l.ID] = true
	}
	for _, l := range all {
		if !on[l.ID] {
			d.OtherLabels = append(d.OtherLabels, l.Label)
		}
	}

	switch kind {
	case "artist":
		if d.ArtistKind, err = s.db.ArtistKind(ctx, id); err != nil {
			return nil, err
		}
		if d.CountsFor, err = s.db.CountsForLinks(ctx, id); err != nil {
			return nil, err
		}
		rel, err := s.db.ArtistRelations(ctx, id)
		if err != nil {
			return nil, err
		}
		d.Members = rel.Members
	case "song":
		if d.Recordings, err = s.db.SongRecordingDetails(ctx, u.ID, id); err != nil {
			return nil, err
		}
		var scopes []int64
		labels := map[int64]string{}
		for _, rec := range d.Recordings {
			scopes = append(scopes, rec.ID)
			labels[rec.ID] = recordingLabel(rec)
			for _, a := range rec.Artists {
				label := a.Name
				if len(d.Recordings) > 1 {
					label += ", on the recording by " + labels[rec.ID]
				}
				d.Credits = append(d.Credits, creditOption{fmt.Sprintf("%d:%d", rec.ID, a.ID), label})
			}
		}
		if len(d.Recordings) == 1 {
			labels[d.Recordings[0].ID] = "" // no need to say which
		}
		os, err := s.db.Overrides(ctx, "recording", scopes)
		if err != nil {
			return nil, err
		}
		for _, o := range os {
			d.Overrides = append(d.Overrides, overrideRow{o.ScopeID, labels[o.ScopeID], o.From, o.To})
		}
		for _, rec := range d.Recordings {
			cs, err := s.db.RecordingCredits(ctx, rec.ID)
			if err != nil {
				return nil, err
			}
			d.RecordingCredits = append(d.RecordingCredits, recordingCredits{rec.ID, "The recording by " + recordingLabel(rec) + ":", cs})
		}
		if d.Buried, err = s.db.SongBuried(ctx, id); err != nil {
			return nil, err
		}
		albums, err := s.db.SongAlbums(ctx, u.ID, id)
		if err != nil {
			return nil, err
		}
		var albumIDs []int64
		for _, a := range albums {
			albumIDs = append(albumIDs, a.ID)
		}
		arts, err := s.db.ArtworkFor(ctx, "release", albumIDs)
		if err != nil {
			return nil, err
		}
		for _, a := range albums {
			d.SongAlbums = append(d.SongAlbums, songAlbum{a, thumbOf(arts, a.ID)})
		}
	case "release":
		if d.Album, err = s.db.Album(ctx, id); err != nil {
			return nil, err
		}
		tracks, err := s.db.AlbumTracks(ctx, u.ID, id)
		if err != nil {
			return nil, err
		}
		seen := map[int64]bool{}
		for _, t := range tracks {
			credited, err := s.db.CreditedArtists(ctx, t.RecordingID)
			if err != nil {
				return nil, err
			}
			for _, a := range credited {
				if !seen[a.ID] {
					seen[a.ID] = true
					d.Credits = append(d.Credits, creditOption{fmt.Sprint(a.ID), a.Name})
				}
			}
		}
		os, err := s.db.Overrides(ctx, "release", []int64{id})
		if err != nil {
			return nil, err
		}
		for _, o := range os {
			d.Overrides = append(d.Overrides, overrideRow{o.ScopeID, "", o.From, o.To})
		}
		d.Tracks = tracks
		if d.AlbumArtists, err = s.db.ReleaseArtists(ctx, id); err != nil {
			return nil, err
		}
		if d.Related, err = s.db.RelatedReleases(ctx, u.ID, id, 20); err != nil {
			return nil, err
		}
	}
	if kind == "artist" || kind == "release" {
		u, err := s.db.ItemUsage(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		d.Usage, d.InUse = &u, inUse(kind, u)
	}

	if d.Picture, err = s.loadPicture(ctx, u, kind, id); err != nil {
		return nil, err
	}
	st, err := s.db.EntityStats(ctx, u.ID, kind, id)
	if err != nil {
		return nil, err
	}
	d.MergeMoves = mergeMoves(d, st.Listens)
	if q := r.URL.Query().Get("merge"); q != "" {
		d.MergeQuery = q
		found, err := s.db.SearchItems(ctx, u.ID, kind, q, 20)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			if f.ID != id {
				d.MergeResults = append(d.MergeResults, f)
			}
		}
	}
	return d, nil
}

// mergeMoves says what merging this item into another moves there: "56
// listens, 3 versions and 4 names".
func mergeMoves(d *editData, n int) string {
	count := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return numberPrinter.Sprintf("%d %s", n, many)
	}
	parts := []string{count(n, "listen", "listens")}
	switch d.Kind {
	case "song":
		parts = append(parts, count(len(d.Recordings), "version", "versions"))
	case "release":
		parts = append(parts, count(len(d.Tracks), "song", "songs"))
	}
	parts = append(parts, count(len(d.Aliases), "name", "names"))
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// inUse says what still uses an artist or album, or "" when nothing does.
func inUse(kind string, u store.Usage) string {
	count := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return numberPrinter.Sprintf("%d %s", n, many)
	}
	and := func(parts []string) string {
		if len(parts) < 2 {
			return strings.Join(parts, "")
		}
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	var sentences, parts []string
	if kind == "artist" {
		if u.Songs > 0 {
			parts = append(parts, count(u.Songs, "song", "songs"))
		}
		if u.Albums > 0 {
			parts = append(parts, count(u.Albums, "album", "albums"))
		}
		if len(parts) > 0 {
			sentences = append(sentences, "Still credited on "+and(parts)+".")
		}
		if u.Links > 0 {
			sentences = append(sentences, "Still linked to other artists, as a member, a group, \"also counts for\" or in \"who gets credit\".")
		}
	} else {
		if u.Songs > 0 {
			parts = append(parts, count(u.Songs, "song", "songs"))
		}
		if u.Listens > 0 {
			parts = append(parts, count(u.Listens, "listen", "listens"))
		}
		if u.Rules > 0 {
			parts = append(parts, count(u.Rules, "remembered link", "remembered links"))
		}
		if len(parts) > 0 {
			sentences = append(sentences, "It still has "+and(parts)+" on it.")
		}
	}
	if u.MergedInto {
		sentences = append(sentences, "Something else was merged into this "+nouns[kind]+".")
	}
	return strings.Join(sentences, " ")
}

func recordingLabel(r store.SongRecording) string {
	var names []string
	for _, a := range r.Artists {
		names = append(names, a.Name)
	}
	l := strings.Join(names, ", ")
	if r.Version != "" {
		l += " (" + r.Version + ")"
	}
	return l
}

// namesForm reads the All names table: each name's kind, whether it's
// listed on the page, and which one shows in lists.
func namesForm(r *http.Request) store.NamesChange {
	r.ParseForm()
	var ch store.NamesChange
	for i, v := range r.PostForm["alias"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		// The first name is the item's own, always shown.
		ch.Names = append(ch.Names, store.NameChoice{AliasID: id, Lang: r.PostFormValue(fmt.Sprintf("lang-%d", id)),
			Shown: i == 0 || r.PostFormValue(fmt.Sprintf("shown-%d", id)) == "1"})
	}
	if v := r.PostFormValue("in-lists"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		ch.InLists = &id
	}
	return ch
}

func formInt(r *http.Request, key string) int64 {
	id, _ := strconv.ParseInt(r.PostFormValue(key), 10, 64)
	return id
}

// itemEdit handles every edit form on an item page.
func (s *Server) itemEdit(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		ctx := r.Context()
		// Edits come back to the Edit view. A merge goes to the item merged
		// into.
		path := fmt.Sprintf("%s/%d/edit", pagePaths[kind], id)
		var editID int64
		switch do := r.PostFormValue("do"); do {
		case "rename":
			editID, err = s.db.Rename(ctx, u.ID, kind, id, r.PostFormValue("name"))
		case "add-name":
			editID, err = s.db.AddName(ctx, u.ID, kind, id, r.PostFormValue("name"))
		case "remove-name":
			editID, err = s.db.RemoveName(ctx, u.ID, kind, id, formInt(r, "alias"))
		case "names":
			editID, err = s.db.SetNames(ctx, u.ID, kind, id, namesForm(r))
		case "pin-name":
			editID, err = s.db.PinName(ctx, u.ID, kind, id, formInt(r, "alias"))
		case "label", "unlabel":
			editID, err = s.db.SetLabel(ctx, u.ID, kind, id, formInt(r, "label"), do == "label")
		case "picture-choose":
			editID, err = s.choosePicture(ctx, u, kind, id, formInt(r, "candidate"))
		case "picture-remove":
			err = s.db.Write(ctx, func(tx *sql.Tx) error {
				var err error
				editID, err = store.ClearArtworkTx(ctx, tx, u.ID, kind, id)
				return err
			})
		case "picture-look":
			err = s.lookAgain(ctx, kind, id)
		case "merge":
			editID, err = s.db.Merge(ctx, u.ID, kind, id, formInt(r, "into"))
			if err == nil {
				path = fmt.Sprintf("%s/%d", pagePaths[kind], formInt(r, "into"))
			}
		case "merge-here":
			// The other one comes here, and this page stays.
			editID, err = s.db.Merge(ctx, u.ID, kind, formInt(r, "from"), id)
		case "delete":
			// The page is gone after, so Changes shows what happened.
			editID, err = s.db.DeleteItem(ctx, u.ID, kind, id)
			path = "/changes"
		default:
			editID, err = s.kindEdit(ctx, r, u, kind, id, do)
		}
		if err != nil {
			code := editErrorCode(err)
			if code == "" {
				s.serverError(w, r, err)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("%s/%d/edit?err=%s", pagePaths[kind], id, code), http.StatusSeeOther)
			return
		}
		s.log.Info("item edited", "user", u.Name, "kind", kind, "id", id, "do", r.PostFormValue("do"))
		// The graveyard in Review sends its buttons here and goes back.
		fragment := ""
		if r.PostFormValue("do") == "picture-look" {
			fragment = "#picture" // where it says what was found
		}
		if r.PostFormValue("back") == "review" {
			path, fragment = "/review", "#graveyard"
		}
		if editID == 0 {
			http.Redirect(w, r, path+fragment, http.StatusSeeOther) // nothing changed
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s?done=%d%s", path, editID, fragment), http.StatusSeeOther)
	}
}

// kindEdit handles the edits only one kind of page has.
func (s *Server) kindEdit(ctx context.Context, r *http.Request, u *store.User, kind string, id int64, do string) (int64, error) {
	artist := func(field string) (int64, error) {
		a, err := s.db.FindArtist(ctx, u.ID, r.PostFormValue(field))
		return a.ID, err
	}
	switch {
	case kind == "artist" && do == "kind":
		return s.db.SetArtistKind(ctx, u.ID, id, r.PostFormValue("kind"))
	case kind == "artist" && (do == "counts-for" || do == "member"):
		to, err := artist("artist")
		if err != nil {
			return 0, err
		}
		table := map[string]string{"counts-for": "artist_counts_for", "member": "group_members"}[do]
		return s.db.SetArtistLink(ctx, u.ID, table, id, to, r.PostFormValue("note"), true)
	case kind == "artist" && (do == "remove-counts-for" || do == "remove-member"):
		table := map[string]string{"remove-counts-for": "artist_counts_for", "remove-member": "group_members"}[do]
		return s.db.SetArtistLink(ctx, u.ID, table, id, formInt(r, "artist"), "", false)
	case kind == "song" && do == "version":
		return s.db.SetVersion(ctx, u.ID, id, formInt(r, "recording"), r.PostFormValue("version"))
	case kind == "song" && (do == "own-row" || do == "pool"):
		return s.db.SetOwnRow(ctx, u.ID, id, formInt(r, "recording"), do == "own-row")
	case kind == "song" && do == "original":
		return s.db.SetOriginal(ctx, u.ID, id, formInt(r, "recording"))
	case kind == "song" && do == "split":
		r.ParseForm()
		var recs []int64
		for _, v := range r.PostForm["recording"] {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				recs = append(recs, n)
			}
		}
		_, editID, err := s.db.SplitSong(ctx, u.ID, id, recs)
		return editID, err
	case kind == "song" && (do == "credit" || do == "uncredit"):
		rec := formInt(r, "recording")
		current, err := s.db.RecordingCredits(ctx, rec)
		if err != nil {
			return 0, err
		}
		var credits []store.CreditChoice
		for _, c := range current {
			if do == "uncredit" && c.ID == formInt(r, "artist") && c.Role == r.PostFormValue("role") {
				continue
			}
			credits = append(credits, store.CreditChoice{ArtistID: c.ID, Role: c.Role})
		}
		if do == "credit" {
			c, err := s.creditChoice(ctx, u, r.PostFormValue("artist"))
			if err != nil {
				return 0, err
			}
			c.Role = r.PostFormValue("role")
			credits = append(credits, c)
		}
		return s.db.SetRecordingCredits(ctx, u.ID, id, rec, credits)
	case kind == "release" && (do == "credit-album" || do == "uncredit-album"):
		current, err := s.db.ReleaseArtists(ctx, id)
		if err != nil {
			return 0, err
		}
		var artists []store.CreditChoice
		for _, a := range current {
			if do == "uncredit-album" && a.ID == formInt(r, "artist") {
				continue
			}
			artists = append(artists, store.CreditChoice{ArtistID: a.ID})
		}
		if do == "credit-album" {
			c, err := s.creditChoice(ctx, u, r.PostFormValue("artist"))
			if err != nil {
				return 0, err
			}
			artists = append(artists, c)
		}
		return s.db.SetAlbumArtists(ctx, u.ID, id, artists)
	case kind == "release" && do == "take-off":
		recs := formIDs(r, "recording")
		if len(recs) == 0 {
			return 0, errPickTracks
		}
		return s.db.TakeOffAlbum(ctx, u.ID, id, recs)
	case kind == "song" && do == "merge-albums":
		albums, into := formIDs(r, "album"), formInt(r, "into")
		if len(albums) == 0 || into == 0 || (len(albums) == 1 && albums[0] == into) {
			return 0, errPickAlbums
		}
		return s.db.MergeMany(ctx, u.ID, "release", albums, into)
	case kind == "song" && do == "bury":
		return s.db.BurySong(ctx, u.ID, id)
	case kind == "song" && do == "unbury":
		return s.db.UnburySong(ctx, u.ID, id)
	case kind == "song" && do == "delete-buried":
		return s.db.DeleteBuriedListens(ctx, u.ID, id)
	case kind == "release" && do == "details":
		return s.db.SetAlbumDetails(ctx, u.ID, id, store.AlbumDetails{Kind: r.PostFormValue("kind"),
			Released: r.PostFormValue("released"), Context: r.PostFormValue("context")})
	case do == "override" || do == "remove-override":
		o := store.Override{Scope: "release", ScopeID: id}
		if kind == "song" {
			o.Scope = "recording"
		}
		if do == "override" {
			rec, from, ok := strings.Cut(r.PostFormValue("from"), ":")
			if kind == "song" {
				if !ok {
					return 0, store.ErrValue
				}
				o.ScopeID, _ = strconv.ParseInt(rec, 10, 64)
			} else {
				from = rec
			}
			o.From.ID, _ = strconv.ParseInt(from, 10, 64)
			to, err := artist("to")
			if err != nil {
				return 0, err
			}
			o.To.ID = to
		} else {
			if kind == "song" {
				o.ScopeID = formInt(r, "scope")
			}
			o.From.ID, o.To.ID = formInt(r, "from"), formInt(r, "to")
		}
		return s.db.SetOverride(ctx, u.ID, kind, id, o, do == "override")
	}
	return 0, store.ErrValue
}

func editErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrName):
		return "name"
	case errors.Is(err, store.ErrLastName):
		return "last-name"
	case errors.Is(err, store.ErrLoop):
		return "loop"
	case errors.Is(err, store.ErrNoArtist):
		return "no-artist"
	case errors.Is(err, store.ErrValue):
		return "value"
	case errors.Is(err, store.ErrSplitAll):
		return "split"
	case errors.Is(err, store.ErrMergeSelf):
		return "merge"
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrMergedAway):
		return "gone"
	case errors.Is(err, store.ErrStale):
		return "stale"
	case errors.Is(err, errPicture):
		return "picture-fetch"
	case errors.Is(err, store.ErrInUse):
		return "in-use"
	case errors.Is(err, store.ErrBuried):
		return "buried"
	case errors.Is(err, store.ErrNoMain):
		return "no-main"
	case errors.Is(err, errPickTracks):
		return "pick-tracks"
	case errors.Is(err, errPickRows):
		return "pick-rows"
	case errors.Is(err, errPickAlbums):
		return "pick-albums"
	}
	return ""
}

var errPickTracks = errors.New("check some songs first")
var errPickAlbums = errors.New("tick the albums to merge")

// creditChoice finds an artist to credit by name, or a new one to add
// when there's no artist by that name.
func (s *Server) creditChoice(ctx context.Context, u *store.User, name string) (store.CreditChoice, error) {
	a, err := s.db.FindArtist(ctx, u.ID, name)
	if errors.Is(err, store.ErrNoArtist) {
		return store.CreditChoice{NewArtist: name}, nil
	}
	return store.CreditChoice{ArtistID: a.ID}, err
}

// viewTabs are the Read and Edit tabs of an item page.
func viewTabs(kind string, id int64, current string) []periodTab {
	path := fmt.Sprintf("%s/%d", pagePaths[kind], id)
	tabs := []periodTab{{"Read", path, current == "read"}, {"Edit", path + "/edit", current == "edit"}}
	if kind != "artist" {
		tabs = append(tabs, periodTab{"Scrobbles", path + "/scrobbles", current == "scrobbles"})
	}
	return tabs
}

// editPage is an item's Edit view.
type editPage struct {
	Page
	Name name
	Tabs []periodTab
	Edit *editData
}

// editView shows an item's Edit view.
func (s *Server) editView(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		e, ok := s.entity(w, r, *u, kind, pagePaths[kind])
		if !ok {
			return
		}
		p := editPage{Page: Page{Title: "Edit " + e.Name, User: u, Error: editErrors[r.URL.Query().Get("err")]},
			Name: name{Name: e.Name, Other: e.OtherNames}, Tabs: viewTabs(kind, e.ID, "edit")}
		s.doneNotice(r, u, &p.Page)
		var err error
		if p.Edit, err = s.loadEdit(r.Context(), r, u, kind, e.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.render(w, http.StatusOK, "itemedit", p)
	}
}
