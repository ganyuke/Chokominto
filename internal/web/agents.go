package web

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"chokominto/internal/auth"
	"chokominto/internal/mcp"
	"chokominto/internal/store"
)

// maxMCPBytes caps one message from an agent. Tool calls are small.
const maxMCPBytes = 1 << 20

// mcpEndpoint is the Model Context Protocol over HTTP ("Streamable HTTP"),
// for AI agents that can't start chokominto mcp themselves: a hosted
// server, a container, claude.ai. Each POST carries one JSON-RPC message
// and gets its answer back as plain JSON. There's no event stream and no
// session, since no tool sends anything later.
func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Agents send messages with POST.", http.StatusMethodNotAllowed)
		return
	}
	// Agents aren't browsers. A browser page elsewhere must not reach this,
	// whatever it learned, so any other site's Origin is turned away (the
	// protocol asks for this, against DNS rebinding).
	if o := r.Header.Get("Origin"); o != "" && !s.ownOrigin(r, o) {
		http.Error(w, "Not from this site.", http.StatusForbidden)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="chokominto"`)
		http.Error(w, "Send an agent token from Settings as Authorization: Bearer <token>.", http.StatusUnauthorized)
		return
	}
	tokenID, userID, canChange, err := s.db.AgentTokenUser(r.Context(), auth.HashSecret(strings.TrimSpace(token)))
	if errors.Is(err, store.ErrNotFound) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="chokominto", error="invalid_token"`)
		http.Error(w, "That agent token isn't valid. Make a new one in Settings.", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBytes))
	if err != nil {
		http.Error(w, "Message too big.", http.StatusRequestEntityTooLarge)
		return
	}
	agent := &mcp.Server{DB: s.db, UserID: userID, Version: Version, ReadOnly: !canChange, Agent: token[:min(len(token), 6)] + "…", Log: s.log}
	answer := agent.Answer(store.WithAgent(r.Context(), tokenID), body)
	if answer == nil {
		w.WriteHeader(http.StatusAccepted) // a notification
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(answer)
}

// ownOrigin reports whether a browser Origin is this site.
func (s *Server) ownOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if s.cfg.PublicURL != "" {
		p, err := url.Parse(s.cfg.PublicURL)
		return err == nil && p.Scheme == u.Scheme && p.Host == u.Host
	}
	return u.Host == r.Host
}

// Agent tokens in Settings.

type newAgent struct {
	Label, Secret string
	CanChange     bool
	Command       string // how to add it to Claude Code
}

func (s *Server) createAgentToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		s.settingsError(w, r, u, "Name the token after the agent that will use it.")
		return
	}
	canChange := r.PostFormValue("change") == "1"
	secret := auth.NewSecret()
	if _, err := s.db.CreateAgentToken(r.Context(), u.ID, label, auth.HashSecret(secret), canChange); err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.settingsData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.NewAgent = &newAgent{Label: label, Secret: secret, CanChange: canChange,
		Command: "claude mcp add --transport http chokominto " + p.BaseURL + "/mcp --header \"Authorization: Bearer " + secret + "\""}
	s.render(w, http.StatusOK, "settings", p)
}

func (s *Server) revokeAgentToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.revoke(w, r, u, "/settings?notice=agent-revoked#agents")
}
