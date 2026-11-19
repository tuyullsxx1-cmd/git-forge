// store.go: the forge's in-memory store. One store holds every repository and
// its git objects, refs, issues, pull requests, labels and comments. All
// state is deterministic: ids and timestamps come from counters seeded at
// fixed values, never from a clock or randomness, so two runs that see the
// same requests leave byte-identical state.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const defaultBranchName = "main"

// viewerName/Email identify the token owner in server-side commits.
var viewerName = "ada iversen"
var viewerEmail = "adaiversen@example.invalid"

// serverCommitEpoch is the fixed base timestamp for commits the forge creates
// itself (contents writes, merges). Each such commit takes the next second,
// so commit ids are deterministic for a given request order.
const serverCommitEpoch = 1735689600

type label struct {
	Name  string
	Color string
}

type comment struct {
	ID   int
	Body string
}

type issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*comment
	Seq      int // creation order, for sorting
}

type pull struct {
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	ChangedPaths   []string
	Seq            int
}

type repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string][20]byte // full refname -> tip commit oid
	Objects       map[[20]byte]obj
	Issues        []*issue
	Pulls         []*pull
	Labels        map[string]*label
	NextNumber    int // shared issue/PR number sequence
	NextCommentID int
	Seq           int // creation order
	packCache map[string]cachedPack
}

// cachedPack is one fetch result held for reuse: the pack bytes plus the
// shallow-info that goes with them. Both derive from the same fetch inputs
// and repo state, so they share one cache entry (and one lock acquisition).
type cachedPack struct {
	pack      []byte
	boundary  [][20]byte
	unshallow [][20]byte
}

type store struct {
	mu            sync.RWMutex
	viewer        string
	repos         map[string]*repo // key owner + "/" + name
	repoSeq       int
	repoIDSeq     int
	issueIDSeq    int
	commitCounter int64
}

func newStore(viewer string) *store {
	return &store{viewer: viewer, repos: map[string]*repo{}}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *store) getRepo(owner, name string) *repo {
	return s.repos[repoKey(owner, name)]
}

// --- repositories ---

func (s *store) createRepo(name, description string, private bool) (*repo, error) {
	if s.repos[repoKey(s.viewer, name)] != nil {
		return nil, errors.New("already exists")
	}
	s.repoIDSeq++
	s.repoSeq++
	r := &repo{
		ID:            s.repoIDSeq,
		Owner:         s.viewer,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranchName,
		Refs:          map[string][20]byte{},
		Objects:       map[[20]byte]obj{},
		Labels:        map[string]*label{},
		NextNumber:    1,
		NextCommentID: 1,
		Seq:           s.repoSeq,
		packCache:     map[string]cachedPack{},
	}
	s.repos[repoKey(s.viewer, name)] = r
	return r, nil
}

