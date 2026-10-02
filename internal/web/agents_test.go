package web

import (
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
