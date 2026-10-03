package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"chokominto/internal/period"
	"chokominto/internal/store"
)

// Tools that answer questions about the owner's listening. They only look.

func init() { tools = append(tools, listeningTools...) }

// periodArgs are the arguments every listening tool takes, like the period
// picker on the ranking pages.
type periodArgs struct {
	Period string `json:"period"`
	Date   string `json:"date"`
	From   string `json:"from"`
	To     string `json:"to"`
}

var periodParams = []param{
	{"period", "string", "week, month, year or all (the default). Weeks, months and years are calendar ones in the owner's time zone.", false, []string{"week", "month", "year", "all"}},
	{"date", "string", "Any day in the week, month or year, as YYYY-MM-DD. Today if left out.", false, nil},
	{"from", "string", "First day of a span of your own, as YYYY-MM-DD. Use with to, in place of period.", false, nil},
	{"to", "string", "Last day of the span, included, as YYYY-MM-DD.", false, nil},
}

// spanned is what every listening result starts with, so the agent can say
// which span it counted.
type spanned struct {
	Period   string `json:"period"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"` // the last day, included
	TimeZone string `json:"time_zone"`
}

func (s *Server) periodOf(ctx context.Context, a periodArgs) (period.Period, spanned, error) {
	owner, err := s.DB.UserByID(ctx, s.UserID)
	if err != nil {
		return period.Period{}, spanned{}, err
	}
	loc, err := time.LoadLocation(owner.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	weekStart := time.Sunday
	if owner.WeekStart != 0 {
		weekStart = time.Monday
	}
	day := func(name, v string) (time.Time, error) {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return t, fmt.Errorf("%s must be a day as YYYY-MM-DD, not %q", name, v)
		}
		return t, nil
	}
	var p period.Period
	switch {
	case a.From != "" || a.To != "":
		if a.From == "" || a.To == "" {
			return p, spanned{}, fmt.Errorf("give both from and to")
		}
		if a.Period != "" || a.Date != "" {
			return p, spanned{}, fmt.Errorf("give either period and date, or from and to")
		}
		from, err := day("from", a.From)
		if err != nil {
			return p, spanned{}, err
		}
		to, err := day("to", a.To)
		if err != nil {
			return p, spanned{}, err
		}
		if to.Before(from) {
			return p, spanned{}, fmt.Errorf("to is before from")
		}
		p = period.Days(from, to, loc)
	default:
		k := period.Kind(a.Period)
		switch k {
		case "":
			k = period.All
		case period.Week, period.Month, period.Year, period.All:
		default:
			return p, spanned{}, fmt.Errorf("period must be week, month, year or all")
		}
		at := time.Now()
		if a.Date != "" {
			if at, err = day("date", a.Date); err != nil {
				return p, spanned{}, err
			}
		}
		p = period.Containing(k, at, loc, weekStart)
	}
	sp := spanned{Period: p.Label(), TimeZone: loc.String()}
	if p.Kind != period.All {
		sp.From, sp.To = p.Start.Format("2006-01-02"), p.End.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return p, sp, nil
}

// splitArgs decodes a tool's own arguments and the period ones from the
// same object.
func splitArgs(raw json.RawMessage, own any) (periodArgs, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return periodArgs{}, fmt.Errorf("arguments: %w", err)
	}
	mine, theirs := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for k, v := range all {
		switch k {
		case "period", "date", "from", "to":
			theirs[k] = v
		default:
			mine[k] = v
		}
	}
	var pa periodArgs
	b, _ := json.Marshal(theirs)
	if err := decode(b, &pa); err != nil {
		return pa, err
	}
	b, _ = json.Marshal(mine)
	return pa, decode(b, own)
}

