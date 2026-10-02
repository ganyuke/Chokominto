package listenbrainz

import (
	"context"
	"net/http"
	"time"

	"chokominto/internal/period"
	"chokominto/internal/store"
)

// Stats endpoints, for Pano Scrobbler's charts screen. They're answered from
// the same rankings as the website, in the owner's time zone, with each
// label's default filter (so characters are hidden like on the website).

func weekStartOf(u store.User) time.Weekday {
	if u.WeekStart == 0 {
		return time.Sunday
	}
	return time.Monday
}

func locationOf(u store.User) *time.Location {
	if loc, err := time.LoadLocation(u.TimeZone); err == nil {
		return loc
	}
	return time.UTC
}

// rangePeriod maps a ListenBrainz range onto a period. "week", "month" and
// "year" are the last complete ones, "this_…" the current ones, "quarter"
// and "half_yearly" the last complete calendar quarter and half year.
func rangePeriod(rng string, now time.Time, u store.User) (period.Period, bool) {
	loc, ws := locationOf(u), weekStartOf(u)
	now = now.In(loc)
	switch rng {
	case "this_week":
		return period.Containing(period.Week, now, loc, ws), true
	case "this_month":
		return period.Containing(period.Month, now, loc, ws), true
	case "this_year":
		return period.Containing(period.Year, now, loc, ws), true
	case "week":
		return period.Containing(period.Week, now, loc, ws).Prev(), true
	case "month":
		return period.Containing(period.Month, now, loc, ws).Prev(), true
	case "year":
		return period.Containing(period.Year, now, loc, ws).Prev(), true
	case "quarter", "half_yearly":
		months := 3
		if rng == "half_yearly" {
			months = 6
		}
		startMonth := (int(now.Month())-1)/months*months + 1
		current := time.Date(now.Year(), time.Month(startMonth), 1, 0, 0, 0, 0, loc)
		prev := current.AddDate(0, -months, 0)
		return period.Days(prev, current.AddDate(0, 0, -1), loc), true
	case "", "all_time":
		return period.Containing(period.All, now, loc, ws), true
	}
	return period.Period{}, false
}

func (a *API) defaultHidden(ctx context.Context, userID int64, entityType string) ([]int64, error) {
	labels, err := a.DB.LabelsInUse(ctx, userID, entityType)
	var hide []int64
	for _, l := range labels {
		if l.HideDefault {
			hide = append(hide, l.ID)
		}
	}
	return hide, err
}

