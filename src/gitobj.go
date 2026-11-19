// gitobj.go: git object storage — SHA-1 object ids, packfile read/write,
// delta application (OBJ_OFS_DELTA / OBJ_REF_DELTA). No git binary is used.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Git object type numbers (packfile).
const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var zeroOID = [20]byte{}

// obj is one git object: its type name and raw content (without the header).
type obj struct {
	typ  string
	data []byte
}

func objTypeNum(typ string) int {
	switch typ {
	case "commit":
		return objCommit
	case "tree":
		return objTree
	case "blob":
		return objBlob
	case "tag":
		return objTag
	}
	return 0
}

func objTypeName(n int) string {
	switch n {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

// hashObject returns the SHA-1 id of "<type> <len>\0<data>".
// Written without fmt: this is called for every object stored or read.
func hashObject(typ string, data []byte) [20]byte {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	var num [20]byte
	h.Write(strconv.AppendInt(num[:0], int64(len(data)), 10))
	h.Write([]byte{0})
	h.Write(data)
	var id [20]byte
	h.Sum(id[:0])
	return id
}

const hexDigits = "0123456789abcdef"

// oidHex renders an oid as 40 lowercase hex chars, without fmt.Sprintf.
func oidHex(id [20]byte) string {
	var buf [40]byte
	for i, b := range id {
		buf[2*i] = hexDigits[b>>4]
		buf[2*i+1] = hexDigits[b&0x0f]
	}
	return string(buf[:])
}

func parseOIDHex(s string) ([20]byte, bool) {
	var id [20]byte
	if len(s) != 40 {
		return id, false
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := hexVal(s[2*i])
		lo, ok2 := hexVal(s[2*i+1])
		if !ok1 || !ok2 {
			return id, false
		}
		id[i] = hi<<4 | lo
	}
	return id, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// --- packfile writing (full objects only, no deltas; always valid) ---

// writePack encodes objects as a v2 packfile. The caller passes objects in any
// order; the pack is sorted by oid for determinism.
func writePack(objects map[[20]byte]obj) []byte {
	keys := make([][20]byte, 0, len(objects))
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) > 0 })
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(keys)))
	buf.Write(hdr[:])
	for _, id := range keys {
		o := objects[id]
		writePackObject(&buf, o)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writePackObject(buf *bytes.Buffer, o obj) {
	typ := objTypeNum(o.typ)
	size := len(o.data)
	// varint: first byte holds 4 low bits of size + type in bits 4..6.
	first := byte((size & 0x0f) | (typ << 4))
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	buf.WriteByte(first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(o.data)
	w.Close()
	buf.Write(z.Bytes())
}

// --- packfile reading (full objects + both delta kinds) ---

// readPack parses a packfile and returns its objects keyed by oid. Delta
// objects are resolved against bases in the same pack or in `base` (objects
// the server already holds).
func readPack(data []byte, base map[[20]byte]obj) (map[[20]byte]obj, error) {
	if len(data) < 12 || string(data[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(data[8:12])
	out := make(map[[20]byte]obj, count)
	// Deferred deltas: offsets in the pack, resolved after all full objects.
	type pending struct {
		typ    int
		baseID [20]byte // for REF deltas
		baseOf int64    // for OFS deltas (negative offset)
		delta  []byte
		pos    int64
	}
	var pend []pending
	// OFS delta bases are addressed by pack offset: record each full
	// object's start offset -> oid as it is read (no second pass).
	offsetToOID := map[int64][20]byte{}
	pos := int64(12)
	for i := uint32(0); i < count; i++ {
		start := pos
		b := data[pos]
		pos++
		typ := int((b >> 4) & 0x7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = data[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var p pending
		p.pos = start
		switch typ {
		case objOfsDelta:
			p.typ = typ
			b = data[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			p.baseOf = off
		case objRefDelta:
			p.typ = typ
			copy(p.baseID[:], data[pos:pos+20])
			pos += 20
		}
		// Inflate once: inflateZlib returns the payload and the exact
		// number of bytes the zlib stream occupies (the bytes.Reader
		// position after the stream ends).
		raw, consumed, err := inflateZlib(data[pos:])
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != size {
			return nil, fmt.Errorf("pack object size mismatch: got %d want %d", len(raw), size)
		}
		pos += consumed
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			id := hashObject(objTypeName(typ), raw)
			out[id] = obj{typ: objTypeName(typ), data: raw}
			offsetToOID[start] = id
		case objOfsDelta, objRefDelta:
			p.delta = raw
			pend = append(pend, p)
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
	}
	// Resolve pending deltas in order (a delta's base may itself be a delta).
	resolved := map[int64][20]byte{} // pack offset of delta -> resolved oid
	lookup := func(p pending) (obj, bool) {
		if p.typ == objRefDelta {
			if o, ok := out[p.baseID]; ok {
				return o, true
			}
			if base != nil {
				if o, ok := base[p.baseID]; ok {
					return o, true
				}
			}
			return obj{}, false
		}
		baseOff := p.pos - p.baseOf
		baseID, ok := offsetToOID[baseOff]
		if !ok {
			if id, ok2 := resolved[baseOff]; ok2 {
				baseID = id
				ok = true
			}
		}
		if !ok {
			return obj{}, false
		}
		if o, ok := out[baseID]; ok {
			return o, true
		}
		if base != nil {
			if o, ok := base[baseID]; ok {
				return o, true
			}
		}
		return obj{}, false
	}
	for len(pend) > 0 {
		progress := false
		var rest []pending
		for _, p := range pend {
			b, ok := lookup(p)
			if !ok {
				rest = append(rest, p)
				continue
			}
			raw, err := applyDelta(b.data, p.delta)
			if err != nil {
				return nil, err
			}
			id := hashObject(b.typ, raw)
			out[id] = obj{typ: b.typ, data: raw}
			resolved[p.pos] = id
			offsetToOID[p.pos] = id
			progress = true
		}
		if !progress {
			return nil, errors.New("pack delta with unresolvable base")
		}
		pend = rest
	}
	return out, nil
}

// inflateZlib inflates one zlib stream starting at data[0] and returns the
// payload plus the exact number of bytes the stream occupies. It inflates
// exactly once: the zlib reader is built over a *bytes.Reader, which
// implements io.ByteReader, so flate reads the stream byte-by-byte without
// reading ahead — after ReadAll, the reader's position is exactly the end
// of the stream (header + DEFLATE data + Adler-32).
func inflateZlib(data []byte) ([]byte, int64, error) {
	if len(data) == 0 {
		return nil, 0, errors.New("truncated zlib stream")
	}
	br := bytes.NewReader(data)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	pos, _ := br.Seek(0, io.SeekCurrent)
	return raw, pos, nil
}

// applyDelta applies a git delta to base data.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	baseSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if baseSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size %d != %d", baseSize, len(base))
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	// Write straight into the final slice: the result size is known up
	// front, so no buffer growth or copy is needed.
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					off |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					size |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(len(out)) != resultSize {
		return nil, fmt.Errorf("delta result size %d != %d", len(out), resultSize)
	}
	return out, nil
}

// --- tree / commit builders (mirrors the kit's git-client helpers) ---

// buildTree encodes tree entries: each is {mode, name, oid}. Entries are
// sorted git's way: a directory sorts as if its name ended with '/'.
func buildTree(entries []treeEntry) []byte {
	sort.Slice(entries, func(i, j int) bool { return treeSortKey(entries[i]) < treeSortKey(entries[j]) })
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(e.oid[:])
	}
	return buf.Bytes()
}

func treeSortKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

type treeEntry struct {
	mode string
	name string
	oid  [20]byte
}

// parseTree decodes a tree object into entries.
func parseTree(data []byte) ([]treeEntry, error) {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errors.New("bad tree entry")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			return nil, errors.New("bad tree entry")
		}
		name := string(data[:nul])
		var oid [20]byte
		copy(oid[:], data[nul+1:nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		data = data[nul+21:]
	}
	return out, nil
}

// buildCommit encodes a commit object.
func buildCommit(tree [20]byte, parents [][20]byte, author, committer string, epochSec int64, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree ")
	buf.WriteString(oidHex(tree))
	buf.WriteByte('\n')
	for _, p := range parents {
		buf.WriteString("parent ")
		buf.WriteString(oidHex(p))
		buf.WriteByte('\n')
	}
	var num [20]byte
	buf.WriteString("author ")
	buf.WriteString(author)
	buf.WriteByte(' ')
	buf.Write(strconv.AppendInt(num[:0], epochSec, 10))
	buf.WriteString(" +0000\n")
	buf.WriteString("committer ")
	buf.WriteString(committer)
	buf.WriteByte(' ')
	buf.Write(strconv.AppendInt(num[:0], epochSec, 10))
	buf.WriteString(" +0000\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

// parseCommitHeader extracts tree + parents from a commit object.
func parseCommitHeader(data []byte) (tree [20]byte, parents [][20]byte, err error) {
	head := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("tree ")) {
			id, ok := parseOIDHex(string(line[5:]))
			if !ok {
				return tree, nil, errors.New("bad commit tree line")
			}
			tree = id
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			id, ok := parseOIDHex(string(line[7:]))
			if !ok {
				return tree, nil, errors.New("bad commit parent line")
			}
			parents = append(parents, id)
		}
	}
	return tree, parents, nil
}
