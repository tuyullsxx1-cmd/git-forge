// graphql.go: a small GraphQL executor for POST /api/graphql, covering exactly
// the documents the suite sends (gh and github-mcp-server shapes). Unknown
// fields answer HTTP 200 with an errors array naming the field.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// --- lexer ---

type gqlToken struct {
	kind string // "name","string","int","punct","..."
	val  string
}

func lexGraphQL(src string) ([]gqlToken, error) {
	var out []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					sb.WriteByte(src[j])
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, gqlToken{"string", sb.String()})
			i = j + 1
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			out = append(out, gqlToken{"...", "..."})
			i += 3
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			out = append(out, gqlToken{"var", src[i+1 : j]})
			i = j
		case c >= '0' && c <= '9' || c == '-':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			out = append(out, gqlToken{"int", src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			out = append(out, gqlToken{"name", src[i:j]})
			i = j
		default:
			out = append(out, gqlToken{"punct", string(c)})
			i++
		}
	}
	return out, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// --- AST ---

type gqlValue struct {
	kind string // "var","string","int","bool","null","enum","list","object"
	str  string
	num  int64
	b    bool
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []*gqlField
	frag  string // inline fragment type condition, "" when a plain field
}

type gqlOp struct {
	kind    string // "query" | "mutation"
	name    string
	varDefs map[string]string // var name -> type name (unused beyond parsing)
	sel     []*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() (gqlToken, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return gqlToken{}, false
}

func (p *gqlParser) next() (gqlToken, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

func (p *gqlParser) expectPunct(v string) error {
	t, ok := p.next()
	if !ok || t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %q", v)
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]*gqlOp, error) {
	var ops []*gqlOp
	for {
		t, ok := p.peek()
		if !ok {
			break
		}
		if t.kind == "punct" && t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &gqlOp{kind: "query", sel: sel})
			continue
		}
		if t.kind != "name" {
			return nil, fmt.Errorf("unexpected token %v", t)
		}
		op := &gqlOp{kind: t.val}
		p.next()
		if t, ok := p.peek(); ok && t.kind == "name" {
			op.name = t.val
			p.next()
		}
		// Variable definitions (...) — skip balanced parens.
		if t, ok := p.peek(); ok && t.kind == "punct" && t.val == "(" {
			if err := p.skipParens(); err != nil {
				return nil, err
			}
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		op.sel = sel
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) skipParens() error {
	depth := 0
	for {
		t, ok := p.next()
		if !ok {
			return fmt.Errorf("unbalanced parens")
		}
		if t.kind == "punct" && t.val == "(" {
			depth++
		}
		if t.kind == "punct" && t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (*gqlField, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end in field")
	}
	// Inline fragment: ... on Type { ... }
	if t.kind == "..." {
		p.next()
		frag := ""
		if t, ok := p.peek(); ok && t.kind == "name" && t.val == "on" {
			p.next()
			if t, ok := p.next(); ok && t.kind == "name" {
				frag = t.val
			}
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlField{frag: frag, sel: sel}, nil
	}
	if t.kind != "name" {
		return nil, fmt.Errorf("expected field name, got %v", t)
	}
	f := &gqlField{name: t.val}
	p.next()
	// Alias: name : name
	if t, ok := p.peek(); ok && t.kind == "punct" && t.val == ":" {
		p.next()
		nt, ok := p.next()
		if !ok || nt.kind != "name" {
			return nil, fmt.Errorf("bad alias")
		}
		f.alias = f.name
		f.name = nt.val
	}
	// Arguments
	if t, ok := p.peek(); ok && t.kind == "punct" && t.val == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	// Nested selection set
	if t, ok := p.peek(); ok && t.kind == "punct" && t.val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]*gqlValue{}
	for {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated arguments")
		}
		if t.kind == "punct" && t.val == ")" {
			p.next()
			return args, nil
		}
		if t.kind != "name" {
			return nil, fmt.Errorf("expected argument name, got %v", t)
		}
		name := t.val
		p.next()
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end in value")
	}
	switch t.kind {
	case "var":
		return &gqlValue{kind: "var", str: t.val}, nil
	case "string":
		return &gqlValue{kind: "string", str: t.val}, nil
	case "int":
		n, _ := strconv.ParseInt(t.val, 10, 64)
		return &gqlValue{kind: "int", num: n}, nil
	case "name":
		switch t.val {
		case "true":
			return &gqlValue{kind: "bool", b: true}, nil
		case "false":
			return &gqlValue{kind: "bool", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: t.val}, nil
		}
	case "punct":
		switch t.val {
		case "[":
			v := &gqlValue{kind: "list"}
			for {
				nt, ok := p.peek()
				if !ok {
					return nil, fmt.Errorf("unterminated list")
				}
				if nt.kind == "punct" && nt.val == "]" {
					p.next()
					return v, nil
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for {
				nt, ok := p.peek()
				if !ok {
					return nil, fmt.Errorf("unterminated object")
				}
				if nt.kind == "punct" && nt.val == "}" {
					p.next()
					return v, nil
				}
				if nt.kind != "name" {
					return nil, fmt.Errorf("expected object key, got %v", nt)
				}
				key := nt.val
				p.next()
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[key] = item
			}
		}
	}
	return nil, fmt.Errorf("unexpected value token %v", t)
}

// --- value resolution ---

// gqlCtx carries the request's variables and collected errors.
type gqlCtx struct {
	vars   map[string]any
	errors []map[string]any
}

func (c *gqlCtx) argString(args map[string]*gqlValue, name string) string {
	v, ok := args[name]
	if !ok {
		return ""
	}
	return c.valueString(v)
}

func (c *gqlCtx) argInt(args map[string]*gqlValue, name string, def int) int {
	v, ok := args[name]
	if !ok {
		return def
	}
	switch v.kind {
	case "int":
		return int(v.num)
	case "var":
		if n, ok := c.vars[v.str].(float64); ok {
			return int(n)
		}
		if n, ok := c.vars[v.str].(int); ok {
			return n
		}
	}
	return def
}

func (c *gqlCtx) valueString(v *gqlValue) string {
	switch v.kind {
	case "string", "enum":
		return v.str
	case "int":
		return strconv.FormatInt(v.num, 10)
	case "bool":
		if v.b {
			return "true"
		}
		return "false"
	case "var":
		switch val := c.vars[v.str].(type) {
		case string:
			return val
		case float64:
			return strconv.FormatInt(int64(val), 10)
		case nil:
			return ""
		}
	}
	return ""
}

func (c *gqlCtx) valueAny(v *gqlValue) any {
	switch v.kind {
	case "string", "enum":
		return v.str
	case "int":
		return v.num
	case "bool":
		return v.b
	case "null":
		return nil
	case "var":
		return c.vars[v.str]
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, c.valueAny(item))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = c.valueAny(item)
		}
		return out
	}
	return nil
}

func (c *gqlCtx) argList(args map[string]*gqlValue, name string) []string {
	v, ok := args[name]
	if !ok {
		return nil
	}
	anyVal := c.valueAny(v)
	if list, ok := anyVal.([]any); ok {
		var out []string
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c *gqlCtx) argObject(args map[string]*gqlValue, name string) map[string]any {
	v, ok := args[name]
	if !ok {
		return nil
	}
	if m, ok := c.valueAny(v).(map[string]any); ok {
		return m
	}
	return nil
}

// --- executor ---

type gqlNode struct {
	kind string // "query","repository","issue","pullrequest","user","type","conn","pageinfo","comment","node"
	repo *repo
	iss  *issue
	pr   *pull
	typ  string // for kind "type": the type name
	conn *gqlConn
	pi   *gqlPageInfo
	cmt  *comment
}

type gqlConn struct {
	nodes     []gqlNode
	hasNext   bool
	endCursor string
}

type gqlPageInfo struct {
	hasNext   bool
	endCursor string
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
}

var gqlTypeEnums = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
}

func (s *server) executeGraphQL(op *gqlOp, vars map[string]any) map[string]any {
	ctx := &gqlCtx{vars: vars}
	data := map[string]any{}
	root := &gqlNode{kind: "query"}
	for _, f := range op.sel {
		s.resolveField(ctx, root, f, data)
	}
	out := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		out["errors"] = ctx.errors
	}
	return out
}

