// rest.go: the GitHub REST v3 API under /api/v3.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

type server struct {
	store     *store
	token     string
	requestID uint64
}

func (s *server) nextRequestID() string {
	n := atomic.AddUint64(&s.requestID, 1)
	return "req-" + strconv.FormatUint(n, 16)
}

// writeJSON answers with a JSON body and the request-id header.
func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"internal error"}`)
		return
	}
	s.writeJSONBytes(w, status, body)
}

// writeJSONBytes answers with a pre-built JSON body (no marshal step).
func (s *server) writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
	w.WriteHeader(status)
	w.Write(body)
}

// jsonBuf builds JSON by hand: no maps, no reflection. Keys are emitted in
// alphabetical order (the order encoding/json uses for maps), so the bytes
// match the previous map-based output exactly. String values are escaped
// with json.Marshal on the string alone, which is encoding/json's own
// escaping and far cheaper than marshaling a whole value tree.
type jsonBuf struct {
	b []byte
}

func (j *jsonBuf) raw(s string) { j.b = append(j.b, s...) }
func (j *jsonBuf) str(s string) {
	esc, _ := json.Marshal(s)
	j.b = append(j.b, esc...)
}
func (j *jsonBuf) num(n int) { j.b = strconv.AppendInt(j.b, int64(n), 10) }

func (s *server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"message": msg})
}

// readBodyJSON parses a request body as a JSON object; an empty body is {}.
// A malformed body answers 400.
func (s *server) readBodyJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "cannot read body")
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, true
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		s.writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return nil, false
	}
	return v, true
}

func strField(v map[string]any, key string) string {
	if s, ok := v[key].(string); ok {
		return s
	}
	return ""
}

func boolField(v map[string]any, key string, def bool) bool {
	if b, ok := v[key].(bool); ok {
		return b
	}
	return def
}

// authorized checks the Bearer token. The trial's token is in
// MAKEFUN_SHOP_TOKEN; when it is empty any non-empty bearer is accepted.
func (s *server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	tok := strings.TrimSpace(auth[len("Bearer "):])
	if tok == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return tok == s.token
}

// paginate slices items per per_page/page query params. When per_page is
// absent the whole list is returned with no Link header.
func (s *server) paginate(w http.ResponseWriter, r *http.Request, total int) (start, end int, ok bool) {
	q := r.URL.Query()
	perPageStr := q.Get("per_page")
	if perPageStr == "" {
		return 0, total, true
	}
	perPage, err := strconv.Atoi(perPageStr)
	if err != nil || perPage < 1 {
		perPage = 30
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	if end < total {
		next := *r.URL
		nq := next.Query()
		nq.Set("per_page", strconv.Itoa(perPage))
		nq.Set("page", strconv.Itoa(page+1))
		next.RawQuery = nq.Encode()
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		abs := scheme + "://" + r.Host + next.RequestURI()
		w.Header().Set("Link", "<"+abs+">; rel=\"next\"")
	}
	return start, end, true
}

// --- JSON shapes ---

func (s *server) repoJSON(r *repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"node_id":        repoGlobalID(r.ID),
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner, "id": 1000, "type": "User"},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
		"html_url":       "",
		"url":            "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"created_at":     "2025-01-01T00:00:00Z",
		"updated_at":     "2025-01-01T00:00:00Z",
		"has_issues":     true,
		"has_projects":   false,
		"has_wiki":       false,
		"fork":           false,
		"archived":       false,
		"disabled":       false,
	}
}

func (s *server) issueJSONBytes(r *repo, it *issue) []byte {
	var j jsonBuf
	j.raw(`{"body":`)
	j.str(it.Body)
	j.raw(`,"comments":`)
	j.num(len(it.Comments))
	j.raw(`,"created_at":"2025-01-01T00:00:00Z","html_url":"","id":`)
	j.num(it.ID)
	j.raw(`,"labels":[`)
	for i, name := range it.Labels {
		if i > 0 {
			j.raw(",")
		}
		color := "ededed"
		if l, ok := r.Labels[name]; ok {
			color = l.Color
		}
		j.raw(`{"color":`)
		j.str(color)
		j.raw(`,"name":`)
		j.str(name)
		j.raw("}")
	}
	j.raw(`],"node_id":`)
	j.str(issueGlobalID(r.ID, it.Number))
	j.raw(`,"number":`)
	j.num(it.Number)
	j.raw(`,"state":`)
	j.str(it.State)
	j.raw(`,"title":`)
	j.str(it.Title)
	j.raw(`,"updated_at":"2025-01-01T00:00:00Z","user":{"id":1000,"login":`)
	j.str(s.store.viewer)
	j.raw(`,"type":"User"}}`)
	return j.b
}

func (s *server) issueJSON(r *repo, it *issue) map[string]any {
	labels := make([]any, 0, len(it.Labels))
	for _, name := range it.Labels {
		color := "ededed"
		if l, ok := r.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	return map[string]any{
		"id":         it.ID,
		"node_id":    issueGlobalID(r.ID, it.Number),
		"number":     it.Number,
		"title":      it.Title,
		"body":       it.Body,
		"state":      it.State,
		"labels":     labels,
		"user":       map[string]any{"login": s.store.viewer, "id": 1000, "type": "User"},
		"comments":   len(it.Comments),
		"created_at": "2025-01-01T00:00:00Z",
		"updated_at": "2025-01-01T00:00:00Z",
		"html_url":   "",
	}
}

func (s *server) pullJSON(r *repo, p *pull) map[string]any {
	var mergedAt any
	var mergeSHA any
	if p.Merged {
		mergedAt = p.MergedAt
		mergeSHA = p.MergeCommitSHA
	}
	return map[string]any{
		"id":               p.Number,
		"node_id":          pullGlobalID(r.ID, p.Number),
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"merged":           p.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head": map[string]any{
			"ref":   p.HeadRef,
			"sha":   p.HeadSHA,
			"label": r.Owner + ":" + p.HeadRef,
		},
		"base": map[string]any{
			"ref":   p.BaseRef,
			"sha":   p.BaseSHA,
			"label": r.Owner + ":" + p.BaseRef,
		},
		"user":       map[string]any{"login": s.store.viewer, "id": 1000, "type": "User"},
		"created_at": "2025-01-01T00:00:00Z",
		"updated_at": "2025-01-01T00:00:00Z",
		"html_url":   "",
	}
}

// parseCommitMessage extracts the message and author identity of a commit.
func parseCommitMessage(data []byte) (message, authorName, authorEmail string) {
	head := data
	body := []byte{}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
		body = data[i+2:]
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("author ")) {
			rest := string(line[len("author "):])
			// "Name <email> epoch +0000"
			if lt := strings.LastIndex(rest, "<"); lt >= 0 {
				authorName = strings.TrimSpace(rest[:lt])
				if gt := strings.Index(rest[lt:], ">"); gt >= 0 {
					authorEmail = rest[lt+1 : lt+gt]
				}
			}
		}
	}
	return string(body), authorName, authorEmail
}

func (s *server) commitJSON(r *repo, id [20]byte) map[string]any {
	o, ok := r.Objects[id]
	if !ok {
		return nil
	}
	message, name, email := parseCommitMessage(o.data)
	_, parents, _ := parseCommitHeader(o.data)
	parentList := make([]any, 0, len(parents))
	for _, p := range parents {
		parentList = append(parentList, map[string]any{"sha": oidHex(p)})
	}
	return map[string]any{
		"sha": oidHex(id),
		"commit": map[string]any{
			"message": message,
			"author":  map[string]any{"name": name, "email": email},
			"committer": map[string]any{"name": name, "email": email},
		},
		"author":    map[string]any{"login": s.store.viewer},
		"committer": map[string]any{"login": s.store.viewer},
		"parents":   parentList,
	}
}

// resolveRepo finds a repo for an owner/name pair: another owner is 404,
// a missing repo is 404.
func (s *server) resolveRepo(owner, name string) (*repo, bool) {
	if owner != s.store.viewer {
		return nil, false
	}
	r := s.store.getRepo(owner, name)
	if r == nil {
		return nil, false
	}
	return r, true
}

// --- router ---

func (s *server) restHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeErr(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path == "/" {
		seg = []string{}
	}
	method := r.Method

	// API root.
	if len(seg) == 0 {
		if method == http.MethodGet {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			s.writeJSON(w, http.StatusOK, map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			})
			return
		}
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}

	switch seg[0] {
	case "meta":
		if method == http.MethodGet && len(seg) == 1 {
			s.writeJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
			return
		}
	case "user":
		if len(seg) == 1 && method == http.MethodGet {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"login": s.store.viewer, "id": 1000, "node_id": "U_adaiversen",
				"name": viewerName, "email": viewerEmail, "type": "User",
			})
			return
		}
		if len(seg) == 2 && seg[1] == "repos" {
			s.handleUserRepos(w, r)
			return
		}
	case "repos":
		if len(seg) >= 3 {
			s.handleRepoRoutes(w, r, seg[1], seg[2], seg[3:])
			return
		}
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *server) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.store.mu.RLock()
		repos := s.store.listViewerRepos()
		s.store.mu.RUnlock()
		start, end, _ := s.paginate(w, r, len(repos))
		items := make([]any, 0, end-start)
		for _, repo := range repos[start:end] {
			items = append(items, s.repoJSON(repo))
		}
		s.writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		body, ok := s.readBodyJSON(w, r)
		if !ok {
			return
		}
		name := strField(body, "name")
		if name == "" {
			s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		s.store.mu.Lock()
		repo, err := s.store.createRepo(name, strField(body, "description"), boolField(body, "private", false))
		s.store.mu.Unlock()
		if err != nil {
			s.writeErr(w, http.StatusUnprocessableEntity, "Repository already exists")
			return
		}
		s.writeJSON(w, http.StatusCreated, s.repoJSON(repo))
	default:
		s.writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *server) handleRepoRoutes(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	s.store.mu.RLock()
	repo, found := s.resolveRepo(owner, name)
	s.store.mu.RUnlock()
	if !found {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.writeJSON(w, http.StatusOK, s.repoJSON(repo))
		case http.MethodPatch:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			s.store.mu.Lock()
			if d, ok := body["description"].(string); ok {
				repo.Description = d
			}
			s.store.mu.Unlock()
			s.writeJSON(w, http.StatusOK, s.repoJSON(repo))
		case http.MethodDelete:
			s.store.mu.Lock()
			s.store.deleteRepo(repo)
			s.store.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
			w.WriteHeader(http.StatusNoContent)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		s.handleBranches(w, r, repo, rest[1:])
	case "commits":
		s.handleCommits(w, r, repo)
	case "contents":
		s.handleContents(w, r, repo, rest[1:])
	case "labels":
		s.handleLabels(w, r, repo)
	case "issues":
		s.handleIssues(w, r, repo, rest[1:])
	case "pulls":
		s.handlePulls(w, r, repo, rest[1:])
	case "git":
		s.handleGitRefs(w, r, repo, rest[1:])
	default:
		s.writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *server) handleBranches(w http.ResponseWriter, r *http.Request, repo *repo, rest []string) {
	if r.Method == http.MethodGet && len(rest) == 0 {
		s.store.mu.RLock()
		var names []string
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sort.Strings(names)
		items := make([]any, 0, len(names))
		for _, name := range names {
			tip := repo.Refs["refs/heads/"+name]
			items = append(items, map[string]any{
				"name":      name,
				"commit":    map[string]any{"sha": oidHex(tip)},
				"protected": false,
			})
		}
		s.store.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, items)
		return
	}
	if r.Method == http.MethodGet && len(rest) == 1 {
		s.store.mu.RLock()
		tip, ok := repo.Refs["refs/heads/"+rest[0]]
		s.store.mu.RUnlock()
		if !ok {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"name":      rest[0],
			"commit":    map[string]any{"sha": oidHex(tip)},
			"protected": false,
		})
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, repo *repo) {
	if r.Method != http.MethodGet {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	branch := r.URL.Query().Get("sha")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	s.store.mu.RLock()
	tip, ok := repo.branchTip(branch)
	if !ok {
		// Maybe a raw commit sha.
		if id, ok2 := parseOIDHex(branch); ok2 {
			if _, exists := repo.Objects[id]; exists {
				tip, ok = id, true
			}
		}
	}
	if !ok {
		s.store.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, []any{})
		return
	}
	commits := repo.commitsFrom(tip)
	items := make([]any, 0, len(commits))
	for _, id := range commits {
		if cj := s.commitJSON(repo, id); cj != nil {
			items = append(items, cj)
		}
	}
	s.store.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, items)
}

// resolveBranchTip resolves a branch name (or commit sha) to a tip commit.
func (s *server) resolveBranchTip(repo *repo, ref string) ([20]byte, bool) {
	if ref == "" {
		return repo.branchTip(repo.DefaultBranch)
	}
	if tip, ok := repo.branchTip(ref); ok {
		return tip, true
	}
	if id, ok := parseOIDHex(ref); ok {
		if o, exists := repo.Objects[id]; exists && o.typ == "commit" {
			return id, true
		}
	}
	return [20]byte{}, false
}

func (s *server) handleContents(w http.ResponseWriter, r *http.Request, repo *repo, rest []string) {
	path := strings.Join(rest, "/")
	// URL-decode each segment (paths arrive percent-encoded).
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	switch r.Method {
	case http.MethodGet:
		s.getContents(w, r, repo, path)
	case http.MethodPut:
		s.putContents(w, r, repo, path)
	case http.MethodDelete:
		s.deleteContents(w, r, repo, path)
	default:
		s.writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *server) getContents(w http.ResponseWriter, r *http.Request, repo *repo, path string) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	tip, ok := s.resolveBranchTip(repo, r.URL.Query().Get("ref"))
	if !ok {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tree, ok := repo.treeOf(tip)
	if !ok {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if path == "" {
		s.writeJSON(w, http.StatusOK, s.dirEntries(repo, tree, ""))
		return
	}
	flat, err := repo.flattenTree(tree)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if blobID, ok := flat[path]; ok {
		o := repo.Objects[blobID]
		s.writeJSON(w, http.StatusOK, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(o.data),
			"name":     baseName(path),
			"path":     path,
			"sha":      oidHex(blobID),
			"content":  base64.StdEncoding.EncodeToString(o.data),
			"url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
		})
		return
	}
	// Directory?
	prefix := path + "/"
	var entries []any
	seen := map[string]bool{}
	for p, blobID := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		first := strings.SplitN(rest, "/", 2)[0]
		if seen[first] {
			continue
		}
		seen[first] = true
		if strings.Contains(rest, "/") {
			// Subdirectory: find its tree oid.
			entries = append(entries, map[string]any{
				"type": "dir",
				"name": first,
				"path": path + "/" + first,
				"sha":  oidHex(s.findSubtree(repo, tree, path+"/"+first)),
			})
		} else {
			o := repo.Objects[blobID]
			entries = append(entries, map[string]any{
				"type": "file",
				"name": first,
				"path": p,
				"sha":  oidHex(blobID),
				"size": len(o.data),
			})
		}
	}
	if len(entries) == 0 {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].(map[string]any)["name"].(string) < entries[j].(map[string]any)["name"].(string)
	})
	s.writeJSON(w, http.StatusOK, entries)
}

func (s *server) findSubtree(repo *repo, treeID [20]byte, path string) [20]byte {
	parts := strings.Split(path, "/")
	cur := treeID
	for _, part := range parts {
		o, ok := repo.Objects[cur]
		if !ok || o.typ != "tree" {
			return [20]byte{}
		}
		entries, err := parseTree(o.data)
		if err != nil {
			return [20]byte{}
		}
		found := false
		for _, e := range entries {
			if e.name == part && e.mode == "40000" {
				cur = e.oid
				found = true
				break
			}
		}
		if !found {
			return [20]byte{}
		}
	}
	return cur
}

func (s *server) dirEntries(repo *repo, treeID [20]byte, prefix string) []any {
	o, ok := repo.Objects[treeID]
	if !ok || o.typ != "tree" {
		return []any{}
	}
	entries, err := parseTree(o.data)
	if err != nil {
		return []any{}
	}
	var out []any
	for _, e := range entries {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			out = append(out, map[string]any{"type": "dir", "name": e.name, "path": path, "sha": oidHex(e.oid)})
		} else {
			blob := repo.Objects[e.oid]
			out = append(out, map[string]any{"type": "file", "name": e.name, "path": path, "sha": oidHex(e.oid), "size": len(blob.data)})
		}
	}
	return out
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (s *server) putContents(w http.ResponseWriter, r *http.Request, repo *repo, path string) {
	body, ok := s.readBodyJSON(w, r)
	if !ok {
		return
	}
	contentB64, _ := body["content"].(string)
	if contentB64 == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "content is required")
		return
	}
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "content is not valid base64")
		return
	}
	message := strField(body, "message")
	if message == "" {
		message = "Update " + path
	}
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := strField(body, "sha")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tip, hasTip := repo.branchTip(branch)
	files := map[string][]byte{}
	if hasTip {
		tree, ok := repo.treeOf(tip)
		if ok {
			flat, err := repo.flattenTree(tree)
			if err == nil {
				for p, blobID := range flat {
					if o, ok := repo.Objects[blobID]; ok {
						files[p] = o.data
					}
				}
			}
		}
	}
	existing, exists := files[path]
	isUpdate := exists
	if isUpdate {
		currentBlob := hashObject("blob", existing)
		if sha == "" || !strings.EqualFold(sha, oidHex(currentBlob)) {
			s.writeErr(w, http.StatusConflict, "sha does not match the current blob")
			return
		}
	}
	files[path] = content
	tree := repo.buildTreeFromFiles(files)
	blobID := hashObject("blob", content)
	var parents [][20]byte
	if hasTip {
		parents = [][20]byte{tip}
	}
	commitID := s.store.serverCommit(repo, branch, tree, parents, message)
	status := http.StatusCreated
	if isUpdate {
		status = http.StatusOK
	}
	s.writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name":     baseName(path),
			"path":     path,
			"sha":      oidHex(blobID),
			"size":     len(content),
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(content),
		},
		"commit": map[string]any{"sha": oidHex(commitID), "message": message},
	})
}

func (s *server) deleteContents(w http.ResponseWriter, r *http.Request, repo *repo, path string) {
	body, ok := s.readBodyJSON(w, r)
	if !ok {
		return
	}
	message := strField(body, "message")
	if message == "" {
		message = "Delete " + path
	}
	sha := strField(body, "sha")
	branch := strField(body, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tip, hasTip := repo.branchTip(branch)
	if !hasTip {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tree, ok := repo.treeOf(tip)
	if !ok {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	flat, err := repo.flattenTree(tree)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	blobID, exists := flat[path]
	if !exists {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if sha != "" && !strings.EqualFold(sha, oidHex(blobID)) {
		s.writeErr(w, http.StatusConflict, "sha does not match the current blob")
		return
	}
	files := map[string][]byte{}
	for p, b := range flat {
		if p == path {
			continue
		}
		if o, ok := repo.Objects[b]; ok {
			files[p] = o.data
		}
	}
	newTree := repo.buildTreeFromFiles(files)
	commitID := s.store.serverCommit(repo, branch, newTree, [][20]byte{tip}, message)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]any{"sha": oidHex(commitID), "message": message},
	})
}

func (s *server) handleLabels(w http.ResponseWriter, r *http.Request, repo *repo) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	body, ok := s.readBodyJSON(w, r)
	if !ok {
		return
	}
	name := strField(body, "name")
	if name == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	color := strField(body, "color")
	if color == "" {
		color = "ededed"
	}
	s.store.mu.Lock()
	repo.Labels[name] = &label{Name: name, Color: color}
	s.store.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, map[string]any{"name": name, "color": color, "default": false})
}

func (s *server) handleIssues(w http.ResponseWriter, r *http.Request, repo *repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listIssues(w, r, repo)
		case http.MethodPost:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			var labels []string
			if raw, ok := body["labels"].([]any); ok {
				for _, v := range raw {
					if name, ok := v.(string); ok {
						labels = append(labels, name)
					}
				}
			}
			s.store.mu.Lock()
			for _, name := range labels {
				if _, ok := repo.Labels[name]; !ok {
					s.store.mu.Unlock()
					s.writeErr(w, http.StatusUnprocessableEntity, "label '"+name+"' does not exist")
					return
				}
			}
			it := s.store.createIssue(repo, strField(body, "title"), strField(body, "body"), labels)
			s.store.mu.Unlock()
			s.writeJSONBytes(w, http.StatusCreated, s.issueJSONBytes(repo, it))
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			it := repo.findIssue(number)
			s.store.mu.RUnlock()
			if it == nil {
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			s.writeJSONBytes(w, http.StatusOK, s.issueJSONBytes(repo, it))
		case http.MethodPatch:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			s.store.mu.Lock()
			it := repo.findIssue(number)
			if it == nil {
				s.store.mu.Unlock()
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if st, ok := body["state"].(string); ok && (st == "open" || st == "closed") {
				it.State = st
			}
			if t, ok := body["title"].(string); ok {
				it.Title = t
			}
			if b, ok := body["body"].(string); ok {
				it.Body = b
			}
			s.store.mu.Unlock()
			s.writeJSONBytes(w, http.StatusOK, s.issueJSONBytes(repo, it))
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			it := repo.findIssue(number)
			s.store.mu.RUnlock()
			if it == nil {
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			items := make([]any, 0, len(it.Comments))
			for _, c := range it.Comments {
				items = append(items, map[string]any{
					"id":   c.ID,
					"body": c.Body,
					"user": map[string]any{"login": s.store.viewer},
				})
			}
			s.writeJSON(w, http.StatusOK, items)
		case http.MethodPost:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			s.store.mu.Lock()
			it := repo.findIssue(number)
			if it == nil {
				s.store.mu.Unlock()
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			id := repo.NextCommentID
			repo.NextCommentID++
			c := &comment{ID: id, Body: strField(body, "body")}
			it.Comments = append(it.Comments, c)
			s.store.mu.Unlock()
			s.writeJSON(w, http.StatusCreated, map[string]any{
				"id":   c.ID,
				"body": c.Body,
				"user": map[string]any{"login": s.store.viewer},
			})
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request, repo *repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortMode := q.Get("sort")
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	s.store.mu.RLock()
	var items []*issue
	for _, it := range repo.Issues {
		if state == "all" || it.State == state {
			items = append(items, it)
		}
	}
	s.store.mu.RUnlock()
	if sortMode == "created" || sortMode == "" {
		sort.Slice(items, func(i, j int) bool {
			if direction == "asc" {
				return items[i].Seq < items[j].Seq
			}
			return items[i].Seq > items[j].Seq
		})
	}
	start, end, _ := s.paginate(w, r, len(items))
	out := make([]any, 0, end-start)
	for _, it := range items[start:end] {
		out = append(out, s.issueJSON(repo, it))
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *server) handlePulls(w http.ResponseWriter, r *http.Request, repo *repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listPulls(w, r, repo)
		case http.MethodPost:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			head := strField(body, "head")
			base := strField(body, "base")
			if base == "" {
				base = repo.DefaultBranch
			}
			s.store.mu.Lock()
			p, err := s.store.createPull(repo, strField(body, "title"), strField(body, "body"), head, base)
			s.store.mu.Unlock()
			if err != nil {
				s.writeErr(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			s.writeJSON(w, http.StatusCreated, s.pullJSON(repo, p))
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method != http.MethodGet {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.store.mu.RLock()
		p := repo.findPull(number)
		s.store.mu.RUnlock()
		if p == nil {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeJSON(w, http.StatusOK, s.pullJSON(repo, p))
		return
	}
	if len(rest) == 2 && rest[1] == "files" {
		if r.Method != http.MethodGet {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.store.mu.RLock()
		p := repo.findPull(number)
		s.store.mu.RUnlock()
		if p == nil {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeJSON(w, http.StatusOK, s.pullFiles(repo, p))
		return
	}
	if len(rest) == 2 && rest[1] == "merge" {
		if r.Method != http.MethodPut {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.store.mu.Lock()
		p := repo.findPull(number)
		if p == nil {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		mergeID, conflict, err := s.store.mergePull(repo, p)
		s.store.mu.Unlock()
		if err != nil {
			if conflict {
				s.writeErr(w, http.StatusMethodNotAllowed, err.Error())
			} else {
				s.writeErr(w, http.StatusMethodNotAllowed, err.Error())
			}
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"sha":     oidHex(mergeID),
			"merged":  true,
			"message": "Pull request successfully merged",
		})
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *server) listPulls(w http.ResponseWriter, r *http.Request, repo *repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	s.store.mu.RLock()
	var items []*pull
	for _, p := range repo.Pulls {
		if state == "all" || p.State == state {
			items = append(items, p)
		}
	}
	s.store.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].Seq > items[j].Seq })
	start, end, _ := s.paginate(w, r, len(items))
	out := make([]any, 0, end-start)
	for _, p := range items[start:end] {
		out = append(out, s.pullJSON(repo, p))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// pullFiles diffs the pull's base and head tips.
func (s *server) pullFiles(repo *repo, p *pull) []any {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	headTip, ok1 := repo.branchTip(p.HeadRef)
	baseTip, ok2 := repo.branchTip(p.BaseRef)
	if !ok1 || !ok2 {
		// Fall back to the recorded shas.
		headTip, _ = parseOIDHex(p.HeadSHA)
		baseTip, _ = parseOIDHex(p.BaseSHA)
	}
	headTree, _ := repo.treeOf(headTip)
	baseTree, _ := repo.treeOf(baseTip)
	changed, detail := repo.diffTrees(baseTree, headTree)
	out := make([]any, 0, len(changed))
	for _, path := range changed {
		pair := detail[path]
		var status string
		var additions, deletions int
		baseHas := pair[0] != zeroOID
		headHas := pair[1] != zeroOID
		switch {
		case !baseHas && headHas:
			status = "added"
			additions = countLines(repo, pair[1])
		case baseHas && !headHas:
			status = "removed"
			deletions = countLines(repo, pair[0])
		default:
			status = "modified"
			additions, deletions = diffLines(repo, pair[0], pair[1])
		}
		if additions == 0 && deletions == 0 {
			additions = 1
		}
		out = append(out, map[string]any{
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
		})
	}
	return out
}

func countLines(repo *repo, blobID [20]byte) int {
	o, ok := repo.Objects[blobID]
	if !ok {
		return 0
	}
	return bytes.Count(o.data, []byte("\n")) + 1
}

// diffLines counts added/removed lines between two blobs (multiset diff).
func diffLines(repo *repo, a, b [20]byte) (int, int) {
	oa, okA := repo.Objects[a]
	ob, okB := repo.Objects[b]
	if !okA || !okB {
		return 1, 0
	}
	counts := map[string]int{}
	for _, line := range bytes.Split(oa.data, []byte("\n")) {
		counts[string(line)]++
	}
	additions := 0
	for _, line := range bytes.Split(ob.data, []byte("\n")) {
		if counts[string(line)] > 0 {
			counts[string(line)]--
		} else {
			additions++
		}
	}
	deletions := 0
	for _, c := range counts {
		deletions += c
	}
	return additions, deletions
}

func (s *server) handleGitRefs(w http.ResponseWriter, r *http.Request, repo *repo, rest []string) {
	if len(rest) == 0 {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if rest[0] != "refs" {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			var names []string
			for name := range repo.Refs {
				names = append(names, name)
			}
			sort.Strings(names)
			items := make([]any, 0, len(names))
			for _, name := range names {
				oid := repo.Refs[name]
				typ := "commit"
				if o, ok := repo.Objects[oid]; ok {
					typ = o.typ
				}
				items = append(items, map[string]any{
					"ref":    name,
					"object": map[string]any{"sha": oidHex(oid), "type": typ},
				})
			}
			s.store.mu.RUnlock()
			s.writeJSON(w, http.StatusOK, items)
		case http.MethodPost:
			body, ok := s.readBodyJSON(w, r)
			if !ok {
				return
			}
			ref := strField(body, "ref")
			sha := strField(body, "sha")
			if !strings.HasPrefix(ref, "refs/") || sha == "" {
				s.writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
				return
			}
			id, ok := parseOIDHex(sha)
			if !ok {
				s.writeErr(w, http.StatusUnprocessableEntity, "sha is not a valid object id")
				return
			}
			s.store.mu.Lock()
			if _, exists := repo.Refs[ref]; exists {
				s.store.mu.Unlock()
				s.writeErr(w, http.StatusUnprocessableEntity, "ref already exists")
				return
			}
			o, exists := repo.Objects[id]
			if !exists {
				s.store.mu.Unlock()
				s.writeErr(w, http.StatusUnprocessableEntity, "object does not exist")
				return
			}
			repo.Refs[ref] = id
			repo.packCache = map[string]cachedPack{}
			s.store.mu.Unlock()
			s.writeJSON(w, http.StatusCreated, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": oidHex(id), "type": o.typ},
			})
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	// GET /git/refs/<full refname>
	if r.Method == http.MethodGet {
		ref := "refs/" + strings.Join(rest[1:], "/")
		s.store.mu.RLock()
		oid, ok := repo.Refs[ref]
		typ := "commit"
		if ok {
			if o, exists := repo.Objects[oid]; exists {
				typ = o.typ
			}
		}
		s.store.mu.RUnlock()
		if !ok {
			s.writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": oidHex(oid), "type": typ},
		})
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}
