package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"chokominto/internal/period"
	"chokominto/internal/store"
)

const (
	rankPageSize = 50
	homeTopSize  = 10
)

// name is an entity as shown anywhere: a link, the primary name, and the
// other names in muted text.
type name struct {
	Href    string
	Name    string
	Other   string
	Version string // a recording's version, shown right after the name
}

func artistName(r store.Ref) name {
	return name{fmt.Sprintf("/artist/%d", r.ID), r.Name, r.OtherNames, ""}
}
func songName(r store.Ref) name { return name{fmt.Sprintf("/song/%d", r.ID), r.Name, r.OtherNames, ""} }
func albumName(r store.Ref) name {
	return name{fmt.Sprintf("/album/%d", r.ID), r.Name, r.OtherNames, ""}
}
func artistNames(rs []store.Ref) []name {
	out := make([]name, len(rs))
	for i, r := range rs {
		out[i] = artistName(r)
	}
	return out
}

// listenRow is one listen in a table. Linked listens show names from the
// library. Others show the text exactly as received.
type listenRow struct {
	Fix        string // the Fix page, when the viewer can fix it
	Art        string // the album cover, 64 px
	Time       string
	Date       string
	Linked     bool
	Incomplete bool
	Song       name
	Version    string
	Artists    []name
	Album      *name
	RawTitle   string
	RawArtist  string
	RawAlbum   string
}

// listenRows turns listens into table rows. fix adds the Fix link, for
// the logged-in owner.
func (s *Server) listenRows(ctx context.Context, ls []store.Listen, loc *time.Location, fix bool) ([]listenRow, error) {
	var recs, rels []int64
	for _, l := range ls {
		if l.RecordingID != 0 {
			recs = append(recs, l.RecordingID)
		}
		if l.ReleaseID != 0 {
			rels = append(rels, l.ReleaseID)
		}
	}
	infos, err := s.db.RecordingInfos(ctx, recs)
	if err != nil {
		return nil, err
	}
	albums, err := s.db.ReleaseRefs(ctx, rels)
	if err != nil {
		return nil, err
	}
	// The cover of the album the listen is on, or of any album of the
	// recording.
	albumArt, err := s.db.ArtworkFor(ctx, "release", rels)
	if err != nil {
		return nil, err
	}
	recArt, err := s.db.RecordingCovers(ctx, recs)
	if err != nil {
		return nil, err
	}
	rows := make([]listenRow, len(ls))
	for i, l := range ls {
		t := time.Unix(l.ListenedAt, 0).In(loc)
		row := listenRow{
			Time: t.Format("15:04"), Date: t.Format("2 Jan 2006"), Incomplete: l.Incomplete,
			RawTitle: l.Title, RawArtist: l.Artist, RawAlbum: l.Album,
		}
		if fix {
			row.Fix = fmt.Sprintf("/listen/%d/fix", l.ID)
		}
		if row.Art = thumbOf(albumArt, l.ReleaseID); row.Art == "" {
			row.Art = thumbOf(recArt, l.RecordingID)
		}
		if info, ok := infos[l.RecordingID]; ok {
			row.Linked = true
			row.Song = songName(info.Song)
			row.Version = info.Version
			row.Artists = artistNames(info.Artists)
			if a, ok := albums[l.ReleaseID]; ok {
				n := albumName(a)
				row.Album = &n
			}
		}
		rows[i] = row
	}
	return rows, nil
}

// Periods

type periodTab struct {
	Label   string
	Href    string
	Current bool
}

type periodNav struct {
	Tabs   []periodTab
	Label  string
	Prev   string
	Next   string
	Custom bool
	From   string
	To     string
	Hidden url.Values // fields the filter form carries along
}

func weekStart(u store.User) time.Weekday {
	if u.WeekStart == 0 {
		return time.Sunday
	}
	return time.Monday
}