func (s *server) resolveField(ctx *gqlCtx, node *gqlNode, f *gqlField, into map[string]any) {
	key := f.name
	if f.alias != "" {
		key = f.alias
	}
	if node.kind == "null" {
		into[key] = nil
		return
	}
	if f.frag != "" {
		// Inline fragment: apply when the type matches.
		if !fragmentMatches(node, f.frag) {
			return
		}
		for _, sub := range f.sel {
			s.resolveField(ctx, node, sub, into)
		}
		return
	}
	val, ok := s.resolveNodeField(ctx, node, f)
	if !ok {
		ctx.errors = append(ctx.errors, map[string]any{
			"message": fmt.Sprintf("Cannot query field %q on type %q", f.name, typeNameOf(node)),
		})
		into[key] = nil
		return
	}
	into[key] = s.selectNode(ctx, val, f.sel)
}

func fragmentMatches(node *gqlNode, frag string) bool {
	switch frag {
	case "Repository":
		return node.kind == "repository" || node.kind == "node"
	case "Issue":
		return node.kind == "issue"
	case "PullRequest":
		return node.kind == "pullrequest"
	case "Node":
		return true
	}
	return false
}

func typeNameOf(node *gqlNode) string {
	switch node.kind {
	case "query":
		return "Query"
	case "repository":
		return "Repository"
	case "issue":
		return "Issue"
	case "pullrequest":
		return "PullRequest"
	case "user":
		return "User"
	case "type":
		return "__Type"
	case "conn":
		return "Connection"
	case "pageinfo":
		return "PageInfo"
	case "comment":
		return "IssueComment"
	case "payload-issue":
		return "CreateIssuePayload"
	case "payload-pr":
		return "PullRequestPayload"
	case "payload-comment":
		return "AddCommentPayload"
	}
	return "Node"
}

