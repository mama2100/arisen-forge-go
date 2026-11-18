// gitobj.go: git object model — SHA-1 hashing, tree/commit parse and build, packfile
// read (with OFS/REF deltas) and write. Ported from the entry's TS implementation.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	OID  string
	Type ObjectType
	Data []byte
}

func IsObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func HashObject(t ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", t, len(data))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ---------------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	OID  string
	Type string // "blob" | "tree"
}

func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	at := 0
	for at < len(data) {
		space := at
		for space < len(data) && data[space] != 0x20 {
			space++
		}
		mode := string(data[at:space])
		nul := space + 1
		for nul < len(data) && data[nul] != 0x00 {
			nul++
		}
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := fmt.Sprintf("%x", data[nul+1:nul+21])
		at = nul + 21
		t := "blob"
		if strings.HasPrefix(mode, "4") || mode == "040000" {
			t = "tree"
		}
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid, Type: t})
	}
	return entries
}

func treeNameKey(name string, isTree bool) string {
	if isTree {
		return name + "/"
	}
	return name
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			a := treeNameKey(sorted[i].Name, sorted[i].Type == "tree")
			b := treeNameKey(sorted[j].Name, sorted[j].Type == "tree")
			if a > b {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	var buf bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if e.Type == "tree" {
			mode = "40000"
		} else if mode == "" {
			mode = "100644"
		} else if len(mode) > 0 && mode[0] == '0' {
			mode = mode[1:]
		}
		fmt.Fprintf(&buf, "%s %s\x00", mode, e.Name)
		oidBytes, _ := hexBytes(e.OID)
		buf.Write(oidBytes)
	}
	return buf.Bytes()
}

func hexBytes(oid string) ([]byte, error) {
	if len(oid) != 40 {
		return nil, errors.New("bad oid")
	}
	out := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hi := hexVal(oid[i*2])
		lo := hexVal(oid[i*2+1])
		if hi < 0 || lo < 0 {
			return nil, errors.New("bad hex")
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

// ---------------------------------------------------------------- commits

type CommitView struct {
	Tree          string
	Parents       []string
	Author        string
	Committer     string
	Message       string
	AuthorName    string
	AuthorEmail   string
	CommitterName string
	CommitterEmail string
}

func ParseCommit(data []byte) CommitView {
	text := string(data)
	head, message := text, ""
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head = text[:idx]
		message = text[idx+2:]
	}
	var view CommitView
	view.Message = message
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			view.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			view.Parents = append(view.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			view.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			view.Committer = line[10:]
		}
	}
	an, ae := parsePerson(view.Author)
	cn, ce := parsePerson(view.Committer)
	view.AuthorName, view.AuthorEmail = an, ae
	view.CommitterName, view.CommitterEmail = cn, ce
	return view
}

func parsePerson(line string) (string, string) {
	lt := strings.Index(line, " <")
	gt := strings.Index(line, "> ")
	if lt < 0 || gt < 0 {
		return strings.TrimSpace(line), ""
	}
	return line[:lt], line[lt+2 : gt]
}

type BuildCommitFields struct {
	Tree           string
	Parents        []string
	AuthorName     string
	CommitterName  string
	Message        string
	Timestamp      int64
}

func BuildCommit(f BuildCommitFields) []byte {
	ts := f.Timestamp
	if ts == 0 {
		ts = 1735689600
	}
	name := f.AuthorName
	if name == "" {
		name = "forge"
	}
	cname := f.CommitterName
	if cname == "" {
		cname = name
	}
	var lines []string
	lines = append(lines, "tree "+f.Tree)
	for _, p := range f.Parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, fmt.Sprintf("author %s <%s@example.invalid> %d +0000", name, name, ts))
	lines = append(lines, fmt.Sprintf("committer %s <%s@example.invalid> %d +0000", cname, cname, ts))
	return []byte(strings.Join(lines, "\n") + "\n\n" + f.Message)
}

// ---------------------------------------------------------------- packs: read

type PackReadResult struct {
	Objects map[string]GitObject
	Deltas  int
	Count   int
}

func inflateAt(buf []byte, at int) ([]byte, int, error) {
	br := bytes.NewReader(buf[at:])
	counting := &countingReader{r: br}
	zr, err := zlib.NewReader(counting)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		// tolerate trailing garbage: Go's zlib reader stops at stream end with ErrUnexpectedEOF
		// only when the stream itself is truncated
		if counting.n == 0 {
			return nil, 0, err
		}
	}
	return data, counting.n, nil
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func readVarintSize(buf []byte, start int) (size int, next int, first int, err error) {
	at := start
	index := 0
	shift := uint(0)
	for {
		if at >= len(buf) {
			return 0, 0, 0, errors.New("truncated varint")
		}
		b := buf[at]
		if index == 0 {
			first = int(b>>4) & 0x07
			size = int(b & 0x0f)
			shift = 4
		} else {
			size |= int(b&0x7f) << shift
			shift += 7
		}
		at++
		index++
		if b&0x80 == 0 {
			break
		}
	}
	return size, at, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (base int, next int, err error) {
	at := start
	if at >= len(buf) {
		return 0, 0, errors.New("truncated ofs-delta")
	}
	v := int(buf[at] & 0x7f)
	at++
	for at < len(buf) && buf[at]&0x80 != 0 {
		v = ((v + 1) << 7) | int(buf[at]&0x7f)
		at++
	}
	return v, at, nil
}

func ApplyDelta(base, delta []byte) []byte {
	at := 0
	readVarint := func() int {
		v, shift := 0, uint(0)
		for {
			b := delta[at]
			at++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v
	}
	readVarint() // base size
	resultSize := readVarint()
	out := make([]byte, 0, resultSize)
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			var offset, length int
			if op&0x01 != 0 {
				offset |= int(delta[at])
				at++
			}
			if op&0x02 != 0 {
				offset |= int(delta[at]) << 8
				at++
			}
			if op&0x04 != 0 {
				offset |= int(delta[at]) << 16
				at++
			}
			if op&0x08 != 0 {
				offset |= int(delta[at]) << 24
				at++
			}
			if op&0x10 != 0 {
				length |= int(delta[at])
				at++
			}
			if op&0x20 != 0 {
				length |= int(delta[at]) << 8
				at++
			}
			if op&0x40 != 0 {
				length |= int(delta[at]) << 16
				at++
			}
			if length == 0 {
				length = 0x10000
			}
			out = append(out, base[offset:offset+length]...)
		} else {
			length := int(op & 0x7f)
			out = append(out, delta[at:at+length]...)
			at += length
		}
	}
	return out
}