// periodFor reads ?period=, ?date= (any day in the period) and, for custom
// periods, ?from= and ?to=. The default is the current week.
func periodFor(r *http.Request, owner store.User, now time.Time) period.Period {
	loc := location(owner)
	q := r.URL.Query()
	parse := func(v string) (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		return t, err == nil
	}
	switch k := period.Kind(q.Get("period")); k {
	case period.Custom:
		from, ok1 := parse(q.Get("from"))
		to, ok2 := parse(q.Get("to"))
		if !ok1 || !ok2 {
			to = now.In(loc)
			from = to.AddDate(0, 0, -29)
		}
		return period.Days(from, to, loc)
	case period.Week, period.Month, period.Year, period.All:
		at := now
		if d, ok := parse(q.Get("date")); ok {
			at = d
		}
		return period.Containing(k, at, loc, weekStart(owner))
	}
	return period.Containing(period.Week, now, loc, weekStart(owner))
}

func periodParams(p period.Period) url.Values {
	v := url.Values{"period": {string(p.Kind)}}
	switch p.Kind {
	case period.Custom:
		v.Set("from", p.Start.Format("2006-01-02"))
		v.Set("to", p.End.AddDate(0, 0, -1).Format("2006-01-02"))
	case period.All:
	default:
		v.Set("date", p.Start.Format("2006-01-02"))
	}
	return v
}

// withParams keeps the page's other settings (sort, filters) when moving
// between periods.
func withParams(path string, base url.Values, p period.Period) string {
	v := url.Values{}
	for k, vs := range base {
		if k != "period" && k != "date" && k != "from" && k != "to" && k != "page" {
			v[k] = vs
		}
	}
	for k, vs := range periodParams(p) {
		v[k] = vs
	}
	return path + "?" + v.Encode()
}

func buildPeriodNav(path string, r *http.Request, p period.Period, owner store.User, now time.Time) periodNav {
	loc := location(owner)
	q := r.URL.Query()
	n := periodNav{Label: p.Label(), Custom: p.Kind == period.Custom}
	// Switching tabs keeps the date being looked at: from September 2026,
	// Week goes to its first week. From the current or all-time view, it's now.
	ref := p.Start
	if p.Kind == period.All || p.Contains(now) {
		ref = now
	}
	for _, t := range []struct {
		k     period.Kind
		label string
	}{{period.Week, "Week"}, {period.Month, "Month"}, {period.Year, "Year"}, {period.All, "All time"}, {period.Custom, "Custom"}} {
		var target period.Period
		if t.k == period.Custom {
			target = period.Days(now.AddDate(0, 0, -29), now, loc)
			if p.Kind == period.Custom {
				target = p
			}
		} else {
			target = period.Containing(t.k, ref, loc, weekStart(owner))
		}
		n.Tabs = append(n.Tabs, periodTab{t.label, withParams(path, q, target), p.Kind == t.k})
	}
	if p.Kind != period.All {
		n.Prev = withParams(path, q, p.Prev())
		if next := p.Next(); !next.Start.After(now) {
			n.Next = withParams(path, q, next)
		}
	}
	if n.Custom {
		n.From = p.Start.Format("2006-01-02")
		n.To = p.End.AddDate(0, 0, -1).Format("2006-01-02")
	}
	n.Hidden = url.Values{}
	for k, vs := range periodParams(p) {
		n.Hidden[k] = vs
	}
	return n
}

// Filters

type labelBox struct {
	ID      int64
	Name    string
	Checked bool
}

type filterForm struct {
	Labels      []labelBox
	ShowCombine bool
	Combine     bool
	Hidden      url.Values
}

// filters reads which labels to hide from a ranking's query. Until the form
// is submitted (f=1), each label's own default applies.
func (s *Server) filters(ctx context.Context, q url.Values, owner store.User, entityType string) (filterForm, []int64, error) {
	labels, err := s.db.LabelsInUse(ctx, owner.ID, entityType)
	if err != nil {
		return filterForm{}, nil, err
	}
	submitted := q.Get("f") == "1"
	var chosen []int64
	for _, v := range q["hide"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			chosen = append(chosen, id)
		}
	}
	var f filterForm
	var hide []int64
	for _, l := range labels {
		on := l.HideDefault
		if submitted {
			on = slices.Contains(chosen, l.ID)
		}
		f.Labels = append(f.Labels, labelBox{l.ID, l.Name, on})
		if on {
			hide = append(hide, l.ID)
		}
	}
	return f, hide, nil
}

