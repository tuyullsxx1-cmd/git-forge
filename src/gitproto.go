// gitproto.go: git smart HTTP — pkt-line framing, upload-pack protocol v2
// (with v0 fallback) and receive-pack v0. No git binary is used.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// --- pkt-line framing ---

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	return append(out, payload...)
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

type pktKind int

const (
	pktKindData pktKind = iota
	pktKindFlush
	pktKindDelim
	pktKindResponseEnd
)

// pktLineReader decodes a pkt-line stream.
type pktLineReader struct {
	data []byte
	pos  int
	err  error
}

func (r *pktLineReader) next() ([]byte, pktKind, bool) {
	if r.err != nil || r.pos >= len(r.data) {
		return nil, pktKindFlush, false
	}
	if len(r.data)-r.pos < 4 {
		r.err = fmt.Errorf("truncated pkt-line header")
		return nil, pktKindFlush, false
	}
	head := string(r.data[r.pos : r.pos+4])
	switch head {
	case "0000":
		r.pos += 4
		return nil, pktKindFlush, true
	case "0001":
		r.pos += 4
		return nil, pktKindDelim, true
	case "0002":
		r.pos += 4
		return nil, pktKindResponseEnd, true
	}
	n, err := strconv.ParseInt(head, 16, 32)
	if err != nil || n < 4 || n > 65520 {
		r.err = fmt.Errorf("bad pkt-line length %q", head)
		return nil, pktKindFlush, false
	}
	if r.pos+int(n) > len(r.data) {
		r.err = fmt.Errorf("truncated pkt-line payload")
		return nil, pktKindFlush, false
	}
	payload := r.data[r.pos+4 : r.pos+int(n)]
	r.pos += int(n)
	return payload, pktKindData, true
}

// --- sideband ---

// sidebandPackets splits data into band-1 pkt-lines (max payload 65515 bytes
// of data after the 1-byte band marker, keeping the pkt-line <= 65520).
func sidebandPackets(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		chunk := 65515
		if len(data) < chunk {
			chunk = len(data)
		}
		payload := append([]byte{1}, data[:chunk]...)
		out = append(out, pktLine(payload))
		data = data[chunk:]
	}
	return out
}

// --- ref advertisement ---

type refLine struct {
	oid     string // hex, or "unborn"
	name    string
	peeled  string // commit oid for annotated tags
	symref  string // symref target for HEAD
	isUnborn bool
}

func (s *store) refLines(r *repo) []refLine {
	var names []string
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []refLine
	for _, name := range names {
		oid := r.Refs[name]
		line := refLine{oid: oidHex(oid), name: name}
		if strings.HasPrefix(name, "refs/tags/") {
			if o, ok := r.Objects[oid]; ok && o.typ == "tag" {
				if target := parseTagTarget(o.data); target != "" {
					line.peeled = target
				}
			}
		}
		out = append(out, line)
	}
	return out
}

// parseTagTarget extracts the target commit oid from a tag object.
func parseTagTarget(data []byte) string {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			return string(line[7:])
		}
		if len(line) == 0 {
			break
		}
	}
	return ""
}

// headLine returns the HEAD ref line for the repo (unborn when empty).
func (s *store) headLine(r *repo) refLine {
	tip, ok := r.Refs[r.headRef()]
	if !ok {
		return refLine{oid: "unborn", name: "HEAD", symref: r.headRef(), isUnborn: true}
	}
	return refLine{oid: oidHex(tip), name: "HEAD", symref: r.headRef()}
}

const gitAgent = "forge/1.0"

func v2Capabilities() []string {
	return []string{
		"agent=" + gitAgent,
		"ls-refs=unborn",
		"fetch=shallow filter",
		"object-format=sha1",
	}
}

func v0Capabilities() string {
	return "symref=HEAD:refs/heads/main side-band-64k ofs-delta agent=" + gitAgent + " object-format=sha1"
}

