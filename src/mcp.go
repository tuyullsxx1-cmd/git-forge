// mcp.go: MCP (Model Context Protocol) over Streamable HTTP at POST /mcp.
// JSON-RPC 2.0. Supports protocol revision 2026-07-28 (server/discover) and
// the legacy 2025-11-25 handshake (initialize + notifications/initialized).
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

var mcpSessionCounter int64

// mcpToolsJSON is the fixed, deterministic tools/list payload (byte-identical
// on every call, as the suite requires).
const mcpToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository owned by the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Filter by state: open or closed"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request using a squash merge.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func (s *server) mcpHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeJSONRPCError(w, http.StatusUnauthorized, nil, -32001, "Requires authentication")
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Method Not Allowed"})
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) == 0 {
		s.writeJSONRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(data, &req); err != nil {
		s.writeJSONRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	method, _ := req["method"].(string)
	id, hasID := req["id"]
	// Notifications (no id) get no response body (HTTP 202).
	if !hasID || id == nil {
		if method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch method {
	case "server/discover":
		s.writeJSONRPCResult(w, id, map[string]any{
			"protocolVersion":  "2026-07-28",
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"capabilities":     map[string]any{"tools": map[string]any{}},
			"serverInfo":       map[string]any{"name": "forge", "version": "1.0.0"},
		})
	case "initialize":
		version := "2025-11-25"
		if params, ok := req["params"].(map[string]any); ok {
			if pv, ok := params["protocolVersion"].(string); ok && (pv == "2026-07-28" || pv == "2025-11-25") {
				version = pv
			}
		}
		w.Header().Set("Mcp-Session-Id", "forge-"+strconv.FormatInt(atomic.AddInt64(&mcpSessionCounter, 1), 10))
		s.writeJSONRPCResult(w, id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		})
	case "ping":
		s.writeJSONRPCResult(w, id, map[string]any{})
	case "tools/list":
		// Byte-identical on every call: write the fixed payload.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"jsonrpc":"2.0","id":`+jsonRaw(id)+`,"result":`+mcpToolsJSON+`}`)
	case "tools/call":
		params, _ := req["params"].(map[string]any)
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		s.mcpCallTool(w, id, name, args)
	default:
		s.writeJSONRPCError(w, http.StatusOK, id, -32601, "Method not found")
	}
}

func jsonRaw(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *server) writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	s.writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) writeJSONRPCError(w http.ResponseWriter, status int, id any, code int, message string) {
	s.writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// mcpCallTool runs one tool and answers with a content/isError result.
func (s *server) mcpCallTool(w http.ResponseWriter, id any, name string, args map[string]any) {
	// Match by suffix, case-insensitive (a server prefix is allowed).
	norm := strings.ToLower(name)
	if i := strings.LastIndexAny(norm, ".:"); i >= 0 {
		norm = norm[i+1:]
	}
	tool := ""
	for _, t := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"} {
		if norm == t || strings.HasSuffix(norm, t) {
			tool = t
			break
		}
	}
	if tool == "" {
		s.writeJSONRPCError(w, http.StatusOK, id, -32602, "unknown tool: "+name)
		return
	}
	text, isErr, httpErr := s.runMCPTool(tool, args)
	if httpErr != "" {
		s.writeJSONRPCError(w, http.StatusOK, id, -32602, httpErr)
		return
	}
	s.writeJSONRPCResult(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	})
}

