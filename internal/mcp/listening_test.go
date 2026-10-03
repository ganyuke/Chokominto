package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestListening(t *testing.T) {
	a, _ := newAgent(t, true,
		[2]string{"YOASOBI", "アイドル"}, [2]string{"YOASOBI", "アイドル"}, [2]string{"Ayase", "夜に駆ける"})

	// The listening tools only look, so a look-only agent has them.
	list := a.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range list {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"top", "listens_in", "history"} {
		if !strings.Contains(" "+strings.Join(names, " ")+" ", " "+want+" ") {
			t.Fatalf("no %s in %v", want, names)
		}
	}

	var songs struct {
		Period string
		Songs  []struct {
			Rank    int
			Song    item
			Listens int
		}
		Ranked int `json:"ranked_in_all"`
	}
	json.Unmarshal([]byte(a.tool("top", map[string]any{"kind": "songs"}, false)), &songs)
	if songs.Period != "All time" || songs.Ranked != 2 || len(songs.Songs) != 2 || songs.Songs[0].Song.Name != "アイドル" || songs.Songs[0].Listens != 2 {
		t.Fatalf("top songs %+v", songs)
	}
	var artists struct {
		Artists []struct {
			Artist  item
			Listens int
		}
	}
	json.Unmarshal([]byte(a.tool("top", map[string]any{"kind": "artists", "limit": 1}, false)), &artists)
	if len(artists.Artists) != 1 || artists.Artists[0].Artist.Name != "YOASOBI" || artists.Artists[0].Listens != 2 {
		t.Fatalf("top artists %+v", artists)
	}
	if text := a.tool("top", map[string]any{"kind": "albums"}, false); !strings.Contains(text, `"ranked_in_all": 0`) {
		t.Fatal(text)
	}

	// Counted over the last two days, a span of the agent's choosing.
	today := time.Now().UTC()
	from, to := today.AddDate(0, 0, -1).Format("2006-01-02"), today.Format("2006-01-02")
	yoasobi := artists.Artists[0].Artist.ID
	var counted struct {
		Period, From, To string
		Item             item
		Listens          int
		All              int `json:"all_listens"`
	}
	json.Unmarshal([]byte(a.tool("listens_in", map[string]any{"kind": "artist", "id": yoasobi, "from": from, "to": to}, false)), &counted)
	if counted.Listens != 2 || counted.All != 3 || counted.From != from || counted.To != to || counted.Item.Name != "YOASOBI" {
		t.Fatalf("listens_in %+v", counted)
	}
	// A year long ago has none.
	json.Unmarshal([]byte(a.tool("listens_in", map[string]any{"period": "year", "date": "2001-06-01"}, false)), &counted)
	if counted.Period != "2001" || counted.All != 0 {
		t.Fatalf("2001 %+v", counted)
	}

	// History, a page at a time.
	type page struct {
		Listens []struct {
			At       string
			Sent     struct{ Artist, Title string }
			LinkedTo *recording `json:"linked_to"`
		}
		Next string
	}
	var p1, p2 page
	json.Unmarshal([]byte(a.tool("history", map[string]any{"limit": 2}, false)), &p1)
	if len(p1.Listens) != 2 || p1.Next == "" || p1.Listens[0].LinkedTo == nil || p1.Listens[0].LinkedTo.Song.Name != "アイドル" {
		t.Fatalf("page 1 %+v", p1)
	}
	json.Unmarshal([]byte(a.tool("history", map[string]any{"limit": 2, "next": p1.Next}, false)), &p2)
	if len(p2.Listens) != 1 || p2.Next != "" || p2.Listens[0].Sent.Title != "夜に駆ける" {
		t.Fatalf("page 2 %+v", p2)
	}

	// Mistakes are errors the agent can read.
	for args, want := range map[string]string{
		`{"period":"month","date":"2026-13-01"}`:                  "date must be a day",
		`{"from":"2026-01-01"}`:                                   "both from and to",
		`{"from":"2026-02-01","to":"2026-01-01"}`:                 "before from",
		`{"period":"decade"}`:                                     "period must be",
		`{"period":"week","from":"2026-01-01","to":"2026-01-02"}`: "either period",
	} {
		var m map[string]any
		json.Unmarshal([]byte(args), &m)
		if text := a.tool("history", m, true); !strings.Contains(text, want) {
			t.Errorf("%s: %s", args, text)
		}
	}
	if text := a.tool("top", map[string]any{"kind": "songs", "perod": "week"}, true); !strings.Contains(text, "perod") {
		t.Fatal(text)
	}
}

func TestAnswerSuggestions(t *testing.T) {
	a, db := newAgent(t, false, [2]string{"YOASOBI", "アイドル"}, [2]string{"YOASOBI", "Aidoru"})
	if err := db.FindSuggestions(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var review struct {
		Suggestions []struct {
			ID   int64
			Kind string
		} `json:"suggested_merges"`
	}
	json.Unmarshal([]byte(a.tool("review", map[string]any{}, false)), &review)
	if len(review.Suggestions) != 1 || review.Suggestions[0].ID == 0 || review.Suggestions[0].Kind != "recording" {
		t.Fatalf("review %+v", review)
	}
	id := review.Suggestions[0].ID

	var done change
	json.Unmarshal([]byte(a.tool("answer_suggestions", map[string]any{"ids": []int64{id}, "answer": "different"}, false)), &done)
	if done.EditID == 0 || !strings.Contains(done.Summary, "different") {
		t.Fatalf("answered %+v", done)
	}
	if text := a.tool("review", map[string]any{}, false); !strings.Contains(text, `"suggested_merges": []`) {
		t.Fatal(text)
	}
	if text := a.tool("answer_suggestions", map[string]any{"ids": []int64{id}, "answer": "different"}, true); !strings.Contains(text, "already answered") {
		t.Fatal(text)
	}
	// Undone, it's back to answer.
	a.tool("undo", map[string]any{"edit_id": done.EditID}, false)
	json.Unmarshal([]byte(a.tool("review", map[string]any{}, false)), &review)
	if len(review.Suggestions) != 1 {
		t.Fatalf("after undo %+v", review)
	}
	a.tool("answer_suggestions", map[string]any{"ids": []int64{id}, "answer": "version"}, false)
	if text := a.tool("search", map[string]any{"kind": "song", "query": "aidoru"}, false); strings.Count(text, `"id"`) != 1 {
		t.Fatalf("not one song: %s", text)
	}
}