// writeV2Advertisement writes the upload-pack v2 advertisement. Protocol v2
// advertisements start with the "version 2" line and carry NO "# service="
// preamble (that line belongs to v0 only; sending it makes git reject the
// response as unreadable).
func writeV2Advertisement(w io.Writer, service string) {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("version 2\n")))
	for _, cap := range v2Capabilities() {
		buf.Write(pktLine([]byte(cap + "\n")))
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

// writeV0Advertisement writes a v0 ref advertisement for service. The v0
// format is: "# service=..." line, a flush, the ref lines, a flush. The
// flush after the service line is mandatory — without it git aborts the
// connection as unreadable.
func (s *store) writeV0Advertisement(w io.Writer, service string, r *repo, caps string) {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush())
	lines := s.refLines(r)
	if len(lines) == 0 {
		// Empty repository: the capabilities^{} pseudo-ref.
		buf.Write(pktLine([]byte("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n")))
		buf.Write(pktFlush())
		w.Write(buf.Bytes())
		return
	}
	head := s.headLine(r)
	buf.Write(pktLine([]byte(head.oid + " HEAD\x00" + caps + "\n")))
	for _, line := range lines {
		buf.Write(pktLine([]byte(line.oid + " " + line.name + "\n")))
		if line.peeled != "" {
			buf.Write(pktLine([]byte(line.peeled + " " + line.name + "^{}\n")))
		}
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

// --- git HTTP handlers ---

// gitHandler routes /{owner}/{repo}.git/... requests.
func (s *store) gitHandler(w http.ResponseWriter, r *http.Request) {
	// Path: /{owner}/{repo}.git/<rest>
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		http.NotFound(w, r)
		return
	}
	owner, repoName := parts[0], strings.TrimSuffix(parts[1], ".git")
	rest := parts[2]

	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	s.mu.Unlock()
	if repo == nil {
		// Missing repository: 404 on both services' info/refs; the server
		// stays up. (POSTs to a missing repo also 404.)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "Not Found")
		return
	}
	if owner != s.viewer {
		// Repositories belong to the token owner; another owner is 404.
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "Not Found")
		return
	}

	switch {
	case r.Method == http.MethodGet && rest == "info/refs":
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "bad service")
			return
		}
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
			writeV2Advertisement(w, service)
			return
		}
		caps := v0Capabilities()
		if service == "git-receive-pack" {
			caps = "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent + " object-format=sha1"
		}
		s.mu.RLock()
		s.writeV0Advertisement(w, service, repo, caps)
		s.mu.RUnlock()
	case r.Method == http.MethodPost && rest == "git-upload-pack":
		s.handleUploadPack(w, r, repo)
	case r.Method == http.MethodPost && rest == "git-receive-pack":
		s.handleReceivePack(w, r, repo)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "Not Found")
	}
}

// --- upload-pack ---

type fetchRequest struct {
	wants         [][20]byte
	haves         [][20]byte
	shallowReq    [][20]byte // shallow lines the client already has
	depth         int
	deepenRelative bool // "deepen N" counts N below the current boundary, not from the wants
	done          bool
	thinPack      bool
	ofsDelta      bool
}

func parseOIDList(lines []string, prefix string) ([][20]byte, error) {
	var out [][20]byte
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix+" ") {
			continue
		}
		id, ok := parseOIDHex(strings.TrimSpace(line[len(prefix)+1:]))
		if !ok {
			return nil, fmt.Errorf("bad oid in %q", line)
		}
		out = append(out, id)
	}
	return out, nil
}

func (s *store) handleUploadPack(w http.ResponseWriter, r *http.Request, repo *repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("Git-Protocol") == "version=2" {
		s.handleUploadPackV2(w, body, repo)
		return
	}
	s.handleUploadPackV0(w, body, repo)
}

