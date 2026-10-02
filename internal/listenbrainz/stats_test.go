package listenbrainz

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"chokominto/internal/jobs"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

func (e *env) linked(entries ...[3]string) {
	e.t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	var ls []store.NewListen
	for i, en := range entries {
		ls = append(ls, store.NewListen{ListenedAt: now - int64(60*(i+1)), Artist: en[0], Title: en[1], Album: en[2], Payload: []byte(`{}`)})
	}
	e.db.InsertListens(ctx, e.userID, "listenbrainz", nil, ls)
	r := &resolve.Resolver{DB: e.db}
	(&jobs.Runner{DB: e.db, Handlers: map[string]jobs.Handler{"resolve": r.Job}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Drain(ctx)
}

func TestStatsForPano(t *testing.T) {
	e := newEnv(t, true)
	e.linked(
		[3]string{"YOASOBI", "アイドル", "アイドル"},
		[3]string{"YOASOBI", "アイドル", "アイドル"},
		[3]string{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド"},
	)
	code, out, _ := e.do("GET", "/1/stats/user/elaina/artists?range=this_week&count=10", "", "")
	if code != 200 {
		t.Fatalf("artists: %d %v", code, out)
	}
	p := out["payload"].(map[string]any)
	artists := p["artists"].([]any)
	first := artists[0].(map[string]any)
	if first["artist_name"] != "YOASOBI" || first["listen_count"] != float64(2) || p["total_artist_count"] != float64(2) {
		t.Fatalf("%v", p)
	}
	// The character is hidden by default, like on the website.
	for _, a := range artists {
		if a.(map[string]any)["artist_name"] == "後藤ひとり" {
			t.Fatal("character in Pano's artist chart")
		}
	}
	code, out, _ = e.do("GET", "/1/stats/user/elaina/recordings?range=all_time", "", "")
	rec := out["payload"].(map[string]any)["recordings"].([]any)[0].(map[string]any)
	if code != 200 || rec["track_name"] != "アイドル" || rec["artist_name"] != "YOASOBI" {
		t.Fatalf("recordings: %d %v", code, rec)
	}
	code, out, _ = e.do("GET", "/1/stats/user/elaina/releases?range=this_month", "", "")
	rel := out["payload"].(map[string]any)["releases"].([]any)[0].(map[string]any)
	if code != 200 || rel["release_name"] != "アイドル" {
		t.Fatalf("releases: %d %v", code, rel)
	}
	// Paging.
	_, out, _ = e.do("GET", "/1/stats/user/elaina/artists?range=all_time&count=1&offset=1", "", "")
	p = out["payload"].(map[string]any)
	if len(p["artists"].([]any)) != 1 || p["offset"] != float64(1) || p["total_artist_count"] != float64(2) {
		t.Fatalf("paging: %v", p)
	}
	// Past the end: an empty page that still says how many there are.
	code, out, _ = e.do("GET", "/1/stats/user/elaina/recordings?range=all_time&offset=5", "", "")
	p = out["payload"].(map[string]any)
	if code != 200 || len(p["recordings"].([]any)) != 0 || p["total_recording_count"] != float64(2) {
		t.Fatalf("past the end: %d %v", code, p)
	}
	// Last year had nothing.
	if code, _, _ := e.do("GET", "/1/stats/user/elaina/artists?range=year", "", ""); code != 204 {
		t.Fatalf("empty range: %d", code)
	}
	if code, _, _ := e.do("GET", "/1/stats/user/elaina/artists?range=forever", "", ""); code != 400 {
		t.Fatalf("bad range: %d", code)
	}

	// Listening activity: days of this week add up to this week's listens.
	code, out, _ = e.do("GET", "/1/stats/user/elaina/listening-activity?range=this_week", "", "")
	days := out["payload"].(map[string]any)["listening_activity"].([]any)
	sum := 0.0
	for _, d := range days {
		sum += d.(map[string]any)["listen_count"].(float64)
	}
	if code != 200 || len(days) != 7 || sum != 3 {
		t.Fatalf("activity: %d days=%d sum=%v", code, len(days), sum)
	}
	_, out, _ = e.do("GET", "/1/stats/user/elaina/listening-activity?range=all_time", "", "")
	years := out["payload"].(map[string]any)["listening_activity"].([]any)
	if len(years) != 1 || years[0].(map[string]any)["listen_count"] != float64(3) {
		t.Fatalf("years: %v", years)
	}
}

func TestRangePeriods(t *testing.T) {
	u := store.User{TimeZone: "Asia/Tokyo", WeekStart: 1}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for rng, want := range map[string]string{
		"this_week": "28 Sep – 4 Oct 2026", "week": "21 – 27 Sep 2026",
		"this_month": "September 2026", "month": "August 2026",
		"this_year": "2026", "year": "2025",
		"quarter": "1 Apr – 30 Jun 2026", "half_yearly": "1 Jan – 30 Jun 2026",
		"all_time": "All time",
	} {
		p, ok := rangePeriod(rng, now, u)
		if !ok || p.Label() != want {
			t.Errorf("%s: %q, want %q", rng, p.Label(), want)
		}
	}
}