func pageNum(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func pageLink(r *http.Request, page int) string {
	v := r.URL.Query()
	v.Set("page", strconv.Itoa(page))
	return r.URL.Path + "?" + v.Encode()
}

// Rankings

type songRow struct {
	Rank    int
	Art     string
	Song    name
	Version string
	Artists []name
	Listens int
}

type artistRow struct {
	Rank      int
	Art       string
	Artist    name
	Total     int
	Credited  int
	ViaGroups int
}

type albumRow struct {
	Rank    int
	Art     string
	Album   name
	Context string
	Artists []name
	Listens int
}

type sortHeader struct {
	Label  string
	Href   string
	Active bool
}

type topPage struct {
	Page
	Kind    string // "songs", "artists" or "albums"
	Period  periodNav
	Filters filterForm
	Songs   []songRow
	Artists []artistRow
	Albums  []albumRow
	Sorts   []sortHeader
	Prev    string
	Next    string
}

func (s *Server) topSongs(ctx context.Context, owner store.User, q store.RankQuery, combine bool) ([]songRow, error) {
	ranked, err := s.db.TopSongs(ctx, owner.ID, q, combine)
	if err != nil {
		return nil, err
	}
	shown := make([]int64, len(ranked))
	for i, r := range ranked {
		shown[i] = r.Shown
	}
	arts, err := s.db.RecordingCovers(ctx, shown)
	if err != nil {
		return nil, err
	}
	out := make([]songRow, len(ranked))
	for i, r := range ranked {
		out[i] = songRow{r.Rank, thumbOf(arts, r.Shown), songName(store.Ref{ID: r.SongID, Name: r.Name, OtherNames: r.OtherNames}), r.Version, artistNames(r.Artists), r.Listens}
	}
	return out, nil
}

func (s *Server) topArtists(ctx context.Context, owner store.User, q store.RankQuery, sort store.ArtistSort) ([]artistRow, error) {
	ranked, err := s.db.TopArtists(ctx, owner.ID, q, sort)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(ranked))
	for i, r := range ranked {
		ids[i] = r.Artist.ID
	}
	arts, err := s.db.ArtworkFor(ctx, "artist", ids)
	if err != nil {
		return nil, err
	}
	out := make([]artistRow, len(ranked))
	for i, r := range ranked {
		out[i] = artistRow{r.Rank, thumbOf(arts, r.Artist.ID), artistName(r.Artist), r.Total, r.Credited, r.ViaGroups}
	}
	return out, nil
}

func (s *Server) topAlbums(ctx context.Context, owner store.User, q store.RankQuery) ([]albumRow, error) {
	ranked, err := s.db.TopAlbums(ctx, owner.ID, q)
	if err != nil {
		return nil, err
	}
	return s.albumRows(ctx, ranked)
}

// albumRows turns ranked albums into table rows, with their covers.
func (s *Server) albumRows(ctx context.Context, ranked []store.AlbumRank) ([]albumRow, error) {
	ids := make([]int64, len(ranked))
	for i, r := range ranked {
		ids[i] = r.Album.ID
	}
	arts, err := s.db.ArtworkFor(ctx, "release", ids)
	if err != nil {
		return nil, err
	}
	out := make([]albumRow, len(ranked))
	for i, r := range ranked {
		out[i] = albumRow{r.Rank, thumbOf(arts, r.Album.ID), albumName(r.Album), r.Context, artistNames(r.Artists), r.Listens}
	}
	return out, nil
}

var entityOf = map[string]string{"songs": "song", "artists": "artist", "albums": "release"}
var topTitles = map[string]string{"songs": "Top songs", "artists": "Top artists", "albums": "Top albums"}

// catchingUpAt is how many songs waiting to be linked make rankings worth
// a note. New scrobbles are linked within seconds, so only an import or an
// update that relinks the history gets there.
const catchingUpAt = 25

// catchingUp says rankings are still filling in, while a big batch of
// listens waits to be linked. Once shown, the page counts it down to
// nothing by itself (see live.go).
func (s *Server) catchingUp(ctx context.Context) string {
	n, err := s.db.PendingJobs(ctx, "resolve")
	if err != nil || n < catchingUpAt {
		return ""
	}
	return sortingText(n)
}