type statsEntry struct {
	ArtistName  string   `json:"artist_name"`
	ArtistMBIDs []string `json:"artist_mbids"`
	TrackName   string   `json:"track_name,omitempty"`
	ReleaseName string   `json:"release_name,omitempty"`
	ListenCount int      `json:"listen_count"`
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	u, ok := a.readUser(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	var entity, key, totalKey string
	switch kind {
	case "artists":
		entity, key, totalKey = "artist", "artists", "total_artist_count"
	case "releases":
		entity, key, totalKey = "release", "releases", "total_release_count"
	case "recordings":
		entity, key, totalKey = "song", "recordings", "total_recording_count"
	default:
		writeError(w, http.StatusNotFound, "No such statistic.")
		return
	}
	rng := r.URL.Query().Get("range")
	p, ok := rangePeriod(rng, time.Now(), u)
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid range.")
		return
	}
	count, has, err := queryInt(r, "count")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !has {
		count = 25
	}
	count = min(max(count, 1), 100)
	offset, _, err := queryInt(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	hide, err := a.defaultHidden(ctx, u.ID, entity)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	// Only the asked-for page is looked up. The ranking also says how many
	// there are in all.
	q := store.RankQuery{From: p.From(), To: p.To(), HideLabels: hide, Limit: int(count), Offset: int(offset)}
	page, total, err := a.statsPage(ctx, u.ID, kind, q)
	if err == nil && len(page) == 0 && offset > 0 {
		// Past the end: say how many there are, with an empty page.
		q.Limit, q.Offset = 1, 0
		_, total, err = a.statsPage(ctx, u.ID, kind, q)
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	// Nothing to show: ListenBrainz answers 204, which Pano shows as empty.
	if total == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if page == nil {
		page = []statsEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{
		key:            page,
		totalKey:       total,
		"count":        len(page),
		"offset":       offset,
		"range":        rngOrAll(rng),
		"from_ts":      clampTS(p.From()),
		"to_ts":        clampTS(p.To()),
		"last_updated": time.Now().Unix(),
		"user_id":      u.Name,
	}})
}

// statsPage ranks one page for Pano's charts, and how many there are in all.
func (a *API) statsPage(ctx context.Context, userID int64, kind string, q store.RankQuery) ([]statsEntry, int, error) {
	var entries []statsEntry
	total := 0
	switch kind {
	case "artists":
		ranked, err := a.DB.TopArtists(ctx, userID, q, store.ByTotal)
		if err != nil {
			return nil, 0, err
		}
		for _, x := range ranked {
			entries = append(entries, statsEntry{ArtistName: x.Artist.Name, ArtistMBIDs: []string{}, ListenCount: x.Total})
			total = x.Of
		}
	case "releases":
		ranked, err := a.DB.TopAlbums(ctx, userID, q)
		if err != nil {
			return nil, 0, err
		}
		for _, x := range ranked {
			entries = append(entries, statsEntry{ArtistName: store.JoinNames(x.Artists), ArtistMBIDs: []string{}, ReleaseName: x.Album.Name, ListenCount: x.Listens})
			total = x.Of
		}
	case "recordings":
		ranked, err := a.DB.TopSongs(ctx, userID, q, false)
		if err != nil {
			return nil, 0, err
		}
		for _, x := range ranked {
			entries = append(entries, statsEntry{ArtistName: store.JoinNames(x.Artists), ArtistMBIDs: []string{}, TrackName: x.Name, ListenCount: x.Listens})
			total = x.Of
		}
	}
	return entries, total, nil
}

func rngOrAll(r string) string {
	if r == "" {
		return "all_time"
	}
	return r
}

// clampTS keeps "all time" from reporting year 9999.
func clampTS(ts int64) int64 {
	return min(ts, time.Now().Unix())
}

type activity struct {
	FromTS      int64  `json:"from_ts"`
	ToTS        int64  `json:"to_ts"`
	TimeRange   string `json:"time_range"`
	ListenCount int    `json:"listen_count"`
}

// listeningActivity counts listens per day, month or year of the range:
// years for all time, months for a year or half year, days otherwise.
func (a *API) listeningActivity(w http.ResponseWriter, r *http.Request) {
	u, ok := a.readUser(w, r)
	if !ok {
		return
	}
	rng := r.URL.Query().Get("range")
	now := time.Now()
	p, ok := rangePeriod(rng, now, u)
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid range.")
		return
	}
	loc, ws := locationOf(u), weekStartOf(u)
	ctx := r.Context()

	var buckets []period.Period
	var format string
	switch rng {
	case "", "all_time":
		_, oldest, _, err := a.DB.ListenStats(ctx, u.ID)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		if oldest == 0 {
			oldest = now.Unix()
		}
		for y := period.Containing(period.Year, time.Unix(oldest, 0), loc, ws); !y.Start.After(now); y = y.Next() {
			buckets = append(buckets, y)
		}
		format = "2006"
	case "year", "this_year", "half_yearly":
		for m := period.Containing(period.Month, p.Start, loc, ws); m.Start.Before(p.End); m = m.Next() {
			buckets = append(buckets, m)
		}
		format = "January"
	default:
		for d := period.Days(p.Start, p.Start, loc); d.Start.Before(p.End); d = d.Next() {
			buckets = append(buckets, d)
		}
		format = "Monday 2 January"
	}
	out := make([]activity, 0, len(buckets))
	for _, b := range buckets {
		n, err := a.DB.ListenCountIn(ctx, u.ID, b.From(), b.To())
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out = append(out, activity{b.From(), b.To(), b.Start.Format(format), n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"payload": map[string]any{
		"from_ts":            clampTS(p.From()),
		"to_ts":              clampTS(p.To()),
		"last_updated":       now.Unix(),
		"range":              rngOrAll(rng),
		"user_id":            u.Name,
		"listening_activity": out,
	}})
}