func (s *store) handleUploadPackV2(w http.ResponseWriter, body []byte, repo *repo) {
	reader := &pktLineReader{data: body}
	var lines []string
	command := ""
	seenDelim := false
	for {
		payload, kind, ok := reader.next()
		if !ok || reader.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch kind {
		case pktKindFlush:
			goto parsed
		case pktKindDelim:
			seenDelim = true
		case pktKindData:
			line := strings.TrimSuffix(string(payload), "\n")
			if command == "" && strings.HasPrefix(line, "command=") {
				command = strings.TrimPrefix(line, "command=")
				continue
			}
			if !seenDelim && (strings.HasPrefix(line, "agent=") || strings.HasPrefix(line, "object-format=")) {
				continue
			}
			lines = append(lines, line)
		}
	}
parsed:
	switch command {
	case "ls-refs":
		s.serveLsRefs(w, lines, repo)
	case "fetch":
		s.serveFetchV2(w, lines, repo)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (s *store) serveLsRefs(w http.ResponseWriter, args []string, repo *repo) {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var buf bytes.Buffer
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	head := s.headLine(repo)
	if head.isUnborn {
		// An empty repository advertises only the unborn HEAD line. (Do
		// NOT also emit "unborn refs/heads/main": the kit's ls-refs
		// parser turns that into a ref named refs/heads/main with the
		// zero oid, which makes the repo look non-empty and makes
		// lsRefs report the branch as existing.)
		if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + head.symref
			}
			if keep("HEAD") {
				buf.Write(pktLine([]byte(line + "\n")))
			}
		}
	} else {
		if keep("HEAD") {
			line := head.oid + " HEAD"
			if symrefs {
				line += " symref-target:" + head.symref
			}
			buf.Write(pktLine([]byte(line + "\n")))
		}
		for _, line := range s.refLines(repo) {
			if !keep(line.name) {
				continue
			}
			text := line.oid + " " + line.name
			if peel && line.peeled != "" {
				text += " peeled:" + line.peeled
			}
			buf.Write(pktLine([]byte(text + "\n")))
		}
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

func (s *store) serveFetchV2(w http.ResponseWriter, args []string, repo *repo) {
	var req fetchRequest
	for _, arg := range args {
		switch {
		case arg == "thin-pack":
			req.thinPack = true
		case arg == "no-progress":
		case arg == "ofs-delta":
			req.ofsDelta = true
		case arg == "done":
			req.done = true
		case strings.HasPrefix(arg, "want "):
			if id, ok := parseOIDHex(strings.TrimSpace(arg[5:])); ok {
				req.wants = append(req.wants, id)
			}
		case strings.HasPrefix(arg, "have "):
			if id, ok := parseOIDHex(strings.TrimSpace(arg[5:])); ok {
				req.haves = append(req.haves, id)
			}
		case strings.HasPrefix(arg, "shallow "):
			if id, ok := parseOIDHex(strings.TrimSpace(arg[8:])); ok {
				req.shallowReq = append(req.shallowReq, id)
			}
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(arg[7:])); err == nil {
				req.depth = n
			}
		case arg == "deepen-relative":
			req.deepenRelative = true
		}
	}
	if len(req.wants) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	objects, boundary, unshallow := repo.collectForFetch(req)
	// Cache key: wants + haves + depth.
	key := fetchCacheKey(req)
	if cached, ok := repo.packCache[key]; ok {
		s.mu.Unlock()
		s.writeFetchV2Response(w, req, repo, cached, boundary, unshallow)
		return
	}
	pack := writePack(objects)
	if repo.packCache == nil {
		repo.packCache = map[string][]byte{}
	}
	repo.packCache[key] = pack
	s.mu.Unlock()
	s.writeFetchV2Response(w, req, repo, pack, boundary, unshallow)
}

func fetchCacheKey(req fetchRequest) string {
	var b strings.Builder
	b.WriteString("v2:")
	for _, w := range req.wants {
		b.WriteString(oidHex(w))
		b.WriteByte(',')
	}
	b.WriteString("|")
	for _, h := range req.haves {
		b.WriteString(oidHex(h))
		b.WriteByte(',')
	}
	fmt.Fprintf(&b, "|d%d", req.depth)
	if req.deepenRelative {
		b.WriteString("|rel")
	}
	for _, s := range req.shallowReq {
		b.WriteString("|s")
		b.WriteString(oidHex(s))
	}
	return b.String()
}

func (s *store) writeFetchV2Response(w http.ResponseWriter, req fetchRequest, repo *repo, pack []byte, boundary, unshallow [][20]byte) {
	var buf bytes.Buffer
	// The acknowledgment section exists only while negotiation is open:
	// the client sent haves but has not sent "done" yet. A request that
	// already says "done" (fresh clone, or a fetch whose haves are final)
	// goes straight to shallow-info/packfile — real git errors with
	// "expected 'packfile', received 'acknowledgments'" otherwise, and
	// errors the opposite way when the section is missing mid-negotiation.
	if len(req.haves) > 0 && !req.done {
		buf.Write(pktLine([]byte("acknowledgments\n")))
		acked := false
		s.mu.RLock()
		for _, h := range req.haves {
			if _, ok := repo.Objects[h]; ok {
				buf.Write(pktLine([]byte("ACK " + oidHex(h) + "\n")))
				acked = true
			}
		}
		s.mu.RUnlock()
		if !acked {
			buf.Write(pktLine([]byte("NAK\n")))
		} else {
			buf.Write(pktLine([]byte("ready\n")))
		}
		buf.Write(pktDelim())
	}
	// shallow-info section
	if len(boundary) > 0 || len(unshallow) > 0 {
		var lines []string
		for _, b := range boundary {
			lines = append(lines, "shallow "+oidHex(b))
		}
		for _, u := range unshallow {
			lines = append(lines, "unshallow "+oidHex(u))
		}
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, line := range lines {
			buf.Write(pktLine([]byte(line + "\n")))
		}
		buf.Write(pktDelim())
	}
	// packfile section: always present for a fetch with wants, even when
	// nothing is new (real git sends an empty pack then).
	buf.Write(pktLine([]byte("packfile\n")))
	for _, pkt := range sidebandPackets(pack) {
		buf.Write(pkt)
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

func (s *store) handleUploadPackV0(w http.ResponseWriter, body []byte, repo *repo) {
	reader := &pktLineReader{data: body}
	var section1, section2 []string
	section := &section1
	for {
		payload, kind, ok := reader.next()
		if !ok || reader.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch kind {
		case pktKindFlush:
			if section == &section1 {
				section = &section2
			} else {
				goto parsed
			}
		case pktKindData:
			*section = append(*section, strings.TrimSuffix(string(payload), "\n"))
		}
	}
parsed:
	var req fetchRequest
	for _, line := range section1 {
		switch {
		case strings.HasPrefix(line, "want "):
			// First want may carry capabilities after a NUL.
			rest := strings.TrimSpace(line[5:])
			if i := strings.Index(rest, "\x00"); i >= 0 {
				rest = rest[:i]
			}
			if id, ok := parseOIDHex(rest); ok {
				req.wants = append(req.wants, id)
			}
		case strings.HasPrefix(line, "shallow "):
			if id, ok := parseOIDHex(strings.TrimSpace(line[8:])); ok {
				req.shallowReq = append(req.shallowReq, id)
			}
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				req.depth = n
			}
		case line == "deepen-relative":
			req.deepenRelative = true
		}
	}
	for _, line := range section2 {
		switch {
		case strings.HasPrefix(line, "have "):
			if id, ok := parseOIDHex(strings.TrimSpace(line[5:])); ok {
				req.haves = append(req.haves, id)
			}
		case line == "done":
			req.done = true
		}
	}
	if len(req.wants) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	objects, boundary, unshallow := repo.collectForFetch(req)
	pack := writePack(objects)
	s.mu.Unlock()
	var buf bytes.Buffer
	acked := false
	s.mu.RLock()
	for _, h := range req.haves {
		if _, ok := repo.Objects[h]; ok {
			buf.Write(pktLine([]byte("ACK " + oidHex(h) + "\n")))
			acked = true
		}
	}
	s.mu.RUnlock()
	if !acked {
		buf.Write(pktLine([]byte("NAK\n")))
	}
	for _, b := range boundary {
		buf.Write(pktLine([]byte("shallow " + oidHex(b) + "\n")))
	}
	for _, u := range unshallow {
		buf.Write(pktLine([]byte("unshallow " + oidHex(u) + "\n")))
	}
	if len(objects) > 0 {
		for _, pkt := range sidebandPackets(pack) {
			buf.Write(pkt)
		}
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

// --- receive-pack ---

type pushCommand struct {
	oldID [20]byte
	newID [20]byte
	ref   string
}

func (s *store) handleReceivePack(w http.ResponseWriter, r *http.Request, repo *repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	reader := &pktLineReader{data: body}
	var commands []pushCommand
	packStart := -1
	for {
		payload, kind, ok := reader.next()
		if !ok || reader.err != nil {
			s.writePushReport(w, false, "malformed request", nil)
			return
		}
		if kind == pktKindFlush {
			packStart = reader.pos
			break
		}
		if kind != pktKindData {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		// First line may carry capabilities after a NUL.
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			s.writePushReport(w, false, "malformed command", nil)
			return
		}
		oldID, ok1 := parseOIDHex(fields[0])
		newID, ok2 := parseOIDHex(fields[1])
		if !ok1 || !ok2 || !strings.HasPrefix(fields[2], "refs/") {
			s.writePushReport(w, false, "malformed command", nil)
			return
		}
		commands = append(commands, pushCommand{oldID: oldID, newID: newID, ref: fields[2]})
	}
	if len(commands) == 0 {
		s.writePushReport(w, false, "no commands", nil)
		return
	}
	var pack []byte
	if packStart >= 0 && packStart < len(body) {
		pack = body[packStart:]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Unpack objects first (even for refused refs, like git).
	unpackOK := true
	unpackDetail := "ok"
	if len(pack) > 0 {
		incoming, err := readPack(pack, repo.Objects)
		if err != nil {
			unpackOK = false
			unpackDetail = "unpack failed: " + err.Error()
		} else {
			for id, o := range incoming {
				repo.Objects[id] = o
			}
		}
	}
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]result, 0, len(commands))
	changed := false
	for _, cmd := range commands {
		if !unpackOK {
			results = append(results, result{ref: cmd.ref, ok: false, reason: "unpack failed"})
			continue
		}
		cur, exists := repo.Refs[cmd.ref]
		zero := cmd.oldID == zeroOID
		del := cmd.newID == zeroOID
		switch {
		case del:
			if !exists {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if !zero && cmd.oldID != cur {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.ref)
				changed = true
				results = append(results, result{ref: cmd.ref, ok: true})
			}
		case zero:
			if exists {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "already exists"})
			} else if _, ok := repo.Objects[cmd.newID]; !ok {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "object not found"})
			} else {
				repo.Refs[cmd.ref] = cmd.newID
				changed = true
				results = append(results, result{ref: cmd.ref, ok: true})
			}
		default:
			if !exists {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "remote ref does not exist"})
			} else if cmd.oldID != cur {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else if _, ok := repo.Objects[cmd.newID]; !ok {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "object not found"})
			} else if !repo.isAncestor(cmd.oldID, cmd.newID) {
				results = append(results, result{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				repo.Refs[cmd.ref] = cmd.newID
				changed = true
				results = append(results, result{ref: cmd.ref, ok: true})
			}
		}
	}
	if changed {
		repo.packCache = map[string][]byte{}
	}
	// Build the report-status stream, pkt-lined inside sideband band 1.
	// The report's own terminating flush lives INSIDE the band-1 data
	// (real git's response is sideband(report + flush) + flush); a flush
	// outside the sideband packet makes git die with "hung up".
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktLine([]byte("unpack ok\n")))
	} else {
		report.Write(pktLine([]byte("unpack " + unpackDetail + "\n")))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLine([]byte("ok " + res.ref + "\n")))
		} else {
			report.Write(pktLine([]byte("ng " + res.ref + " " + res.reason + "\n")))
		}
	}
	report.Write(pktFlush())
	var out bytes.Buffer
	for _, pkt := range sidebandPackets(report.Bytes()) {
		out.Write(pkt)
	}
	out.Write(pktFlush())
	w.Write(out.Bytes())
}

// writePushReport answers a malformed receive-pack with a sideband report
// (the server stays alive; the suite then proves a healthy clone).
// The report's terminating flush lives inside the band-1 data.
func (s *store) writePushReport(w http.ResponseWriter, ok bool, detail string, _ any) {
	var report bytes.Buffer
	if ok {
		report.Write(pktLine([]byte("unpack ok\n")))
	} else {
		report.Write(pktLine([]byte("unpack " + detail + "\n")))
	}
	report.Write(pktFlush())
	var out bytes.Buffer
	for _, pkt := range sidebandPackets(report.Bytes()) {
		out.Write(pkt)
	}
	out.Write(pktFlush())
	w.Write(out.Bytes())
}