func sortingText(n int) string {
	if n == 1 {
		return "Still sorting out your history, with 1 song to go. Rankings fill in as that finishes."
	}
	return numberPrinter.Sprintf("Still sorting out your history, with %d songs to go. Rankings fill in as that finishes.", n)
}

func (s *Server) top(kind string) func(http.ResponseWriter, *http.Request, *store.User, store.User) {
	return func(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
		ctx := r.Context()
		now := time.Now()
		p := periodFor(r, owner, now)
		path := r.URL.Path
		page := topPage{Page: Page{Title: topTitles[kind], Nav: kind, User: viewer, Sorting: s.catchingUp(ctx)}, Kind: kind, Period: buildPeriodNav(path, r, p, owner, now)}

		f, hide, err := s.filters(ctx, r.URL.Query(), owner, entityOf[kind])
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		q := r.URL.Query()
		f.Hidden = url.Values{}
		for k, vs := range page.Period.Hidden {
			f.Hidden[k] = vs
		}
		n := pageNum(r)
		rq := store.RankQuery{From: p.From(), To: p.To(), HideLabels: hide, Limit: rankPageSize + 1, Offset: (n - 1) * rankPageSize}
		more := false
		switch kind {
		case "songs":
			f.ShowCombine = true
			f.Combine = q.Get("f") != "1" || q.Get("combine") == "1"
			page.Songs, err = s.topSongs(ctx, owner, rq, f.Combine)
			if more = len(page.Songs) > rankPageSize; more {
				page.Songs = page.Songs[:rankPageSize]
			}
		case "artists":
			sort := store.ArtistSort(q.Get("sort"))
			if sort != store.ByCredited && sort != store.ByViaGroups {
				sort = store.ByTotal
			}
			if sort != store.ByTotal {
				f.Hidden.Set("sort", string(sort))
			}
			for _, h := range []struct {
				label string
				s     store.ArtistSort
			}{{"Total", store.ByTotal}, {"Credited", store.ByCredited}, {"Via groups", store.ByViaGroups}} {
				v := r.URL.Query()
				v.Set("sort", string(h.s))
				v.Del("page")
				page.Sorts = append(page.Sorts, sortHeader{h.label, path + "?" + v.Encode(), sort == h.s})
			}
			page.Artists, err = s.topArtists(ctx, owner, rq, sort)
			if more = len(page.Artists) > rankPageSize; more {
				page.Artists = page.Artists[:rankPageSize]
			}
		case "albums":
			page.Albums, err = s.topAlbums(ctx, owner, rq)
			if more = len(page.Albums) > rankPageSize; more {
				page.Albums = page.Albums[:rankPageSize]
			}
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		page.Filters = f
		if n > 1 {
			page.Prev = pageLink(r, n-1)
		}
		if more {
			page.Next = pageLink(r, n+1)
		}
		s.render(w, http.StatusOK, "top", page)
	}
}

// Home

type homePage struct {
	Page
	OwnerName string
	Playing   *playingBox
	Recent    []listenRow
	Week      string
	WeekQuery string
	Songs     []songRow
	Artists   []artistRow
	Albums    []albumRow
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	ctx := r.Context()
	now := time.Now()
	p := period.Containing(period.Week, now, location(owner), weekStart(owner))
	page := homePage{Page: Page{Title: "", Nav: "home", User: viewer, Sorting: s.catchingUp(ctx)}, OwnerName: owner.Shown(), Week: p.Label()}
	page.WeekQuery = periodParams(p).Encode()
	var err error
	page.Playing, err = s.nowPlaying(ctx, owner)
	if err == nil {
		page.Recent, err = s.recentListens(ctx, owner, viewer)
	}
	// The home page uses each label's default filter.
	var hideSongs, hideArtists, hideAlbums []int64
	if err == nil {
		_, hideSongs, err = s.filters(ctx, nil, owner, "song")
	}
	if err == nil {
		_, hideArtists, err = s.filters(ctx, nil, owner, "artist")
	}
	if err == nil {
		_, hideAlbums, err = s.filters(ctx, nil, owner, "release")
	}
	rq := func(hide []int64) store.RankQuery {
		return store.RankQuery{From: p.From(), To: p.To(), HideLabels: hide, Limit: homeTopSize}
	}
	if err == nil {
		page.Songs, err = s.topSongs(ctx, owner, rq(hideSongs), true)
	}
	if err == nil {
		page.Artists, err = s.topArtists(ctx, owner, rq(hideArtists), store.ByTotal)
	}
	if err == nil {
		page.Albums, err = s.topAlbums(ctx, owner, rq(hideAlbums))
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "home", page)
}

// Artist, song and album pages

type entityPage struct {
	Page
	Kind        string
	Name        name
	Labels      []string
	Listens     int
	Relations   store.ArtistRelations
	CountsFor   []name
	CountedFrom []name
	Members     []name
	MemberOf    []name
	By          []name // album artists
	Albums      []name // albums a song is on
	Recordings  []recordingRow
	AlbumRows   []albumRow // artists: albums credited to them
	Recent      []listenRow
	Tabs        []periodTab // logged in: Read and Edit
	Stats       []string    // listens, rank, first and last heard, shown under the name
	AlbumType   string      // albums: Album, Single, Soundtrack…
	Released    string      // albums: release date as set
	ShowCover   bool        // the infobox has a picture
	Cover       string      // 440 px, or "" for a plain square
}

type recordingRow struct {
	Rank        int
	Art         string
	RecordingID int64
	Song        name
	Version     string
	Artists     []name
	Listens     int
}

var kindWords = map[string]string{"artist": "artist", "song": "song", "release": "album"}

// statsLine is what's said under an item's name: "6 listens", "#1 album
// of all time", "first heard 1 Oct 2026", "last heard 3 Oct 2026".
func statsLine(st store.EntityStats, kind string, loc *time.Location) []string {
	if st.Listens == 0 {
		return []string{"No listens yet"}
	}
	out := []string{numberPrinter.Sprintf("%d listens", st.Listens)}
	if st.Listens == 1 {
		out[0] = "1 listen"
	}
	if st.Rank > 0 {
		out = append(out, numberPrinter.Sprintf("#%d %s of all time", st.Rank, kindWords[kind]))
	}
	day := func(t int64) string { return time.Unix(t, 0).In(loc).Format("2 Jan 2006") }
	if st.First == st.Last || day(st.First) == day(st.Last) {
		return append(out, "heard "+day(st.First))
	}
	return append(out, "first heard "+day(st.First), "last heard "+day(st.Last))
}

// itemCover puts an artist's or album's picture in its infobox.
func (s *Server) itemCover(ctx context.Context, p *entityPage, kind string, id int64) error {
	a, _, err := s.db.ItemArtwork(ctx, kind, id)
	p.ShowCover = true
	if a != nil {
		p.Cover = artURL(*a, 440)
	}
	return err
}

// addCovers gives recording rows their album covers.
func (s *Server) addCovers(ctx context.Context, rows []recordingRow) error {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.RecordingID
	}
	arts, err := s.db.RecordingCovers(ctx, ids)
	for i := range rows {
		rows[i].Art = thumbOf(arts, rows[i].RecordingID)
	}
	return err
}

