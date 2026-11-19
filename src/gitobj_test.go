package main

import (
	"bytes"
	"testing"
)

func TestPackRoundTrip(t *testing.T) {
	blob := []byte("hello world\n")
	blobID := hashObject("blob", blob)
	tree := buildTree([]treeEntry{{mode: "100644", name: "hello.txt", oid: blobID}})
	treeID := hashObject("tree", tree)
	commit := buildCommit(treeID, nil, "Ada <ada@example.invalid>", "Ada <ada@example.invalid>", 1700000000, "initial\n")
	commitID := hashObject("commit", commit)
	objs := map[[20]byte]obj{
		blobID:   {typ: "blob", data: blob},
		treeID:   {typ: "tree", data: tree},
		commitID: {typ: "commit", data: commit},
	}
	pack := writePack(objs)
	back, err := readPack(pack, nil)
	if err != nil {
		t.Fatalf("readPack: %v", err)
	}
	if len(back) != 3 {
		t.Fatalf("got %d objects, want 3", len(back))
	}
	for id, want := range objs {
		got, ok := back[id]
		if !ok {
			t.Fatalf("missing object %s", oidHex(id))
		}
		if got.typ != want.typ || !bytes.Equal(got.data, want.data) {
			t.Fatalf("object %s mismatch", oidHex(id))
		}
	}
}

func TestDeltaApply(t *testing.T) {
	base := []byte("The quick brown fox jumps over the lazy dog.")
	// Build a delta by hand: copy "The quick brown fox " (20 bytes), insert "red ", copy "jumps over the lazy dog." (24 bytes).
	// base size 44, result size 48.
	delta := []byte{
		44,       // base size varint
		48,       // result size varint
		0x91, 0, 20, // copy offset 0, size 20
		4, 'r', 'e', 'd', ' ', // insert "red "
		0x91, 20, 24, // copy offset 20, size 24
	}
	out, err := applyDelta(base, delta)
	if err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	want := "The quick brown fox red jumps over the lazy dog."
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestTreeParseRoundTrip(t *testing.T) {
	blobID := hashObject("blob", []byte("x"))
	entries := []treeEntry{
		{mode: "100644", name: "b.txt", oid: blobID},
		{mode: "100644", name: "a.txt", oid: blobID},
		{mode: "40000", name: "src", oid: blobID},
	}
	raw := buildTree(entries)
	parsed, err := parseTree(raw)
	if err != nil {
		t.Fatalf("parseTree: %v", err)
	}
	if len(parsed) != 3 || parsed[0].name != "a.txt" || parsed[1].name != "b.txt" || parsed[2].name != "src" {
		t.Fatalf("unexpected parse order: %+v", parsed)
	}
}
