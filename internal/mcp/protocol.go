// Package mcp lets an AI agent look through and tidy the owner's music
// over the Model Context Protocol: JSON-RPC 2.0 messages, either one per
// line on stdin and stdout (chokominto mcp) or one per HTTP request (the
// website's /mcp). It speaks the small part of the protocol that tools
// need (initialize, ping, tools/list, tools/call), with no dependencies.
//
// Every change goes through the same edits as the web pages, so it shows
// up in Changes and can be undone there.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"chokominto/internal/store"
)

// Protocol versions this server speaks, newest first. A client asking
// for one of them gets it, otherwise the newest.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Server answers one agent on one stream.
type Server struct {
	DB       *store.DB
	UserID   int64
	Version  string
	ReadOnly bool   // offer only the tools that look, none that change
	Agent    string // who's asking, for the log
	Log      *slog.Logger

	out sync.Mutex
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes.
const (
	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
	invalidParams  = -32602
)

// Serve reads requests from r and writes answers to w until r ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if b := s.Answer(ctx, sc.Bytes()); b != nil {
			s.out.Lock()
			w.Write(append(b, '\n'))
			s.out.Unlock()
		}
	}
	return sc.Err()
}

// Answer handles one message and returns the answer, or nil for a
// notification, which gets none.
func (s *Server) Answer(ctx context.Context, msg []byte) []byte {
	var req request
	if err := json.Unmarshal(msg, &req); err != nil {
		return encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{parseError, "not JSON: " + err.Error()}})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if req.ID == nil {
			return nil
		}
		return encode(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{invalidRequest, "not a JSON-RPC 2.0 request"}})
	}
	result, rerr := s.handle(ctx, req)
	if req.ID == nil {
		return nil
	}
	resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = struct{}{}
	}
	return encode(resp)
}

func encode(resp response) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{-32603, err.Error()}})
	}
	return b
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "chokominto", "version": s.Version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		var list []map[string]any
		for _, t := range tools {
			if s.ReadOnly && !t.readOnly {
				continue
			}
			list = append(list, map[string]any{
				"name":        t.name,
				"description": t.description,
				"inputSchema": t.schema(),
				// Apps like Claude group tools by these and can ask the owner
				// before running the ones that delete.
				"annotations": map[string]any{"readOnlyHint": t.readOnly, "destructiveHint": t.destructive},
			})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{invalidParams, err.Error()}
		}
		i := slices.IndexFunc(tools, func(t tool) bool { return t.name == p.Name })
		if i < 0 || (s.ReadOnly && !tools[i].readOnly) {
			return nil, &rpcError{invalidParams, "no tool named " + p.Name}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		// Every change an agent makes joins its open task, so the owner can
		// undo a whole batch at once.
		out, err := tools[i].run(store.InAgentTask(ctx), s, p.Arguments)
		if err != nil {
			// Tool failures go back to the agent as results, so it can
			// read them and try something else.
			return toolResult(err.Error(), true), nil
		}
		if s.Log != nil && !tools[i].readOnly {
			s.Log.Info("agent changed something", "tool", p.Name, "agent", s.Agent)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(string(b), false), nil
	}
	if req.ID == nil {
		return nil, nil // notifications/initialized and the like
	}
	return nil, &rpcError{methodNotFound, fmt.Sprintf("no method %q", req.Method)}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}