func mcpArgString(args map[string]any, name string) (string, bool) {
	v, ok := args[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mcpArgInt(args map[string]any, name string) (int, bool) {
	v, ok := args[name]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func mcpMissing(arg string) (string, bool, string) {
	return `{"error":"missing required argument: ` + arg + `"}`, true, ""
}

// runMCPTool executes a tool against the shared store. Returns the text
// payload, whether it is an error, and a JSON-RPC error string ("" = none).
func (s *server) runMCPTool(tool string, args map[string]any) (string, bool, string) {
	switch tool {
	case "create_repo":
		name, ok := mcpArgString(args, "name")
		if !ok || name == "" {
			return mcpMissing("name")
		}
		s.store.mu.Lock()
		repo, err := s.store.createRepo(name, "", false)
		s.store.mu.Unlock()
		if err != nil {
			return `{"error":"` + err.Error() + `"}`, true, ""
		}
		return mustJSON(map[string]any{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name, "owner": map[string]any{"login": repo.Owner}, "default_branch": repo.DefaultBranch}), false, ""
	case "get_repo":
		owner, ok1 := mcpArgString(args, "owner")
		name, ok2 := mcpArgString(args, "repo")
		if !ok1 || owner == "" {
			return mcpMissing("owner")
		}
		if !ok2 || name == "" {
			return mcpMissing("repo")
		}
		s.store.mu.RLock()
		repo, ok := s.resolveRepo(owner, name)
		s.store.mu.RUnlock()
		if !ok {
			return `{"error":"Not Found"}`, true, ""
		}
		return mustJSON(s.repoJSON(repo)), false, ""
	case "create_issue":
		owner, ok1 := mcpArgString(args, "owner")
		name, ok2 := mcpArgString(args, "repo")
		title, ok3 := mcpArgString(args, "title")
		if !ok1 || owner == "" {
			return mcpMissing("owner")
		}
		if !ok2 || name == "" {
			return mcpMissing("repo")
		}
		if !ok3 || title == "" {
			return mcpMissing("title")
		}
		body, _ := mcpArgString(args, "body")
		s.store.mu.Lock()
		repo, ok := s.resolveRepo(owner, name)
		if !ok {
			s.store.mu.Unlock()
			return `{"error":"Not Found"}`, true, ""
		}
		it := s.store.createIssue(repo, title, body, nil)
		s.store.mu.Unlock()
		return mustJSON(map[string]any{"id": it.ID, "number": it.Number, "title": it.Title, "body": it.Body, "state": it.State}), false, ""
	case "list_issues":
		owner, ok1 := mcpArgString(args, "owner")
		name, ok2 := mcpArgString(args, "repo")
		if !ok1 || owner == "" {
			return mcpMissing("owner")
		}
		if !ok2 || name == "" {
			return mcpMissing("repo")
		}
		state, _ := mcpArgString(args, "state")
		s.store.mu.RLock()
		repo, ok := s.resolveRepo(owner, name)
		if !ok {
			s.store.mu.RUnlock()
			return `{"error":"Not Found"}`, true, ""
		}
		var out []any
		for _, it := range repo.Issues {
			if state != "" && it.State != strings.ToLower(state) {
				continue
			}
			out = append(out, s.issueJSON(repo, it))
		}
		s.store.mu.RUnlock()
		if out == nil {
			out = []any{}
		}
		return mustJSON(map[string]any{"issues": out}), false, ""
	case "create_pull_request":
		owner, ok1 := mcpArgString(args, "owner")
		name, ok2 := mcpArgString(args, "repo")
		title, ok3 := mcpArgString(args, "title")
		head, ok4 := mcpArgString(args, "head")
		base, ok5 := mcpArgString(args, "base")
		if !ok1 || owner == "" {
			return mcpMissing("owner")
		}
		if !ok2 || name == "" {
			return mcpMissing("repo")
		}
		if !ok3 || title == "" {
			return mcpMissing("title")
		}
		if !ok4 || head == "" {
			return mcpMissing("head")
		}
		if !ok5 || base == "" {
			return mcpMissing("base")
		}
		s.store.mu.Lock()
		repo, ok := s.resolveRepo(owner, name)
		if !ok {
			s.store.mu.Unlock()
			return `{"error":"Not Found"}`, true, ""
		}
		p, err := s.store.createPull(repo, title, "", head, base)
		s.store.mu.Unlock()
		if err != nil {
			return `{"error":"` + err.Error() + `"}`, true, ""
		}
		return mustJSON(map[string]any{"id": p.Number, "number": p.Number, "title": p.Title, "state": p.State, "head": map[string]any{"ref": p.HeadRef}, "base": map[string]any{"ref": p.BaseRef}}), false, ""
	case "merge_pull_request":
		owner, ok1 := mcpArgString(args, "owner")
		name, ok2 := mcpArgString(args, "repo")
		num, ok3 := mcpArgInt(args, "pull_number")
		if !ok1 || owner == "" {
			return mcpMissing("owner")
		}
		if !ok2 || name == "" {
			return mcpMissing("repo")
		}
		if !ok3 {
			return mcpMissing("pull_number")
		}
		s.store.mu.Lock()
		repo, ok := s.resolveRepo(owner, name)
		if !ok {
			s.store.mu.Unlock()
			return `{"error":"Not Found"}`, true, ""
		}
		p := repo.findPull(num)
		if p == nil {
			s.store.mu.Unlock()
			return `{"error":"Not Found"}`, true, ""
		}
		sha, _, err := s.store.mergePull(repo, p)
		s.store.mu.Unlock()
		if err != nil {
			return `{"error":"` + err.Error() + `"}`, true, ""
		}
		return mustJSON(map[string]any{"merged": true, "sha": sha, "message": "Pull Request successfully merged"}), false, ""
	}
	return `{"error":"unknown tool"}`, true, ""
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