// entity loads an entity by the {id} path value for the owner, redirecting
// when it was merged into another.
func (s *Server) entity(w http.ResponseWriter, r *http.Request, owner store.User, kind, path string) (store.Entity, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return store.Entity{}, false
	}
	e, err := s.db.Entity(r.Context(), owner.ID, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return e, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return e, false
	}
	if e.MergedInto != 0 {
		http.Redirect(w, r, fmt.Sprintf("%s/%d", path, e.MergedInto), http.StatusFound)
		return e, false
	}
	return e, true
}

func recordingRows(rcs []store.RecordingCount) []recordingRow {
	out := make([]recordingRow, len(rcs))
	for i, rc := range rcs {
		out[i] = recordingRow{Rank: i + 1, RecordingID: rc.RecordingID, Song: songName(rc.Song), Version: rc.Version, Artists: artistNames(rc.Artists), Listens: rc.Listens}
	}
	return out
}

const entityRecent = 20

func (s *Server) entityCommon(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User, kind string, e store.Entity) (entityPage, bool) {
	ctx := r.Context()
	p := entityPage{Page: Page{Title: e.Name, User: viewer, Error: editErrors[r.URL.Query().Get("err")]}, Kind: kind}
	s.doneNotice(r, viewer, &p.Page)
	var err error
	if viewer != nil {
		p.Tabs = viewTabs(kind, e.ID, "read")
	}
	if p.Labels, err = s.db.EntityLabels(ctx, kind, e.ID); err == nil {
		var st store.EntityStats
		if st, err = s.db.EntityStats(ctx, owner.ID, kind, e.ID); err == nil {
			p.Listens = st.Listens
			p.Stats = statsLine(st, kind, location(owner))
			var ls []store.Listen
			if ls, err = s.db.EntityListens(ctx, owner.ID, kind, e.ID, entityRecent); err == nil {
				p.Recent, err = s.listenRows(ctx, ls, location(owner), viewer != nil)
			}
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return p, false
	}
	return p, true
}

func (s *Server) artistPage(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	e, ok := s.entity(w, r, owner, "artist", "/artist")
	if !ok {
		return
	}
	p, ok := s.entityCommon(w, r, viewer, owner, "artist", e)
	if !ok {
		return
	}
	p.Name = artistName(e.Ref)
	rel, err := s.db.ArtistRelations(r.Context(), e.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.CountsFor, p.CountedFrom, p.Members, p.MemberOf = artistNames(rel.CountsFor), artistNames(rel.CountedFrom), artistNames(rel.Members), artistNames(rel.MemberOf)
	if err := s.itemCover(r.Context(), &p, "artist", e.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	albums, err := s.db.ArtistAlbums(r.Context(), owner.ID, e.ID)
	if err == nil {
		p.AlbumRows, err = s.albumRows(r.Context(), albums)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	top, err := s.db.ArtistTopRecordings(r.Context(), owner.ID, e.ID, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Recordings = recordingRows(top)
	if err := s.addCovers(r.Context(), p.Recordings); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "artist", p)
}

func (s *Server) songPage(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	e, ok := s.entity(w, r, owner, "song", "/song")
	if !ok {
		return
	}
	p, ok := s.entityCommon(w, r, viewer, owner, "song", e)
	if !ok {
		return
	}
	p.Name = songName(e.Ref)
	recs, err := s.db.SongRecordings(r.Context(), owner.ID, e.ID)
	if err == nil {
		p.Recordings = recordingRows(recs)
		if err = s.addCovers(r.Context(), p.Recordings); err == nil && len(p.Recordings) > 0 {
			// A song shows the cover of its first recording's album.
			p.ShowCover, p.Cover = true, strings.Replace(p.Recordings[0].Art, "-64.", "-440.", 1)
		}
		var albums []store.Ref
		if albums, err = s.db.SongReleases(r.Context(), e.ID); err == nil {
			for _, a := range albums {
				p.Albums = append(p.Albums, albumName(a))
			}
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "song", p)
}

func (s *Server) albumPage(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	e, ok := s.entity(w, r, owner, "release", "/album")
	if !ok {
		return
	}
	p, ok := s.entityCommon(w, r, viewer, owner, "release", e)
	if !ok {
		return
	}
	p.Name = albumName(e.Ref)
	if err := s.itemCover(r.Context(), &p, "release", e.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d, err := s.db.Album(r.Context(), e.ID); err == nil {
		for _, o := range albumKindNames {
			if o.Value == d.Kind && d.Kind != "other" {
				p.AlbumType = o.Label
			}
		}
		p.Released = d.Released
	}
	by, err := s.db.ReleaseArtists(r.Context(), e.ID)
	if err == nil {
		p.By = artistNames(by)
		var tracks []store.RecordingCount
		if tracks, err = s.db.AlbumTracks(r.Context(), owner.ID, e.ID); err == nil {
			p.Recordings = recordingRows(tracks)
			err = s.addCovers(r.Context(), p.Recordings)
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "album", p)
}