// resolveNodeField resolves one field on a node; ok=false means unknown field.
func (s *server) resolveNodeField(ctx *gqlCtx, node *gqlNode, f *gqlField) (*gqlNode, bool) {
	switch node.kind {
	case "query":
		return s.resolveQueryField(ctx, f)
	case "repository":
		return s.resolveRepoField(ctx, node, f)
	case "issue":
		return s.resolveIssueField(ctx, node, f)
	case "pullrequest":
		return s.resolvePullField(ctx, node, f)
	case "user":
		if f.name == "login" {
			return &gqlNode{kind: "scalar", typ: s.store.viewer}, true
		}
		return nil, false
	case "type":
		return s.resolveTypeField(ctx, node, f)
	case "conn":
		switch f.name {
		case "nodes":
			return &gqlNode{kind: "connnodes", conn: node.conn}, true
		case "pageInfo":
			return &gqlNode{kind: "pageinfo", pi: &gqlPageInfo{hasNext: node.conn.hasNext, endCursor: node.conn.endCursor}}, true
		}
		return nil, false
	case "connnodes":
		return nil, false
	case "pageinfo":
		switch f.name {
		case "hasNextPage":
			return &gqlNode{kind: "scalar", typ: boolStr(node.pi.hasNext)}, true
		case "endCursor":
			return &gqlNode{kind: "scalar", typ: node.pi.endCursor}, true
		}
		return nil, false
	case "comment":
		switch f.name {
		case "id":
			return &gqlNode{kind: "scalar", typ: commentGlobalID(node.repo.ID, node.iss.Number, node.cmt.ID)}, true
		case "body":
			return &gqlNode{kind: "scalar", typ: node.cmt.Body}, true
		}
		return nil, false
	case "payload-issue":
		if f.name == "issue" {
			return &gqlNode{kind: "issue", repo: node.repo, iss: node.iss}, true
		}
		return nil, false
	case "payload-pr":
		if f.name == "pullRequest" {
			return &gqlNode{kind: "pullrequest", repo: node.repo, pr: node.pr}, true
		}
		return nil, false
	case "payload-comment":
		if f.name == "commentEdge" {
			return &gqlNode{kind: "commentedge", repo: node.repo, iss: node.iss, cmt: node.cmt}, true
		}
		return nil, false
	case "commentedge":
		if f.name == "node" {
			return &gqlNode{kind: "comment", repo: node.repo, iss: node.iss, cmt: node.cmt}, true
		}
		return nil, false
	case "branchref", "typefield":
		if f.name == "name" {
			return &gqlNode{kind: "scalar", typ: node.typ}, true
		}
		return nil, false
	case "changedfile":
		if f.name == "path" {
			return &gqlNode{kind: "scalar", typ: node.typ}, true
		}
		return nil, false
	case "null":
		return nil, false
	case "scalar":
		return nil, false
	}
	return nil, false
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// selectNode applies a selection set to a resolved node value.
func (s *server) selectNode(ctx *gqlCtx, node *gqlNode, sel []*gqlField) any {
	if node.kind == "null" {
		return nil
	}
	if node.kind == "scalar" {
		// Scalars are stored as strings; restore their real type.
		if node.typ == "true" {
			return true
		}
		if node.typ == "false" {
			return false
		}
		if n, err := strconv.ParseInt(node.typ, 10, 64); err == nil && node.typ != "" && isAllDigits(node.typ) {
			return n
		}
		return node.typ
	}
	if node.kind == "connnodes" {
		out := make([]any, 0, len(node.conn.nodes))
		for i := range node.conn.nodes {
			n := node.conn.nodes[i]
			out = append(out, s.selectNode(ctx, &n, sel))
		}
		return out
	}
	if len(sel) == 0 {
		return nil
	}
	m := map[string]any{}
	for _, f := range sel {
		s.resolveField(ctx, node, f, m)
	}
	return m
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *server) resolveQueryField(ctx *gqlCtx, f *gqlField) (*gqlNode, bool) {
	switch f.name {
	case "viewer":
		return &gqlNode{kind: "user"}, true
	case "repository":
		owner := ctx.argString(f.args, "owner")
		name := ctx.argString(f.args, "name")
		s.store.mu.RLock()
		repo, ok := s.resolveRepo(owner, name)
		s.store.mu.RUnlock()
		if !ok {
			return &gqlNode{kind: "null"}, true
		}
		return &gqlNode{kind: "repository", repo: repo}, true
	case "node":
		id := ctx.argString(f.args, "id")
		kind, parts, ok := parseGlobalID(id)
		if !ok {
			return &gqlNode{kind: "null"}, true
		}
		if kind == "Repository" && len(parts) == 1 {
			n, err := strconv.Atoi(parts[0])
			if err != nil {
				return &gqlNode{kind: "null"}, true
			}
			s.store.mu.RLock()
			var found *repo
			for _, r := range s.store.repos {
				if r.ID == n {
					found = r
					break
				}
			}
			s.store.mu.RUnlock()
			if found == nil {
				return &gqlNode{kind: "null"}, true
			}
			return &gqlNode{kind: "repository", repo: found}, true
		}
		return &gqlNode{kind: "null"}, true
	case "__type":
		name := ctx.argString(f.args, "name")
		if _, ok := gqlTypeFields[name]; ok {
			return &gqlNode{kind: "type", typ: name}, true
		}
		if _, ok := gqlTypeEnums[name]; ok {
			return &gqlNode{kind: "type", typ: name}, true
		}
		return &gqlNode{kind: "null"}, true
	case "createIssue":
		input := ctx.argObject(f.args, "input")
		repoIDStr, _ := input["repositoryId"].(string)
		kind, parts, ok := parseGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(parts) != 1 {
			return &gqlNode{kind: "null"}, true
		}
		n, _ := strconv.Atoi(parts[0])
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		s.store.mu.Lock()
		var found *repo
		for _, r := range s.store.repos {
			if r.ID == n {
				found = r
				break
			}
		}
		if found == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		it := s.store.createIssue(found, title, body, nil)
		s.store.mu.Unlock()
		return &gqlNode{kind: "payload-issue", repo: found, iss: it}, true
	case "closeIssue":
		input := ctx.argObject(f.args, "input")
		issueIDStr, _ := input["issueId"].(string)
		kind, parts, ok := parseGlobalID(issueIDStr)
		if !ok || kind != "Issue" || len(parts) != 2 {
			return &gqlNode{kind: "null"}, true
		}
		repoID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		s.store.mu.Lock()
		var found *repo
		for _, r := range s.store.repos {
			if r.ID == repoID {
				found = r
				break
			}
		}
		if found == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		it := found.findIssue(num)
		if it == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		it.State = "closed"
		s.store.mu.Unlock()
		return &gqlNode{kind: "payload-issue", repo: found, iss: it}, true
	case "createPullRequest":
		input := ctx.argObject(f.args, "input")
		repoIDStr, _ := input["repositoryId"].(string)
		kind, parts, ok := parseGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(parts) != 1 {
			return &gqlNode{kind: "null"}, true
		}
		n, _ := strconv.Atoi(parts[0])
		head, _ := input["headRefName"].(string)
		base, _ := input["baseRefName"].(string)
		title, _ := input["title"].(string)
		s.store.mu.Lock()
		var found *repo
		for _, r := range s.store.repos {
			if r.ID == n {
				found = r
				break
			}
		}
		if found == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		p, err := s.store.createPull(found, title, "", head, base)
		s.store.mu.Unlock()
		if err != nil {
			return &gqlNode{kind: "null"}, true
		}
		return &gqlNode{kind: "payload-pr", repo: found, pr: p}, true
	case "mergePullRequest":
		input := ctx.argObject(f.args, "input")
		prIDStr, _ := input["pullRequestId"].(string)
		kind, parts, ok := parseGlobalID(prIDStr)
		if !ok || kind != "PullRequest" || len(parts) != 2 {
			return &gqlNode{kind: "null"}, true
		}
		repoID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		s.store.mu.Lock()
		var found *repo
		for _, r := range s.store.repos {
			if r.ID == repoID {
				found = r
				break
			}
		}
		if found == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		p := found.findPull(num)
		if p == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		_, _, err := s.store.mergePull(found, p)
		s.store.mu.Unlock()
		if err != nil {
			return &gqlNode{kind: "null"}, true
		}
		return &gqlNode{kind: "payload-pr", repo: found, pr: p}, true
	case "addComment":
		input := ctx.argObject(f.args, "input")
		subjectStr, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		kind, parts, ok := parseGlobalID(subjectStr)
		if !ok || kind != "Issue" || len(parts) != 2 {
			return &gqlNode{kind: "null"}, true
		}
		repoID, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		s.store.mu.Lock()
		var found *repo
		for _, r := range s.store.repos {
			if r.ID == repoID {
				found = r
				break
			}
		}
		if found == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		it := found.findIssue(num)
		if it == nil {
			s.store.mu.Unlock()
			return &gqlNode{kind: "null"}, true
		}
		id := found.NextCommentID
		found.NextCommentID++
		c := &comment{ID: id, Body: body}
		it.Comments = append(it.Comments, c)
		s.store.mu.Unlock()
		return &gqlNode{kind: "payload-comment", repo: found, iss: it, cmt: c}, true
	}
	return nil, false
}

func (s *server) resolveTypeField(ctx *gqlCtx, node *gqlNode, f *gqlField) (*gqlNode, bool) {
	switch f.name {
	case "name":
		return &gqlNode{kind: "scalar", typ: node.typ}, true
	case "fields":
		fields, ok := gqlTypeFields[node.typ]
		if !ok {
			return &gqlNode{kind: "connnodes", conn: &gqlConn{nodes: nil}}, true
		}
		nodes := make([]gqlNode, 0, len(fields))
		for _, name := range fields {
			nodes = append(nodes, gqlNode{kind: "typefield", typ: name})
		}
		return &gqlNode{kind: "connnodes", conn: &gqlConn{nodes: nodes}}, true
	case "enumValues":
		enums, ok := gqlTypeEnums[node.typ]
		if !ok {
			return &gqlNode{kind: "connnodes", conn: &gqlConn{nodes: nil}}, true
		}
		nodes := make([]gqlNode, 0, len(enums))
		for _, name := range enums {
			nodes = append(nodes, gqlNode{kind: "typefield", typ: name})
		}
		return &gqlNode{kind: "connnodes", conn: &gqlConn{nodes: nodes}}, true
	}
	return nil, false
}

func (s *server) resolveRepoField(ctx *gqlCtx, node *gqlNode, f *gqlField) (*gqlNode, bool) {
	r := node.repo
	switch f.name {
	case "id":
		return &gqlNode{kind: "scalar", typ: repoGlobalID(r.ID)}, true
	case "databaseId":
		return &gqlNode{kind: "scalar", typ: strconv.Itoa(r.ID)}, true
	case "name":
		return &gqlNode{kind: "scalar", typ: r.Name}, true
	case "nameWithOwner":
		return &gqlNode{kind: "scalar", typ: r.Owner + "/" + r.Name}, true
	case "owner":
		return &gqlNode{kind: "user"}, true
	case "defaultBranchRef":
		return &gqlNode{kind: "branchref", typ: r.DefaultBranch}, true
	case "viewerPermission":
		return &gqlNode{kind: "scalar", typ: "ADMIN"}, true
	case "hasIssuesEnabled":
		return &gqlNode{kind: "scalar", typ: "true"}, true
	case "issue":
		num := ctx.argInt(f.args, "number", 0)
		s.store.mu.RLock()
		it := r.findIssue(num)
		s.store.mu.RUnlock()
		if it == nil {
			return &gqlNode{kind: "null"}, true
		}
		return &gqlNode{kind: "issue", repo: r, iss: it}, true
	case "issues":
		return &gqlNode{kind: "conn", conn: s.issuesConn(ctx, r, f)}, true
	case "pullRequests":
		return &gqlNode{kind: "conn", conn: s.pullsConn(ctx, r, f)}, true
	case "pullRequest":
		num := ctx.argInt(f.args, "number", 0)
		s.store.mu.RLock()
		p := r.findPull(num)
		s.store.mu.RUnlock()
		if p == nil {
			return &gqlNode{kind: "null"}, true
		}
		return &gqlNode{kind: "pullrequest", repo: r, pr: p}, true
	}
	return nil, false
}

// issuesConn builds the IssueConnection for a repository.issues field.
func (s *server) issuesConn(ctx *gqlCtx, r *repo, f *gqlField) *gqlConn {
	first := ctx.argInt(f.args, "first", 0)
	states := ctx.argList(f.args, "states")
	orderBy := ctx.argObject(f.args, "orderBy")
	after := ctx.argString(f.args, "after")
	s.store.mu.RLock()
	var items []*issue
	for _, it := range r.Issues {
		if len(states) > 0 {
			match := false
			for _, st := range states {
				if strings.EqualFold(st, it.State) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		items = append(items, it)
	}
	s.store.mu.RUnlock()
	// Order: CREATED_AT DESC (or ASC) = number desc (or asc).
	dir := "DESC"
	if orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok && d != "" {
			dir = d
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if dir == "ASC" {
			return items[i].Number < items[j].Number
		}
		return items[i].Number > items[j].Number
	})
	// Cursor: base64("issue:<number>"); resume after that number.
	afterNum := 0
	if after != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(after); err == nil {
			parts := strings.Split(string(raw), ":")
			if len(parts) == 2 {
				afterNum, _ = strconv.Atoi(parts[1])
			}
		}
	}
	var page []*issue
	for _, it := range items {
		if afterNum != 0 {
			if dir == "ASC" {
				if it.Number <= afterNum {
					continue
				}
			} else {
				if it.Number >= afterNum {
					continue
				}
			}
		}
		page = append(page, it)
	}
	hasNext := false
	endCursor := ""
	if first > 0 && len(page) > first {
		hasNext = true
		page = page[:first]
	}
	if len(page) > 0 {
		endCursor = base64.RawURLEncoding.EncodeToString([]byte("issue:" + strconv.Itoa(page[len(page)-1].Number)))
	}
	nodes := make([]gqlNode, 0, len(page))
	for _, it := range page {
		nodes = append(nodes, gqlNode{kind: "issue", repo: r, iss: it})
	}
	return &gqlConn{nodes: nodes, hasNext: hasNext, endCursor: endCursor}
}

func (s *server) pullsConn(ctx *gqlCtx, r *repo, f *gqlField) *gqlConn {
	first := ctx.argInt(f.args, "first", 0)
	s.store.mu.RLock()
	items := append([]*pull{}, r.Pulls...)
	s.store.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].Number > items[j].Number })
	if first > 0 && len(items) > first {
		items = items[:first]
	}
	nodes := make([]gqlNode, 0, len(items))
	for _, p := range items {
		nodes = append(nodes, gqlNode{kind: "pullrequest", repo: r, pr: p})
	}
	return &gqlConn{nodes: nodes}
}

