package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chokominto/internal/jobs"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

// agent talks to a Server the way an MCP client does.
type agent struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	next int
}

func newAgent(t *testing.T, readOnly bool, listens ...[2]string) (*agent, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	uid, err := db.CreateUser(ctx, "ruby", "x")
	if err != nil {
		t.Fatal(err)
	}
	var ls []store.NewListen
	for i, l := range listens {
		ls = append(ls, store.NewListen{ListenedAt: time.Now().Unix() - int64(60*(i+1)), Artist: l[0], Title: l[1], Payload: []byte(`{}`)})
	}
	if _, err := db.InsertListens(ctx, uid, "listenbrainz", nil, ls); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &resolve.Resolver{DB: db}
	(&jobs.Runner{DB: db, Handlers: map[string]jobs.Handler{"resolve": r.Job}, Log: quiet}).Drain(ctx)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{DB: db, UserID: uid, Version: "test", ReadOnly: readOnly, Log: quiet}
	go func() { s.Serve(ctx, inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	return &agent{t: t, in: inW, out: bufio.NewScanner(outR)}, db
}

func (a *agent) send(line string) {
	a.t.Helper()
	if _, err := io.WriteString(a.in, line+"\n"); err != nil {
		a.t.Fatal(err)
	}
}

// call sends a request and returns its answer.
func (a *agent) call(method string, params any) map[string]any {
	a.t.Helper()
	a.next++
	p, _ := json.Marshal(params)
	a.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, a.next, method, p))
	if !a.out.Scan() {
		a.t.Fatal("no answer")
	}
	var resp map[string]any
	if err := json.Unmarshal(a.out.Bytes(), &resp); err != nil {
		a.t.Fatal(err)
	}
	if resp["id"] != float64(a.next) {
		a.t.Fatalf("answer to the wrong request: %s", a.out.Bytes())
	}
	return resp
}

// tool calls a tool and returns its result as JSON text, failing the test
// when the tool reports an error and wantErr is false.
func (a *agent) tool(name string, args map[string]any, wantErr bool) string {
	a.t.Helper()
	resp := a.call("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		a.t.Fatalf("%s: %v", name, resp)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] != wantErr {
		a.t.Fatalf("%s: isError %v: %s", name, res["isError"], text)
	}
	return text
}

