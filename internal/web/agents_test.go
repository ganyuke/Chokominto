package web

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// mcpPost sends one message to /mcp, the way an agent does.
func (e *env) mcpPost(token, body string, header ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// newAgentToken makes an agent token in Settings and returns it.
func (e *env) newAgentToken(label string, canChange bool) string {
	e.t.Helper()
	form := url.Values{"label": {label}}
	if canChange {
		form.Set("change", "1")
	}
	code, body, _ := e.post("/settings/agents", form)
	m := regexp.MustCompile(`Bearer ([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if code != http.StatusOK || m == nil {
		e.t.Fatalf("agent token: %d\n%s", code, body)
	}
	return m[1]
}

func TestAgentsOverHTTP(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"YURiKA", "鏡面の波", ""}, [3]string{"YURiKA", "Kyoumen no Nami", ""})
	call := func(name, args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	}
	search := call("search", `{"kind":"song","query":"YURiKA"}`)

	// Without a token, with a scrobbler's token, or as a browser elsewhere: no.
	if code, _ := e.mcpPost("", search); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := e.mcpPost(e.token, search); code != http.StatusUnauthorized {
		t.Fatalf("scrobbler token: %d", code)
	}
	look := e.newAgentToken("Claude, looking", false)
	change := e.newAgentToken("Claude", true)
	if code, _ := e.mcpPost(change, search, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("other site: %d", code)
	}
	// An agent token isn't a scrobbler token either.
	req, _ := http.NewRequest("GET", e.srv.URL+"/1/validate-token", nil)
	req.Header.Set("Authorization", "Token "+change)
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), `"valid":true`) {
		t.Fatal("agent token scrobbles")
	}

	code, body := e.mcpPost(change, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if code != http.StatusOK || !strings.Contains(body, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize: %d %s", code, body)
	}
	if code, _ := e.mcpPost(change, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	if resp, _ := http.Get(e.srv.URL + "/mcp"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}

	// The look-only token can search but not merge.
	if _, body := e.mcpPost(look, call("search_sent_text", `{"query":"YURiKA"}`)); !strings.Contains(body, "Kyoumen no Nami") {
		t.Fatalf("look: %s", body)
	}
	if _, body := e.mcpPost(look, call("merge", `{"kind":"song","from_id":2,"into_id":1}`)); !strings.Contains(body, `"error"`) {
		t.Fatalf("look-only merged: %s", body)
	}
	_, body = e.mcpPost(change, call("merge", `{"kind":"song","from_id":2,"into_id":1}`))
	if !strings.Contains(body, `Merged`) || strings.Contains(body, `"isError":true`) {
		t.Fatalf("merge: %s", body)
	}
	// Changes says who did it.
	if _, body, _ := e.get("/changes"); !strings.Contains(body, `<span class="muted">(by Claude)</span>`) {
		t.Fatalf("changes:\n%s", body)
	}
	_, body, _ = e.get("/settings")
	if !strings.Contains(body, "Look and change") || !strings.Contains(body, "Claude, looking") {
		t.Fatalf("settings:\n%s", body)
	}
	// Revoked, it stops working.
	m := regexp.MustCompile(`/settings/agents/(\d+)/revoke`).FindAllStringSubmatch(body, -1)
	for _, id := range m {
		e.post("/settings/agents/"+id[1]+"/revoke", nil)
	}
	if code, _ := e.mcpPost(change, search); code != http.StatusUnauthorized {
		t.Fatalf("revoked: %d", code)
	}
}

func TestUndoAgentTask(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"YURiKA", "鏡面の波", ""}, [3]string{"YURiKA", "Kyoumen no Nami", ""})
	token := e.newAgentToken("Claude", true)
	songs, _ := e.db.SearchItems(t.Context(), e.userID, "song", "nami", 5)
	if len(songs) != 2 {
		t.Fatalf("songs %v", songs)
	}
	call := func(name, args string) string {
		_, body := e.mcpPost(token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		return body
	}
	call("start_task", `{"name":"Merge spellings"}`)
	call("merge", fmt.Sprintf(`{"kind":"song","from_id":%d,"into_id":%d}`, songs[1].ID, songs[0].ID))
	call("add_name", fmt.Sprintf(`{"kind":"song","id":%d,"name":"Mirror Waves"}`, songs[0].ID))

	_, body, _ := e.get("/changes")
	if !strings.Contains(body, "Merge spellings <span class=\"muted\">(by Claude)</span> <span class=\"muted\">· 2 changes</span>") {
		t.Fatalf("changes:\n%s", body)
	}
	m := regexp.MustCompile(`action="(/changes/tasks/\d+/undo)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Undo all")
	}

	// The owner removed the agent's name since: nothing is undone.
	as, _ := e.db.Aliases(t.Context(), "song", songs[0].ID)
	for _, a := range as {
		if a.Name == "Mirror Waves" {
			e.post(fmt.Sprintf("/song/%d/edit", songs[0].ID), url.Values{"do": {"remove-name"}, "alias": {fmt.Sprint(a.ID)}})
		}
	}
	code, body, _ := e.post(m[1], nil)
	if code != http.StatusConflict || !strings.Contains(body, "Nothing was undone") || !strings.Contains(body, "Removed the name Mirror Waves") {
		t.Fatalf("conflict: %d\n%s", code, body)
	}
	if e2, _ := e.db.Entity(t.Context(), e.userID, "song", songs[1].ID); e2.MergedInto == 0 {
		t.Fatal("partly undone")
	}
	// After undoing that, Undo all works.
	_, body, _ = e.get("/changes")
	rename := regexp.MustCompile(`action="(/changes/\d+/undo)"`).FindStringSubmatch(body)
	e.post(rename[1], nil)
	if code, _, h := e.post(m[1], nil); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "task-undone") {
		t.Fatalf("undo all: %d %s", code, h.Get("Location"))
	}
	if e2, _ := e.db.Entity(t.Context(), e.userID, "song", songs[1].ID); e2.MergedInto != 0 {
		t.Fatal("merge not undone")
	}
	// It stays one row, with its own two changes, now undone.
	if _, body, _ = e.get("/changes"); !strings.Contains(body, "· 2 changes") || strings.Contains(body, "Undo: Merged") {
		t.Fatalf("after undo all:\n%s", body)
	}
}