func (s *server) resolveIssueField(ctx *gqlCtx, node *gqlNode, f *gqlField) (*gqlNode, bool) {
	it := node.iss
	switch f.name {
	case "id":
		return &gqlNode{kind: "scalar", typ: issueGlobalID(node.repo.ID, it.Number)}, true
	case "number":
		return &gqlNode{kind: "scalar", typ: strconv.Itoa(it.Number)}, true
	case "title":
		return &gqlNode{kind: "scalar", typ: it.Title}, true
	case "state":
		return &gqlNode{kind: "scalar", typ: strings.ToUpper(it.State)}, true
	case "body":
		return &gqlNode{kind: "scalar", typ: it.Body}, true
	}
	return nil, false
}

func (s *server) resolvePullField(ctx *gqlCtx, node *gqlNode, f *gqlField) (*gqlNode, bool) {
	p := node.pr
	switch f.name {
	case "id":
		return &gqlNode{kind: "scalar", typ: pullGlobalID(node.repo.ID, p.Number)}, true
	case "number":
		return &gqlNode{kind: "scalar", typ: strconv.Itoa(p.Number)}, true
	case "state":
		if p.Merged {
			return &gqlNode{kind: "scalar", typ: "MERGED"}, true
		}
		return &gqlNode{kind: "scalar", typ: strings.ToUpper(p.State)}, true
	case "merged":
		return &gqlNode{kind: "scalar", typ: boolStr(p.Merged)}, true
	case "headRefName":
		return &gqlNode{kind: "scalar", typ: p.HeadRef}, true
	case "baseRefName":
		return &gqlNode{kind: "scalar", typ: p.BaseRef}, true
	case "mergeable":
		return &gqlNode{kind: "scalar", typ: "MERGEABLE"}, true
	case "files":
		nodes := make([]gqlNode, 0, len(p.ChangedPaths))
		for _, path := range p.ChangedPaths {
			nodes = append(nodes, gqlNode{kind: "changedfile", typ: path})
		}
		return &gqlNode{kind: "conn", conn: &gqlConn{nodes: nodes}}, true
	}
	return nil, false
}

// --- HTTP handler ---

func (s *server) graphqlHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Requires authentication"})
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"message": "cannot read body"})
		return
	}
	// Batch: a JSON array of operations.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []map[string]any
		if err := json.Unmarshal(data, &batch); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"message": "malformed JSON body"})
			return
		}
		results := make([]any, 0, len(batch))
		for _, item := range batch {
			results = append(results, s.runGraphQLOp(item))
		}
		s.writeJSON(w, http.StatusOK, results)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"message": "malformed JSON body"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.runGraphQLOp(body))
}

func (s *server) runGraphQLOp(body map[string]any) map[string]any {
	query, _ := body["query"].(string)
	vars := map[string]any{}
	if v, ok := body["variables"].(map[string]any); ok {
		vars = v
	}
	toks, err := lexGraphQL(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	parser := &gqlParser{toks: toks}
	ops, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	if len(ops) == 0 {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "empty document"}}}
	}
	op := ops[0]
	if name, _ := body["operationName"].(string); name != "" {
		for _, o := range ops {
			if o.name == name {
				op = o
				break
			}
		}
	}
	return s.executeGraphQL(op, vars)
}