func (s *store) listViewerRepos() []*repo {
	out := make([]*repo, 0, len(s.repos))
	for _, r := range s.repos {
		if r.Owner == s.viewer {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s *store) deleteRepo(r *repo) {
	delete(s.repos, repoKey(r.Owner, r.Name))
}

// --- git objects and refs ---

func (r *repo) putObject(typ string, data []byte) [20]byte {
	id := hashObject(typ, data)
	r.Objects[id] = obj{typ: typ, data: data}
	return id
}

func (r *repo) getObject(id [20]byte) (obj, bool) {
	o, ok := r.Objects[id]
	return o, ok
}

// headRef returns the full refname of the default branch.
func (r *repo) headRef() string { return "refs/heads/" + r.DefaultBranch }

func (r *repo) branchTip(branch string) ([20]byte, bool) {
	id, ok := r.Refs["refs/heads/"+branch]
	return id, ok
}

// isAncestor reports whether anc is reachable from desc (following parents).
func (r *repo) isAncestor(anc, desc [20]byte) bool {
	if anc == desc {
		return true
	}
	seen := map[[20]byte]bool{}
	stack := [][20]byte{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o, ok := r.Objects[cur]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		for _, p := range parents {
			if p == anc {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// mergeBase returns the first common ancestor of a and b (linear histories:
// walk a's ancestry into a set, then walk b's).
func (r *repo) mergeBase(a, b [20]byte) ([20]byte, bool) {
	ancestors := map[[20]byte]bool{}
	stack := [][20]byte{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		o, ok := r.Objects[cur]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		stack = append(stack, parents...)
	}
	stack = [][20]byte{b}
	seen := map[[20]byte]bool{}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancestors[cur] {
			return cur, true
		}
		o, ok := r.Objects[cur]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		stack = append(stack, parents...)
	}
	return [20]byte{}, false
}

// commitsFrom returns commits reachable from tip, newest first (parent walk).
func (r *repo) commitsFrom(tip [20]byte) [][20]byte {
	var out [][20]byte
	seen := map[[20]byte]bool{}
	stack := [][20]byte{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o, ok := r.Objects[cur]
		if !ok || o.typ != "commit" {
			continue
		}
		out = append(out, cur)
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		// Push parents in reverse so the first parent is visited first.
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return out
}

// treeOf returns the root tree oid of a commit.
func (r *repo) treeOf(commitID [20]byte) ([20]byte, bool) {
	o, ok := r.Objects[commitID]
	if !ok || o.typ != "commit" {
		return [20]byte{}, false
	}
	tree, _, err := parseCommitHeader(o.data)
	if err != nil {
		return [20]byte{}, false
	}
	return tree, true
}

// flattenTree returns path -> blob oid for every file under a tree.
func (r *repo) flattenTree(treeID [20]byte) (map[string][20]byte, error) {
	out := map[string][20]byte{}
	var walk func(t [20]byte, prefix string) error
	walk = func(t [20]byte, prefix string) error {
		o, ok := r.Objects[t]
		if !ok || o.typ != "tree" {
			return fmt.Errorf("missing tree %s", oidHex(t))
		}
		entries, err := parseTree(o.data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			path := prefix + e.name
			if e.mode == "40000" {
				if err := walk(e.oid, path+"/"); err != nil {
					return err
				}
			} else {
				out[path] = e.oid
			}
		}
		return nil
	}
	if err := walk(treeID, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// buildTreeFromFiles builds nested trees for files (path -> content bytes),
// storing every object, and returns the root tree oid.
func (r *repo) buildTreeFromFiles(files map[string][]byte) [20]byte {
	type node struct {
		files map[string][]byte
		dirs  map[string]*node
	}
	root := &node{files: map[string][]byte{}, dirs: map[string]*node{}}
	for path, data := range files {
		parts := strings.Split(path, "/")
		n := root
		for _, p := range parts[:len(parts)-1] {
			if n.dirs[p] == nil {
				n.dirs[p] = &node{files: map[string][]byte{}, dirs: map[string]*node{}}
			}
			n = n.dirs[p]
		}
		n.files[parts[len(parts)-1]] = data
	}
	var build func(n *node) [20]byte
	build = func(n *node) [20]byte {
		var entries []treeEntry
		for name, data := range n.files {
			entries = append(entries, treeEntry{mode: "100644", name: name, oid: r.putObject("blob", data)})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
		}
		raw := buildTree(entries)
		return r.putObject("tree", raw)
	}
	return build(root)
}

// fileAt returns the blob bytes at path in the tree of commitID.
func (r *repo) fileAt(commitID [20]byte, path string) ([]byte, bool) {
	tree, ok := r.treeOf(commitID)
	if !ok {
		return nil, false
	}
	flat, err := r.flattenTree(tree)
	if err != nil {
		return nil, false
	}
	blobID, ok := flat[path]
	if !ok {
		return nil, false
	}
	o, ok := r.Objects[blobID]
	if !ok {
		return nil, false
	}
	return o.data, true
}

// nextCommitTime returns the next deterministic timestamp for a server commit.
func (s *store) nextCommitTime() int64 {
	s.commitCounter++
	return serverCommitEpoch + s.commitCounter
}

// serverCommit creates a commit on branch with the given tree, message and
// parents, updates the branch ref, and clears the pack cache.
func (s *store) serverCommit(r *repo, branch string, tree [20]byte, parents [][20]byte, message string) [20]byte {
	ident := viewerName + " <" + viewerEmail + ">"
	epoch := s.nextCommitTime()
	raw := buildCommit(tree, parents, ident, ident, epoch, message)
	id := r.putObject("commit", raw)
	r.Refs["refs/heads/"+branch] = id
	r.packCache = map[string]cachedPack{}
	return id
}

// depthFrom returns the generation depth (1-based, a start itself is 1) of
// target measured from any of starts, following parent links.
func (r *repo) depthFrom(starts [][20]byte, target [20]byte) (int, bool) {
	type item struct {
		id    [20]byte
		depth int
	}
	seen := map[[20]byte]bool{}
	queue := []item{}
	for _, s := range starts {
		queue = append(queue, item{s, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.id] {
			continue
		}
		seen[cur.id] = true
		if cur.id == target {
			return cur.depth, true
		}
		o, ok := r.Objects[cur.id]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		for _, p := range parents {
			queue = append(queue, item{p, cur.depth + 1})
		}
	}
	return 0, false
}

// collectForFetch returns the objects to send for a fetch request, excluding
// anything the client already holds, limited to req.depth commits from each
// want (0 = no limit). It also returns the shallow boundary commit ids
// (commits at the depth limit) and the previous shallow commits that are now
// fully included (their parents were sent or were already held) — those are
// reported as "unshallow".
//
// Shallow semantics (the subtle part): a have's ancestors are held by the
// client ONLY up to the client's shallow boundary — a boundary commit is
// held, but its parents are not. The walk therefore descends through held
// shallow-boundary commits (that is what "deepen" means: send the history
// below the boundary) while skipping held non-boundary commits entirely.
func (r *repo) collectForFetch(req fetchRequest) (map[[20]byte]obj, [][20]byte, [][20]byte) {
	wants, haves := req.wants, req.haves
	maxDepth := req.depth
	if req.deepenRelative && maxDepth > 0 && len(req.shallowReq) > 0 {
		// "deepen N" with deepen-relative counts N generations below the
		// current shallow boundary, not from the wants: find the deepest
		// shallow boundary commit (by generation from the wants) and
		// extend N below it.
		deepest := 0
		for _, s := range req.shallowReq {
			if d, ok := r.depthFrom(wants, s); ok && d > deepest {
				deepest = d
			}
		}
		if deepest > 0 {
			maxDepth = deepest + req.depth
		}
	}
	shallow := map[[20]byte]bool{}
	for _, s := range req.shallowReq {
		shallow[s] = true
	}
	// Held set: commits reachable from haves without passing through a
	// shallow boundary commit (the boundary itself is held, its parents
	// are not — the client only has the boundary commit, not its history).
	haveReachable := map[[20]byte]bool{}
	for _, h := range haves {
		stack := [][20]byte{h}
		seen := map[[20]byte]bool{}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[cur] {
				continue
			}
			seen[cur] = true
			o, ok := r.Objects[cur]
			if !ok || o.typ != "commit" {
				continue
			}
			haveReachable[cur] = true
			if shallow[cur] {
				continue // do not descend past a shallow boundary
			}
			_, parents, err := parseCommitHeader(o.data)
			if err != nil {
				continue
			}
			stack = append(stack, parents...)
		}
		// Haves may name non-commit objects too; mark the oid itself.
		haveReachable[h] = true
	}
	// Walk wants breadth-first by commit generation. The walk traverses
	// through held commits when it has to (to reach a shallow boundary
	// below them for a deepen), but only sends commits the client does
	// not hold.
	//
	// A want naming an annotated tag object includes the tag object
	// itself and peels to its target (recursively, for nested tags);
	// the peeled commit is then walked like any other want.
	wantCommits := [][20]byte{}
	extraObjects := [][20]byte{}
	boundary := [][20]byte{}
	type item struct {
		id    [20]byte
		depth int
	}
	seen := map[[20]byte]bool{}
	var queue []item
	for _, w := range wants {
		id := w
		for {
			o, ok := r.Objects[id]
			if !ok || o.typ != "tag" {
				break
			}
			extraObjects = append(extraObjects, id)
			targetHex := parseTagTarget(o.data)
			target, ok := parseOIDHex(targetHex)
			if !ok {
				id = [20]byte{}
				break
			}
			id = target
		}
		if id != [20]byte{} {
			queue = append(queue, item{id, 1})
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.id] {
			continue
		}
		seen[cur.id] = true
		o, ok := r.Objects[cur.id]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		held := haveReachable[cur.id]
		if held && !shallow[cur.id] {
			// Client has this commit and its full history. Descend
			// through it only when a shallow boundary may lie below
			// (a deepen is in progress), respecting the depth limit.
			if len(req.shallowReq) > 0 || maxDepth > 0 {
				if maxDepth == 0 || cur.depth < maxDepth {
					for _, p := range parents {
						queue = append(queue, item{p, cur.depth + 1})
					}
				}
			}
			continue
		}
		if held && shallow[cur.id] {
			// Client has the boundary commit but not its history:
			// descend into its parents (deepen), but do not send it.
			// With no deepen requested (maxDepth 0) the boundary is a
			// hard stop.
			if maxDepth > 0 && cur.depth < maxDepth {
				for _, p := range parents {
					queue = append(queue, item{p, cur.depth + 1})
				}
			}
			continue
		}
		// Not held: send this commit.
		wantCommits = append(wantCommits, cur.id)
		if maxDepth > 0 && cur.depth >= maxDepth {
			boundary = append(boundary, cur.id)
			continue
		}
		for _, p := range parents {
			queue = append(queue, item{p, cur.depth + 1})
		}
	}
	out := map[[20]byte]obj{}
	add := func(id [20]byte) {
		if o, ok := r.Objects[id]; ok {
			out[id] = o
		}
	}
	// Peeled tag objects named by a want are always sent, unless the
	// client already holds them.
	for _, id := range extraObjects {
		if !haveReachable[id] {
			add(id)
		}
	}
	for _, c := range wantCommits {
		add(c)
		tree, ok := r.treeOf(c)
		if !ok {
			continue
		}
		// Include the tree and its blobs, but only trees/blobs not already
		// reachable from a have commit: walk the tree recursively, skipping
		// subtrees whose oid is in a have commit's tree. Cheap approach:
		// include everything under this commit's tree unless the commit is
		// itself a have (it is not, by construction).
		var walk func(t [20]byte)
		walk = func(t [20]byte) {
			if _, done := out[t]; done {
				return
			}
			o, ok := r.Objects[t]
			if !ok || o.typ != "tree" {
				return
			}
			out[t] = o
			entries, err := parseTree(o.data)
			if err != nil {
				return
			}
			for _, e := range entries {
				if e.mode == "40000" {
					walk(e.oid)
				} else {
					add(e.oid)
				}
			}
		}
		walk(tree)
	}
	// A previous shallow commit is unshallow when it is no longer on the
	// boundary and at least one of its parents is now present on the
	// client (sent in this fetch, or already held). Otherwise it stays
	// shallow and the client keeps it without any mention.
	sentSet := map[[20]byte]bool{}
	for _, c := range wantCommits {
		sentSet[c] = true
	}
	boundarySet := map[[20]byte]bool{}
	for _, b := range boundary {
		boundarySet[b] = true
	}
	var unshallow [][20]byte
	for _, s := range req.shallowReq {
		if boundarySet[s] {
			continue
		}
		o, ok := r.Objects[s]
		if !ok || o.typ != "commit" {
			continue
		}
		_, parents, err := parseCommitHeader(o.data)
		if err != nil {
			continue
		}
		for _, p := range parents {
			if sentSet[p] || haveReachable[p] {
				unshallow = append(unshallow, s)
				break
			}
		}
	}
	// Remove anything the client already holds: a commit's tree/blobs may
	// coincide with held history. Only the held set is removed (bounded by
	// the shallow boundary — the parents of a held boundary commit are NOT
	// held and must stay in the pack).
	for c := range haveReachable {
		delete(out, c)
		if tree, ok := r.treeOf(c); ok {
			flat, err := r.flattenTree(tree)
			if err == nil {
				for _, b := range flat {
					delete(out, b)
				}
			}
			// Also drop the trees themselves.
			var dropTrees func(t [20]byte)
			dropTrees = func(t [20]byte) {
				delete(out, t)
				if o, ok := r.Objects[t]; ok && o.typ == "tree" {
					if entries, err := parseTree(o.data); err == nil {
						for _, e := range entries {
							if e.mode == "40000" {
								dropTrees(e.oid)
							}
						}
					}
				}
			}
			dropTrees(tree)
		}
	}
	return out, boundary, unshallow
}

// --- issues ---

func (s *store) createIssue(r *repo, title, body string, labels []string) *issue {
	s.issueIDSeq++
	n := r.NextNumber
	r.NextNumber++
	it := &issue{
		ID:     s.issueIDSeq,
		Number: n,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string{}, labels...),
		Seq:    n,
	}
	r.Issues = append(r.Issues, it)
	return it
}

func (r *repo) findIssue(number int) *issue {
	for _, it := range r.Issues {
		if it.Number == number {
			return it
		}
	}
	return nil
}

func (r *repo) findPull(number int) *pull {
	for _, p := range r.Pulls {
		if p.Number == number {
			return p
		}
	}
	return nil
}

// --- pull requests ---

// diffTrees returns changed paths between base and head trees.
func (r *repo) diffTrees(baseTree, headTree [20]byte) ([]string, map[string][2][20]byte) {
	baseFlat, _ := r.flattenTree(baseTree)
	headFlat, _ := r.flattenTree(headTree)
	changed := []string{}
	detail := map[string][2][20]byte{}
	for path, h := range headFlat {
		if b, ok := baseFlat[path]; !ok || b != h {
			changed = append(changed, path)
			detail[path] = [2][20]byte{b, h}
		}
	}
	for path, b := range baseFlat {
		if _, ok := headFlat[path]; !ok {
			changed = append(changed, path)
			detail[path] = [2][20]byte{b, [20]byte{}}
		}
	}
	sort.Strings(changed)
	return changed, detail
}

func (s *store) createPull(r *repo, title, body, headRef, baseRef string) (*pull, error) {
	headTip, ok := r.branchTip(headRef)
	if !ok {
		return nil, errors.New("head branch not found")
	}
	baseTip, ok := r.branchTip(baseRef)
	if !ok {
		return nil, errors.New("base branch not found")
	}
	headTree, _ := r.treeOf(headTip)
	baseTree, _ := r.treeOf(baseTip)
	changed, _ := r.diffTrees(baseTree, headTree)
	n := r.NextNumber
	r.NextNumber++
	p := &pull{
		Number:       n,
		Title:        title,
		Body:         body,
		State:        "open",
		HeadRef:      headRef,
		BaseRef:      baseRef,
		HeadSHA:      oidHex(headTip),
		BaseSHA:      oidHex(baseTip),
		ChangedPaths: changed,
		Seq:          n,
	}
	r.Pulls = append(r.Pulls, p)
	return p, nil
}

// mergePull performs a squash merge. It returns the merge commit oid, or an
// error; a conflict error is flagged with isConflict.
func (s *store) mergePull(r *repo, p *pull) ([20]byte, bool, error) {
	if p.State != "open" {
		return [20]byte{}, false, errors.New("pull request is not open")
	}
	headTip, ok := r.branchTip(p.HeadRef)
	if !ok {
		return [20]byte{}, false, errors.New("head branch not found")
	}
	baseTip, ok := r.branchTip(p.BaseRef)
	if !ok {
		return [20]byte{}, false, errors.New("base branch not found")
	}
	base, ok := r.mergeBase(baseTip, headTip)
	if !ok {
		return [20]byte{}, false, errors.New("no merge base")
	}
	baseTree, _ := r.treeOf(base)
	headTree, _ := r.treeOf(headTip)
	curTree, _ := r.treeOf(baseTip)
	baseFlat, _ := r.flattenTree(baseTree)
	headFlat, _ := r.flattenTree(headTree)
	curFlat, _ := r.flattenTree(curTree)
	// Three-way merge at file granularity.
	merged := map[string][]byte{}
	for path, blobID := range curFlat {
		if o, ok := r.Objects[blobID]; ok {
			merged[path] = o.data
		}
	}
	changed, _ := r.diffTrees(baseTree, headTree)
	for _, path := range changed {
		headBlob, headHas := headFlat[path]
		baseBlob, baseHas := baseFlat[path]
		curBlob, curHas := curFlat[path]
		// "Changed" means the side's version differs from the base
		// version, where absent counts as a version: a file absent from
		// both base and cur was not changed by cur, even though it is
		// absent from base.
		curChanged := curHas != baseHas || (curHas && baseHas && curBlob != baseBlob)
		_ = baseBlob
		if curChanged && headBlob != curBlob {
			// Both sides changed the file differently.
			if headHas && curHas {
				return [20]byte{}, true, errors.New("merge conflict")
			}
			if !headHas && curHas {
				return [20]byte{}, true, errors.New("merge conflict")
			}
			if headHas && !curHas {
				return [20]byte{}, true, errors.New("merge conflict")
			}
		}
		if headHas {
			if o, ok := r.Objects[headBlob]; ok {
				merged[path] = o.data
			}
		} else {
			delete(merged, path)
		}
	}
	tree := r.buildTreeFromFiles(merged)
	message := "Merge pull request #" + itoa(p.Number) + ": " + p.Title + "\n"
	mergeID := s.serverCommit(r, p.BaseRef, tree, [][20]byte{baseTip}, message)
	p.State = "closed"
	p.Merged = true
	p.MergedAt = "2025-01-01T00:00:00Z"
	p.MergeCommitSHA = oidHex(mergeID)
	p.BaseSHA = oidHex(mergeID)
	return mergeID, false, nil
}

// --- global ids (GraphQL) ---

func globalID(kind string, parts ...string) string {
	raw := kind + ":" + strings.Join(parts, ":")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func repoGlobalID(id int) string    { return globalID("Repository", itoa(id)) }
func issueGlobalID(repoID, n int) string {
	return globalID("Issue", itoa(repoID), itoa(n))
}
func pullGlobalID(repoID, n int) string {
	return globalID("PullRequest", itoa(repoID), itoa(n))
}
func commentGlobalID(repoID, issueNum, commentID int) string {
	return globalID("IssueComment", itoa(repoID), itoa(issueNum), itoa(commentID))
}

// parseGlobalID decodes an opaque id back to kind + parts.
func parseGlobalID(id string) (string, []string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}