var listeningTools = []tool{
	{
		name:        "top",
		description: "The owner's top songs, artists or albums in a period, as the ranking pages show them. Songs count every version together, except versions marked to rank on their own row. Artists count listens of their groups too. Items with a label hidden by default (like characters) are left out unless include_hidden is set.",
		readOnly:    true,
		params: append([]param{
			{"kind", "string", "What to rank.", true, []string{"songs", "artists", "albums"}},
			{"limit", "integer", "At most this many, 25 if left out, up to 100.", false, nil},
			{"offset", "integer", "Skip this many, for the next page.", false, nil},
			{"include_hidden", "boolean", "Also rank items with a label hidden by default.", false, nil},
		}, periodParams...),
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind          string
				Limit, Offset int
				IncludeHidden bool `json:"include_hidden"`
			}
			pa, err := splitArgs(raw, &a)
			if err != nil {
				return nil, err
			}
			entity := map[string]string{"songs": "song", "artists": "artist", "albums": "release"}[a.Kind]
			if entity == "" {
				return nil, fmt.Errorf("kind must be one of songs, artists, albums")
			}
			p, sp, err := s.periodOf(ctx, pa)
			if err != nil {
				return nil, err
			}
			var hide []int64
			if !a.IncludeHidden {
				labels, err := s.DB.LabelsInUse(ctx, s.UserID, entity)
				if err != nil {
					return nil, err
				}
				for _, l := range labels {
					if l.HideDefault {
						hide = append(hide, l.ID)
					}
				}
			}
			n := a.Limit
			if n <= 0 {
				n = 25
			}
			q := store.RankQuery{From: p.From(), To: p.To(), HideLabels: hide, Limit: min(n, 100), Offset: max(a.Offset, 0)}

			type songRow struct {
				Rank    int    `json:"rank"`
				Song    item   `json:"song"`
				Version string `json:"version,omitempty"`
				Artists []item `json:"artists"`
				Listens int    `json:"listens"`
			}
			type artistRow struct {
				Rank      int  `json:"rank"`
				Artist    item `json:"artist"`
				Listens   int  `json:"listens"`
				Credited  int  `json:"credited"`
				ViaGroups int  `json:"via_groups"`
			}
			type albumRow struct {
				Rank    int    `json:"rank"`
				Album   item   `json:"album"`
				Artists []item `json:"artists"`
				Listens int    `json:"listens"`
			}
			out := struct {
				spanned
				Songs   []songRow   `json:"songs,omitempty"`
				Artists []artistRow `json:"artists,omitempty"`
				Albums  []albumRow  `json:"albums,omitempty"`
				Ranked  int         `json:"ranked_in_all"`
				Hidden  bool        `json:"some_hidden,omitempty"`
			}{spanned: sp, Hidden: len(hide) > 0}
			switch a.Kind {
			case "songs":
				rows, err := s.DB.TopSongs(ctx, s.UserID, q, true)
				if err != nil {
					return nil, err
				}
				out.Songs = []songRow{}
				for _, r := range rows {
					out.Songs = append(out.Songs, songRow{r.Rank, item{r.SongID, r.Name, r.OtherNames}, r.Version, items(r.Artists), r.Listens})
					out.Ranked = r.Of
				}
			case "artists":
				rows, err := s.DB.TopArtists(ctx, s.UserID, q, store.ByTotal)
				if err != nil {
					return nil, err
				}
				out.Artists = []artistRow{}
				for _, r := range rows {
					out.Artists = append(out.Artists, artistRow{r.Rank, item{r.Artist.ID, r.Artist.Name, r.Artist.OtherNames}, r.Total, r.Credited, r.ViaGroups})
					out.Ranked = r.Of
				}
			case "albums":
				rows, err := s.DB.TopAlbums(ctx, s.UserID, q)
				if err != nil {
					return nil, err
				}
				out.Albums = []albumRow{}
				for _, r := range rows {
					out.Albums = append(out.Albums, albumRow{r.Rank, item{r.Album.ID, r.Album.Name, r.Album.OtherNames}, items(r.Artists), r.Listens})
					out.Ranked = r.Of
				}
			}
			return out, nil
		},
	},
	{
		name:        "listens_in",
		description: "How many times the owner listened to a song, artist or album in a period, and how many listens there were in all. Leave out kind and id for just the total. An artist's count includes their groups' listens.",
		readOnly:    true,
		params: append([]param{
			{"kind", "string", "What to count.", false, []string{"song", "artist", "album"}},
			{"id", "integer", "Its id, from search or another tool.", false, nil},
		}, periodParams...),
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind string
				ID   int64
			}
			pa, err := splitArgs(raw, &a)
			if err != nil {
				return nil, err
			}
			p, sp, err := s.periodOf(ctx, pa)
			if err != nil {
				return nil, err
			}
			out := struct {
				spanned
				Item    *item `json:"item,omitempty"`
				Listens *int  `json:"listens,omitempty"`
				All     int   `json:"all_listens"`
			}{spanned: sp}
			if a.Kind != "" || a.ID != 0 {
				kind, err := kindOf(a.Kind, "song", "artist", "album")
				if err != nil {
					return nil, err
				}
				e, err := s.live(ctx, kind, a.ID)
				if err != nil {
					return nil, err
				}
				n, err := s.DB.EntityListenCountIn(ctx, s.UserID, kind, e.ID, p.From(), p.To())
				if err != nil {
					return nil, err
				}
				out.Item, out.Listens = &item{e.ID, e.Name, e.OtherNames}, &n
			}
			if out.All, err = s.DB.ListenCountIn(ctx, s.UserID, p.From(), p.To()); err != nil {
				return nil, err
			}
			return out, nil
		},
	},
	{
		name:        "history",
		description: "The owner's listens in a period, newest first: when, what the player sent, and the song and album it's linked to. For more, pass the next value from the result.",
		readOnly:    true,
		params: append([]param{
			{"limit", "integer", "At most this many, 50 if left out, up to 200.", false, nil},
			{"next", "string", "The next value from the previous result, to go on from there.", false, nil},
		}, periodParams...),
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Limit int
				Next  string
			}
			pa, err := splitArgs(raw, &a)
			if err != nil {
				return nil, err
			}
			p, sp, err := s.periodOf(ctx, pa)
			if err != nil {
				return nil, err
			}
			before := store.CursorAt(p.To())
			if a.Next != "" {
				ts, id, ok := strings.Cut(a.Next, ".")
				before.TS, err = strconv.ParseInt(ts, 10, 64)
				if err == nil {
					before.ID, err = strconv.ParseInt(id, 10, 64)
				}
				if !ok || err != nil {
					return nil, fmt.Errorf("next must be the value from the previous result")
				}
			}
			n := a.Limit
			if n <= 0 {
				n = 50
			}
			n = min(n, 200)
			ls, err := s.DB.Listens(ctx, s.UserID, store.ListenRange{Before: &before, After: new(store.CursorAfter(p.From() - 1)), Limit: n + 1})
			if err != nil {
				return nil, err
			}
			more := len(ls) > n
			ls = ls[:min(len(ls), n)]
			var recs, rels []int64
			for _, l := range ls {
				recs = append(recs, l.RecordingID)
				rels = append(rels, l.ReleaseID)
			}
			infos, err := s.DB.RecordingInfos(ctx, recs)
			if err != nil {
				return nil, err
			}
			albums, err := s.DB.ReleaseRefs(ctx, rels)
			if err != nil {
				return nil, err
			}
			loc, err := time.LoadLocation(sp.TimeZone)
			if err != nil {
				loc = time.UTC
			}
			type sent struct {
				Artist string `json:"artist"`
				Title  string `json:"title"`
				Album  string `json:"album,omitempty"`
			}
			type listen struct {
				At       string     `json:"at"`
				Sent     sent       `json:"sent"`
				LinkedTo *recording `json:"linked_to"`
				Album    *item      `json:"album,omitempty"`
			}
			out := struct {
				spanned
				Listens []listen `json:"listens"`
				Next    string   `json:"next,omitempty"`
			}{spanned: sp, Listens: []listen{}}
			for _, l := range ls {
				row := listen{At: time.Unix(l.ListenedAt, 0).In(loc).Format(time.RFC3339), Sent: sent{Artist: l.Artist, Title: l.Title, Album: l.Album}}
				if ri, ok := infos[l.RecordingID]; ok {
					r := recordingOf(ri)
					row.LinkedTo = &r
				}
				if al, ok := albums[l.ReleaseID]; ok {
					row.Album = &item{al.ID, al.Name, al.OtherNames}
				}
				out.Listens = append(out.Listens, row)
			}
			if more {
				last := ls[len(ls)-1]
				out.Next = fmt.Sprintf("%d.%d", last.ListenedAt, last.ID)
			}
			return out, nil
		},
	},
}