var typeNumbers = map[byte]ObjectType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

func ReadPack(pack []byte) (*PackReadResult, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	count := binary.BigEndian.Uint32(pack[8:12])
	res := &PackReadResult{Objects: map[string]GitObject{}, Count: int(count)}
	type rawEntry struct {
		typ     byte
		data    []byte
		oid     string
		baseRef string
		baseOfs int
		hasOfs  bool
	}
	entries := make([]rawEntry, 0, count)
	offsetIndex := map[int]int{}
	at := 12
	for i := 0; i < int(count); i++ {
		offsetIndex[at] = i
		_, next, first, err := readVarintSize(pack, at)
		if err != nil {
			return nil, err
		}
		at = next
		var e rawEntry
		e.typ = byte(first)
		if t, ok := typeNumbers[byte(first)]; ok {
			data, consumed, err := inflateAt(pack, at)
			if err != nil {
				return nil, fmt.Errorf("inflate object %d: %w", i, err)
			}
			at += consumed
			oid := HashObject(t, data)
			res.Objects[oid] = GitObject{OID: oid, Type: t, Data: data}
			e.data = data
			e.oid = oid
		} else {
			res.Deltas++
			if first == 7 {
				e.baseRef = fmt.Sprintf("%x", pack[at:at+20])
				at += 20
			} else if first == 6 {
				base, next2, err := readOfsDeltaBase(pack, at)
				if err != nil {
					return nil, err
				}
				e.baseOfs = base
				e.hasOfs = true
				at = next2
			} else {
				return nil, fmt.Errorf("bad pack object type %d", first)
			}
			data, consumed, err := inflateAt(pack, at)
			if err != nil {
				return nil, fmt.Errorf("inflate delta %d: %w", i, err)
			}
			at += consumed
			e.data = data
		}
		entries = append(entries, e)
	}
	_ = version
	changed := true
	for changed {
		changed = false
		for i := range entries {
			e := &entries[i]
			if e.oid != "" {
				continue
			}
			var baseOID string
			if e.baseRef != "" {
				baseOID = e.baseRef
			} else if e.hasOfs {
				if idx, ok := offsetIndex[e.baseOfs]; ok && entries[idx].oid != "" {
					baseOID = entries[idx].oid
				}
			}
			if baseOID == "" {
				continue
			}
			base, ok := res.Objects[baseOID]
			if !ok {
				continue
			}
			data := ApplyDelta(base.Data, e.data)
			oid := HashObject(base.Type, data)
			res.Objects[oid] = GitObject{OID: oid, Type: base.Type, Data: data}
			e.oid = oid
			changed = true
		}
	}
	return res, nil
}

func VerifyPackChecksum(pack []byte) bool {
	if len(pack) < 21 {
		return false
	}
	h := sha1.Sum(pack[:len(pack)-20])
	return fmt.Sprintf("%x", h) == fmt.Sprintf("%x", pack[len(pack)-20:])
}

// ---------------------------------------------------------------- packs: write

func writePackObjectHeader(t ObjectType, size int) []byte {
	tn := map[ObjectType]byte{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}[t]
	var out []byte
	b := int(tn<<4) | (size & 0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, byte(b|0x80))
		b = size & 0x7f
		size >>= 7
	}
	out = append(out, byte(b))
	return out
}

func WritePack(objects []GitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var h [8]byte
	binary.BigEndian.PutUint32(h[0:4], 2)
	binary.BigEndian.PutUint32(h[4:8], uint32(len(objects)))
	buf.Write(h[:])
	for _, o := range objects {
		buf.Write(writePackObjectHeader(o.Type, len(o.Data)))
		zw := zlib.NewWriter(&buf)
		zw.Write(o.Data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func CollectReachable(roots []string, lookup func(string) (GitObject, bool), have map[string]bool) map[string]GitObject {
	out := map[string]GitObject{}
	seen := map[string]bool{}
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := lookup(oid)
		if !ok {
			continue
		}
		if !have[oid] {
			out[oid] = object
		}
		switch object.Type {
		case ObjCommit:
			v := ParseCommit(object.Data)
			queue = append(queue, v.Tree)
			queue = append(queue, v.Parents...)
		case ObjTree:
			for _, e := range ParseTree(object.Data) {
				queue = append(queue, e.OID)
			}
		case ObjTag:
			lines := strings.SplitN(string(object.Data), "\n", 2)
			if len(lines) > 0 && strings.HasPrefix(lines[0], "object ") {
				queue = append(queue, strings.TrimSpace(lines[0][7:]))
			}
		}
	}
	return out
}