func TestHandshakeAndTools(t *testing.T) {
	a, _ := newAgent(t, false)
	init := a.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || !strings.Contains(res["instructions"].(string), "Recording") {
		t.Fatalf("initialize: %v", res)
	}
	a.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`) // no answer expected
	if r := a.call("ping", nil); r["error"] != nil {
		t.Fatalf("ping: %v", r)
	}
	list := a.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range list {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"search", "search_sent_text", "show_song", "merge", "link_sent_text", "undo"} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Fatalf("no %s tool in %v", want, names)
		}
	}
	if r := a.call("nope", nil); r["error"].(map[string]any)["code"] != float64(methodNotFound) {
		t.Fatalf("unknown method: %v", r)
	}
	// A misspelled argument is an error the agent can read.
	if text := a.tool("search", map[string]any{"kind": "song", "qeury": "x"}, true); !strings.Contains(text, "qeury") {
		t.Fatal(text)
	}
}

func TestReadOnly(t *testing.T) {
	a, _ := newAgent(t, true)
	list := a.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	for _, tl := range list {
		if name := tl.(map[string]any)["name"]; name == "merge" || name == "undo" {
			t.Fatalf("read-only offers %s", name)
		}
	}
	if r := a.call("tools/call", map[string]any{"name": "merge", "arguments": map[string]any{}}); r["error"] == nil {
		t.Fatalf("merge allowed read-only: %v", r)
	}
}

func TestMergeSpellingsAndUndo(t *testing.T) {
	a, db := newAgent(t, false,
		[2]string{"YURiKA", "鏡面の波"}, [2]string{"YURiKA", "鏡面の波"},
		[2]string{"YURiKA", "Kyoumen no Nami"},
		[2]string{"YURiKA", "鏡面の波 (Instrumental)"})

	type sent struct {
		SourceID int64 `json:"source_id"`
		Title    string
		Listens  int
		LinkedTo struct {
			RecordingID int64 `json:"recording_id"`
			Song        struct{ ID int64 }
			Version     string
		} `json:"linked_to"`
	}
	var texts []sent
	json.Unmarshal([]byte(a.tool("search_sent_text", map[string]any{"query": "YURiKA"}, false)), &texts)
	if len(texts) != 3 || texts[0].Title != "鏡面の波" || texts[0].Listens != 2 {
		t.Fatalf("sent text: %+v", texts)
	}
	kanji, romaji := texts[0].LinkedTo.Song.ID, int64(0)
	for _, x := range texts {
		if x.Title == "Kyoumen no Nami" {
			romaji = x.LinkedTo.Song.ID
		}
	}
	if romaji == 0 || romaji == kanji {
		t.Fatalf("expected two songs: %+v", texts)
	}

	var merged change
	json.Unmarshal([]byte(a.tool("merge", map[string]any{"kind": "song", "from_id": romaji, "into_id": kanji}, false)), &merged)
	if merged.EditID == 0 || !strings.Contains(merged.Summary, "Merged") {
		t.Fatalf("merge: %+v", merged)
	}
	song := a.tool("show_song", map[string]any{"song_id": kanji}, false)
	if !strings.Contains(song, "Kyoumen no Nami") || !strings.Contains(song, `"version": "Instrumental"`) || !strings.Contains(song, `"listens": 4`) {
		t.Fatalf("merged song:\n%s", song)
	}
	if text := a.tool("show_song", map[string]any{"song_id": romaji}, true); !strings.Contains(text, "merged into") {
		t.Fatal(text)
	}

	a.tool("undo", map[string]any{"edit_id": merged.EditID}, false)
	if _, err := db.Entity(context.Background(), 1, "song", romaji); err != nil {
		t.Fatal(err)
	}
	if text := a.tool("show_song", map[string]any{"song_id": romaji}, false); !strings.Contains(text, "Kyoumen no Nami") {
		t.Fatalf("after undo:\n%s", text)
	}
	if changes := a.tool("recent_changes", nil, false); !strings.Contains(changes, `"undone": true`) {
		t.Fatalf("changes:\n%s", changes)
	}
}

func TestNamesAndCleanup(t *testing.T) {
	a, db := newAgent(t, false,
		[2]string{"Aniplex", "不可思議のカルテ 桜島麻衣 Ver."},
		[2]string{"瀬戸麻沙美", "不可思議のカルテ"},
		[2]string{"Some Channel", "Rick Astley - Never Gonna Give You Up"})
	ctx := context.Background()

	// Delete and the graveyard are marked so apps can ask first.
	list := a.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	for _, tl := range list {
		m := tl.(map[string]any)
		ann := m["annotations"].(map[string]any)
		want := m["name"] == "delete" || m["name"] == "move_to_graveyard"
		if ann["destructiveHint"] != want {
			t.Errorf("%s destructiveHint %v", m["name"], ann["destructiveHint"])
		}
	}

	var found []item
	json.Unmarshal([]byte(a.tool("search", map[string]any{"kind": "song", "query": "不可思議のカルテ"}, false)), &found)
	if len(found) != 2 {
		t.Fatalf("songs %+v", found)
	}
	song := found[0].ID
	a.tool("merge", map[string]any{"kind": "song", "from_id": found[1].ID, "into_id": song}, false)
	var shown struct {
		Names      []nameInfo
		Recordings []struct {
			RecordingID int64 `json:"recording_id"`
			Artists     []item
		}
	}
	json.Unmarshal([]byte(a.tool("show_song", map[string]any{"song_id": song}, false)), &shown)
	if len(shown.Names) != 1 || !shown.Names[0].Main || shown.Names[0].Kind != "original" {
		t.Fatalf("names %+v", shown.Names)
	}

	// Names: add the romaji, mark it, show it in lists, hide another.
	a.tool("add_name", map[string]any{"kind": "song", "id": song, "name": "Fukashigi no Karte"}, false)
	a.tool("add_name", map[string]any{"kind": "song", "id": song, "name": "fukashigi no carte lofi"}, false)
	json.Unmarshal([]byte(a.tool("show_song", map[string]any{"song_id": song}, false)), &shown)
	ids := map[string]int64{}
	for _, n := range shown.Names {
		ids[n.Name] = n.NameID
	}
	a.tool("set_name", map[string]any{"kind": "song", "id": song, "name_id": ids["Fukashigi no Karte"], "name_kind": "romaji", "in_lists": true}, false)
	a.tool("set_name", map[string]any{"kind": "song", "id": song, "name_id": ids["fukashigi no carte lofi"], "on_page": false}, false)
	e, _ := db.Entity(ctx, 1, "song", song)
	if e.Name != "不可思議のカルテ" && e.Name != "Fukashigi no Karte" || strings.Contains(e.OtherNames, "lofi") {
		t.Fatalf("shown %q / %q", e.Name, e.OtherNames)
	}
	a.tool("remove_name", map[string]any{"kind": "song", "id": song, "name_id": ids["fukashigi no carte lofi"]}, false)
	if text := a.tool("set_name", map[string]any{"kind": "song", "id": song, "name_id": 999}, true); !strings.Contains(text, "isn't one of the names there") {
		t.Fatal(text)
	}

	// The character's version is credited to the channel: credit the
	// character, new, and then the channel can go.
	var rec int64
	var channel int64
	for _, r := range shown.Recordings {
		if len(r.Artists) == 1 && r.Artists[0].Name == "Aniplex" {
			rec, channel = r.RecordingID, r.Artists[0].ID
		}
	}
	if rec == 0 {
		t.Fatalf("recordings %+v", shown.Recordings)
	}
	if text := a.tool("delete", map[string]any{"kind": "artist", "id": channel}, true); !strings.Contains(text, "1 songs") {
		t.Fatal(text)
	}
	c := a.tool("set_credits", map[string]any{"recording_id": rec, "new_artists": []string{"桜島麻衣"}}, false)
	if !strings.Contains(c, "Credited 桜島麻衣") {
		t.Fatal(c)
	}
	if c := a.tool("delete", map[string]any{"kind": "artist", "id": channel}, false); !strings.Contains(c, "Deleted the artist Aniplex") {
		t.Fatal(c)
	}

	// Not music.
	json.Unmarshal([]byte(a.tool("search", map[string]any{"kind": "song", "query": "Never Gonna"}, false)), &found)
	g := a.tool("move_to_graveyard", map[string]any{"song_id": found[0].ID}, false)
	if !strings.Contains(g, "graveyard with 1 listen") {
		t.Fatal(g)
	}
	if gs, _ := db.Graveyard(ctx, 1); len(gs) != 1 {
		t.Fatalf("graveyard %+v", gs)
	}
	a.tool("bring_back", map[string]any{"song_id": found[0].ID}, false)
	if gs, _ := db.Graveyard(ctx, 1); len(gs) != 0 {
		t.Fatalf("still in the graveyard %+v", gs)
	}
}

func TestTasks(t *testing.T) {
	a, db := newAgent(t, false,
		[2]string{"YURiKA", "鏡面の波"}, [2]string{"YURiKA", "Kyoumen no Nami"}, [2]string{"YURiKA", "Kyomen no Nami"})
	var found []item
	rows, _ := db.Reader().Query(`SELECT id FROM songs ORDER BY id`)
	for rows.Next() {
		var it item
		rows.Scan(&it.ID)
		found = append(found, it)
	}
	rows.Close()
	if len(found) != 3 {
		t.Fatalf("songs %+v", found)
	}
	var started struct {
		TaskID int64 `json:"task_id"`
	}
	json.Unmarshal([]byte(a.tool("start_task", map[string]any{"name": "Merge 鏡面の波 spellings"}, false)), &started)
	a.tool("merge", map[string]any{"kind": "song", "from_id": found[1].ID, "into_id": found[0].ID}, false)
	a.tool("merge", map[string]any{"kind": "song", "from_id": found[2].ID, "into_id": found[0].ID}, false)
	a.tool("finish_task", map[string]any{}, false)
	if changes := a.tool("recent_changes", map[string]any{"limit": 2}, false); strings.Count(changes, `"task": "Merge 鏡面の波 spellings"`) != 2 {
		t.Fatalf("changes:\n%s", changes)
	}
	if text := a.tool("undo_task", map[string]any{"task_id": started.TaskID}, false); !strings.Contains(text, "Undid all 2 changes") {
		t.Fatal(text)
	}
	for _, f := range found {
		if e, _ := db.Entity(context.Background(), 1, "song", f.ID); e.MergedInto != 0 {
			t.Fatalf("song %d still merged", f.ID)
		}
	}
	if text := a.tool("undo_task", map[string]any{"task_id": started.TaskID}, true); !strings.Contains(text, "already undone") {
		t.Fatal(text)
	}
}
