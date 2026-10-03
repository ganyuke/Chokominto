package web

import (
	"context"
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

// The Scrobbles view of a song or album: the received text behind it, by
// version, with what each text is linked to and how, and moving checked
// rows to another version, song or album. The Fixed links page in Settings
// shows the same rows for every text linked by hand.

// linkedBy says how received text got its link.
type linkedBy struct {
	Text string
	Href string // the rule, when a rule did it
}

// sentRow is one received text.
type sentRow struct {
	ID      int64
	Artist  string
	Title   string
	Album   string
	Listens int
	Song    name // what it's linked to, on album and fixed-link pages
	Version string
	OnAlbum *name
	By      linkedBy
	Fix     string
	Checked bool
}

// aloneRow is a listen linked on its own, apart from its text.
type aloneRow struct {
	When    string
	Artist  string
	Title   string
	OnAlbum *name
	Fix     string
}

// sentGroup is the received text of one version of a song, or all of an
// album's.
type sentGroup struct {
	Anchor  string
	Version string
	Artists []name
	Listens int
	Rows    []sentRow
	Alone   []aloneRow
}

type albumResult struct {
	ID      int64
	Album   name
	Context string
}

type scrobblesPage struct {
	Page
	Kind       string // song or release
	Name       name
	Tabs       []periodTab
	Path       string // /song/1/scrobbles
	Groups     []sentGroup
	Texts      int
	Versions   []option // the song's versions, to move rows to
	Query      string
	Results    []recordingRow
	AlbumQuery string
	Albums     []albumResult
	Rules      []resolve.OwnRule
}

// linkedByText says how a text was linked, with the date for links made by
// hand.
func linkedByText(s store.LinkedSource, rules map[int64]string, loc *time.Location) linkedBy {
	switch s.SetBy {
	case "":
		return linkedBy{Text: "Not linked"}
	case "auto":
		return linkedBy{Text: "Read automatically"}
	case "rule":
		text := rules[s.RuleID]
		if text == "" {
			text = "a rule that's gone"
		}
		return linkedBy{Text: "Rule: " + text, Href: "/settings#rules"}
	}
	how := map[string]string{"link": "By hand", "merge": "Merge", "review": "Review", "edit": "Edit"}[s.SetBy]
	if how == "" {
		how = "By hand"
	}
	if s.Agent != "" {
		how += " by " + s.Agent
	}
	return linkedBy{Text: how + ", " + time.Unix(s.SetAt, 0).In(loc).Format("2 Jan 2006")}
}

// sentRows turns received text into table rows. checked are the rows
// ticked before a search.
func (s *Server) sentRows(ctx context.Context, u *store.User, srcs []store.LinkedSource, checked []int64) ([]sentRow, error) {
	rules, err := resolve.RuleTexts(ctx, s.db, u.ID)
	if err != nil {
		return nil, err
	}
	var recs []int64
	for _, src := range srcs {
		if src.RecordingID.Valid {
			recs = append(recs, src.RecordingID.Int64)
		}
	}
	infos, err := s.db.RecordingInfos(ctx, recs)
	if err != nil {
		return nil, err
	}
	loc := location(*u)
	out := make([]sentRow, len(srcs))
	for i, src := range srcs {
		row := sentRow{ID: src.ID, Artist: src.Artist, Title: src.Title, Album: src.Album, Listens: src.Listens,
			By: linkedByText(src, rules, loc), Checked: containsID(checked, src.ID)}
		if src.LatestListen != 0 {
			row.Fix = fmt.Sprintf("/listen/%d/fix", src.LatestListen)
		}
		if info, ok := infos[src.RecordingID.Int64]; ok {
			row.Song, row.Version = songName(info.Song), info.Version
		}
		if src.OnAlbum.ID != 0 {
			n := albumName(src.OnAlbum)
			row.OnAlbum = &n
		}
		out[i] = row
	}
	return out, nil
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// queryIDs reads ids from the query string, the rows ticked before a
// search.
func queryIDs(q url.Values, key string) []int64 {
	var out []int64
	for _, v := range q[key] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// destinations fills in the searches for a song and an album to move
// checked rows to.
func (s *Server) destinations(ctx context.Context, u *store.User, q url.Values, p *scrobblesPage) error {
	if p.Query = strings.TrimSpace(q.Get("q")); p.Query != "" {
		found, err := s.db.SearchRecordings(ctx, u.ID, p.Query, fixResults)
		if err != nil {
			return err
		}
		p.Results = recordingRows(found)
	}
	if p.AlbumQuery = strings.TrimSpace(q.Get("album")); p.AlbumQuery != "" {
		found, err := s.db.SearchItems(ctx, u.ID, "release", p.AlbumQuery, fixResults)
		if err != nil {
			return err
		}
		for _, f := range found {
			d, err := s.db.Album(ctx, f.ID)
			if err != nil {
				return err
			}
			p.Albums = append(p.Albums, albumResult{f.ID, albumName(f), d.Context})
		}
	}
	return nil
}

func (s *Server) scrobblesView(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		e, ok := s.entity(w, r, *u, kind, pagePaths[kind])
		if !ok {
			return
		}
		ctx := r.Context()
		p := scrobblesPage{Page: Page{Title: "Scrobbles of " + e.Name, User: u, Error: editErrors[r.URL.Query().Get("err")]},
			Kind: kind, Name: name{Name: e.Name, Other: e.OtherNames}, Tabs: viewTabs(kind, e.ID, "scrobbles"),
			Path: fmt.Sprintf("%s/%d/scrobbles", pagePaths[kind], e.ID)}
		s.doneNotice(r, u, &p.Page)
		err := s.loadScrobbles(ctx, u, kind, e.ID, queryIDs(r.URL.Query(), "s"), &p)
		if err == nil {
			err = s.destinations(ctx, u, r.URL.Query(), &p)
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.render(w, http.StatusOK, "scrobbles", p)
	}
}

func (s *Server) loadScrobbles(ctx context.Context, u *store.User, kind string, id int64, checked []int64, p *scrobblesPage) error {
	srcs, err := s.db.SourcesOf(ctx, u.ID, kind, id)
	if err != nil {
		return err
	}
	rows, err := s.sentRows(ctx, u, srcs, checked)
	if err != nil {
		return err
	}
	p.Texts = len(rows)
	alone, err := s.db.FixedListens(ctx, u.ID, kind, id)
	if err != nil {
		return err
	}
	aloneRows, err := s.listenRows(ctx, alone, location(*u), true)
	if err != nil {
		return err
	}
	aloneOf := func(i int) aloneRow {
		l, row := alone[i], aloneRows[i]
		return aloneRow{When: row.Date + ", " + row.Time, Artist: l.Artist, Title: l.Title, OnAlbum: row.Album, Fix: row.Fix}
	}
	if kind == "release" {
		g := sentGroup{Anchor: "received", Rows: rows}
		for i := range alone {
			g.Alone = append(g.Alone, aloneOf(i))
		}
		p.Groups = []sentGroup{g}
		return nil
	}
	// A song: one group per version, most listened first.
	recs, err := s.db.SongRecordings(ctx, u.ID, id)
	if err != nil {
		return err
	}
	for _, rc := range recs {
		g := sentGroup{Anchor: fmt.Sprintf("v%d", rc.RecordingID), Version: rc.Version, Artists: artistNames(rc.Artists), Listens: rc.Listens}
		for i, src := range srcs {
			if src.RecordingID.Int64 == rc.RecordingID {
				g.Rows = append(g.Rows, rows[i])
			}
		}
		for i, l := range alone {
			if l.RecordingID == rc.RecordingID {
				g.Alone = append(g.Alone, aloneOf(i))
			}
		}
		if len(g.Rows)+len(g.Alone) > 0 {
			p.Groups = append(p.Groups, g)
		}
		p.Versions = append(p.Versions, option{fmt.Sprint(rc.RecordingID), versionLabel(rc.Version, rc.Artists)})
	}
	if len(p.Versions) < 2 {
		p.Versions = nil
	}
	_, own, err := resolve.ReadingSettings(ctx, s.db, u.ID)
	if err != nil {
		return err
	}
	for _, o := range own {
		for _, rc := range recs {
			if o.RecordingID == rc.RecordingID {
				p.Rules = append(p.Rules, o)
			}
		}
	}
	return nil
}

// versionLabel names a version in a list of a song's versions: "TV size,
// by LiSA", or "Main version, by LiSA".
func versionLabel(version string, artists []store.Ref) string {
	if version == "" {
		version = "Main version"
	}
	if len(artists) == 0 {
		return version
	}
	return version + ", by " + store.JoinNames(artists)
}

// moveSources moves the checked received texts where the form says: to a
// version or song ("version", "song:<recording>"), an album
// ("album:<album>") or no album ("no-album").
func (s *Server) moveSources(ctx context.Context, r *http.Request, u *store.User) (int64, error) {
	sources := formIDs(r, "s")
	if len(sources) == 0 {
		return 0, errPickRows
	}
	do, target, _ := strings.Cut(r.PostFormValue("do"), ":")
	id, _ := strconv.ParseInt(target, 10, 64)
	var res store.LinkResult
	var err error
	switch do {
	case "version":
		res, err = s.db.LinkSources(ctx, u.ID, sources, formInt(r, "version"))
	case "song":
		res, err = s.db.LinkSources(ctx, u.ID, sources, id)
	case "album":
		if id == 0 {
			return 0, store.ErrValue
		}
		res, err = s.db.SetSourcesRelease(ctx, u.ID, sources, id)
	case "no-album":
		res, err = s.db.SetSourcesRelease(ctx, u.ID, sources, 0)
	case "reread":
		return resolve.Reread(ctx, s.db, u.ID, sources)
	default:
		return 0, store.ErrValue
	}
	return res.EditID, err
}

var errPickRows = errors.New("check some rows first")

func (s *Server) scrobblesMove(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		path := fmt.Sprintf("%s/%d/scrobbles", pagePaths[kind], id)
		editID, err := s.moveSources(r.Context(), r, u)
		if err != nil {
			code := editErrorCode(err)
			if code == "" {
				s.serverError(w, r, err)
				return
			}
			http.Redirect(w, r, path+"?err="+code, http.StatusSeeOther)
			return
		}
		s.log.Info("scrobbles moved", "user", u.Name, "kind", kind, "id", id, "do", r.PostFormValue("do"))
		if editID == 0 {
			http.Redirect(w, r, path, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s?done=%d", path, editID), http.StatusSeeOther)
	}
}

// Fixed links: every received text linked by hand, in Settings.

type linksPage struct {
	scrobblesPage
	Find  string
	Total int
	Prev  string
	Next  string
}

const linksPageSize = 50

func (s *Server) fixedLinks(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	q := r.URL.Query()
	n := pageNum(r)
	p := linksPage{Find: strings.TrimSpace(q.Get("find"))}
	p.Page = Page{Title: "Fixed links", Nav: "settings", User: u, Error: editErrors[q.Get("err")]}
	p.Path = "/settings/links"
	s.doneNotice(r, u, &p.Page)
	srcs, total, err := s.db.FixedSources(ctx, u.ID, p.Find, linksPageSize, (n-1)*linksPageSize)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Total = total
	rows, err := s.sentRows(ctx, u, srcs, queryIDs(q, "s"))
	if err == nil {
		err = s.destinations(ctx, u, q, &p.scrobblesPage)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(rows) > 0 {
		p.Groups = []sentGroup{{Rows: rows}}
	}
	if n > 1 {
		p.Prev = pageLink(r, n-1)
	}
	if n*linksPageSize < total {
		p.Next = pageLink(r, n+1)
	}
	s.render(w, http.StatusOK, "links", p)
}

func (s *Server) fixedLinksMove(w http.ResponseWriter, r *http.Request, u *store.User) {
	editID, err := s.moveSources(r.Context(), r, u)
	if err != nil {
		code := editErrorCode(err)
		if code == "" {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, "/settings/links?err="+code, http.StatusSeeOther)
		return
	}
	s.log.Info("fixed links changed", "user", u.Name, "do", r.PostFormValue("do"))
	if editID == 0 {
		http.Redirect(w, r, withNotice("/settings/links", "reread-none"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/settings/links?done=%d", editID), http.StatusSeeOther)
}
