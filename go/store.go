// The forge's in-memory store: git objects, refs, repositories, issues and
// pull requests, held for the life of the process. Nothing here touches the
// disk during a request; a replay rebuilds the same state from the same
// requests.
//
// Identity is fixed by the trial's seeded scenario: the bearer token names
// one login ("adaiversen"), and every repository belongs to that login. Any
// other owner in a URL is a 404, never a 403, so repositories are keyed by
// name alone and the owner is checked once at routing time.
//
// Clocks are fixed too: server-made commits and timestamps use
// MAKEFUN_TRIAL_NOW (or a constant fallback), never the wall clock, so a
// live run and its replay produce byte-identical commits.
//
// All Forge methods assume the caller holds Forge.mu (the dispatcher locks
// once per request); nothing here locks on its own.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"hash/adler32"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ViewerLogin is the login the trial's bearer token names.
const ViewerLogin = "adaiversen"

// ViewerID is the viewer's numeric user id (the scenario's first user).
const ViewerID = 1000

// ForgeIdentity is the identity server-made commits carry.
const ForgeIdentity = "adaiversen <adaiversen@example.invalid>"

// FallbackEpochSec is the fixed epoch used when MAKEFUN_TRIAL_NOW is absent.
const FallbackEpochSec = 1700000000

// ZeroOID is the all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

func trialNowSec() int64 {
	raw := strings.TrimSpace(os.Getenv("MAKEFUN_TRIAL_NOW"))
	if raw == "" {
		return FallbackEpochSec
	}
	allDigits := true
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			return ms / 1000
		}
		return FallbackEpochSec
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Unix()
		}
	}
	return FallbackEpochSec
}

// TrialEpochSec is the trial's fixed now, epoch seconds.
var TrialEpochSec = trialNowSec()

// TrialISO is the trial's fixed now as an ISO instant (millis + Z, like
// JavaScript's Date.toISOString).
var TrialISO = time.Unix(TrialEpochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")

// ObjKind is a git object type. The values match the packfile type codes.
type ObjKind uint8

const (
	KindCommit ObjKind = 1
	KindTree   ObjKind = 2
	KindBlob   ObjKind = 3
	KindTag    ObjKind = 4
)

func (k ObjKind) name() string {
	switch k {
	case KindCommit:
		return "commit"
	case KindTree:
		return "tree"
	case KindBlob:
		return "blob"
	default:
		return "tag"
	}
}

func kindFromCode(code int) (ObjKind, bool) {
	switch code {
	case 1:
		return KindCommit, true
	case 2:
		return KindTree, true
	case 3:
		return KindBlob, true
	case 4:
		return KindTag, true
	}
	return 0, false
}

// GitObject is one stored git object.
type GitObject struct {
	Kind ObjKind
	Data []byte
}

// IsOID reports whether s is a 40-char lowercase hex object id.
func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !ok {
			return false
		}
	}
	return true
}

// HashObject returns git's object id: SHA-1 over "<type> <size>\0" + bytes.
func HashObject(kind ObjKind, data []byte) string {
	h := sha1.New()
	h.Write([]byte(kind.name()))
	h.Write([]byte(" "))
	h.Write(strconv.AppendInt(nil, int64(len(data)), 10))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// sha1Hex returns the SHA-1 of data as hex.
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// object codecs
// ---------------------------------------------------------------------------

// TreeEntry is one entry of a tree object, in stored order.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object into its entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errBadTree
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errBadTree
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errBadTree
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

var errBadTree = errString("tree entry is malformed")

type errString string

func (e errString) Error() string { return string(e) }

// treeLess is git's tree-entry sort: byte order of the name, a directory
// compared as "name/".
func treeLess(a, b TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return an < bn
}

// BuildTree builds a tree object's bytes from entries, sorted git's way.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeLess(sorted[i], sorted[j]) })
	var out []byte
	for _, e := range sorted {
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit parses a commit object.
func ParseCommit(data []byte) CommitView {
	var v CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		v.Message = string(data[blank+2:])
	}
	rest := data[:headerEnd]
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		key := string(line)
		val := ""
		if sp >= 0 {
			key, val = string(line[:sp]), string(line[sp+1:])
		}
		switch key {
		case "tree":
			v.Tree = val
		case "parent":
			v.Parents = append(v.Parents, val)
		case "author":
			v.Author = val
		case "committer":
			v.Committer = val
		}
	}
	return v
}

// BuildCommitBytes builds a commit object's bytes. The message is verbatim.
func BuildCommitBytes(tree string, parents []string, message string, timestamp int64) []byte {
	who := ForgeIdentity + " " + strconv.FormatInt(timestamp, 10) + " +0000"
	var b bytes.Buffer
	b.WriteString("tree ")
	b.WriteString(tree)
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(who)
	b.WriteString("\ncommitter ")
	b.WriteString(who)
	b.WriteString("\n\n")
	b.WriteString(message)
	return b.Bytes()
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Kind    ObjKind
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses an annotated tag object.
func ParseTag(data []byte) TagView {
	var v TagView
	v.Kind = KindCommit
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		v.Message = string(data[blank+2:])
	}
	rest := data[:headerEnd]
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "object":
			v.Object = val
		case "type":
			switch val {
			case "commit":
				v.Kind = KindCommit
			case "tree":
				v.Kind = KindTree
			case "blob":
				v.Kind = KindBlob
			case "tag":
				v.Kind = KindTag
			}
		case "tag":
			v.Tag = val
		case "tagger":
			v.Tagger = val
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// packfile codec (plain objects only: the suite's client never sends deltas)
// ---------------------------------------------------------------------------

// Pooled zlib codecs: a fresh flate compressor allocates ~200KB of tables,
// and a fresh decompressor ~32KB, so creating one per pack object showed up
// as the dominant cost in the cold-fetch profile (memclr + GC churn). The
// pools reuse the codec state across objects; Reset re-points a codec at a
// new stream without reallocating its tables.
var zlibWriterPool = sync.Pool{New: func() any {
	return zlib.NewWriter(io.Discard)
}}

// zlibResetter is the Reset method the concrete zlib reader implements.
type zlibResetter interface {
	io.ReadCloser
	Reset(r io.Reader, dict []byte) error
}

var zlibReaderPool = sync.Pool{New: func() any {
	// NewReader needs a valid stream to read its header from; an empty
	// stream produced by the library itself is the cheapest valid one.
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Close()
	r, err := zlib.NewReader(&buf)
	if err != nil {
		return nil
	}
	return r
}}

// PackInput is one object to write into a packfile.
type PackInput struct {
	Kind ObjKind
	Data []byte
}

func packObjectHeader(kind ObjKind, size int) []byte {
	var out []byte
	first := byte(kind)<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

// WritePack builds a packfile from plain objects (no deltas), deduplicated
// by id and sorted by id for determinism.
func WritePack(objects []PackInput) []byte {
	byOID := make(map[string]PackInput, len(objects))
	for _, o := range objects {
		oid := HashObject(o.Kind, o.Data)
		if _, ok := byOID[oid]; !ok {
			byOID[oid] = o
		}
	}
	return writePackKnown(byOID)
}

// appendStoredZlib appends a zlib stream holding data as stored
// (uncompressed) deflate blocks. The trial link is loopback, so bytes are
// nearly free and CPU is the constraint: skipping the Huffman/deflate pass
// is a pure win for the small objects a fetch packs. The stream is valid
// zlib (header 0x78 0x01, stored blocks of at most 65535 bytes, Adler-32
// trailer) and any inflater reads it.
func appendStoredZlib(out []byte, data []byte) []byte {
	sum := adler32.Checksum(data)
	out = append(out, 0x78, 0x01)
	if len(data) == 0 {
		out = append(out, 0x01, 0, 0, 0xff, 0xff)
	} else {
		for len(data) > 0 {
			n := len(data)
			if n > 65535 {
				n = 65535
			}
			final := byte(0x00)
			if n == len(data) {
				final = 0x01
			}
			out = append(out, final, byte(n), byte(n>>8), byte(^n), byte(^n>>8))
			out = append(out, data[:n]...)
			data = data[n:]
		}
	}
	return append(out, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
}

// writePackKnown builds a packfile from objects whose ids are already known
// (the fetch path's reachability walk already computed them, so no re-hash).
// Objects are sorted by id for determinism. Object bytes go in as stored
// zlib blocks (see appendStoredZlib): no deflate, no codec allocation, one
// pre-sized buffer for the whole pack.
func writePackKnown(byOID map[string]PackInput) []byte {
	oids := make([]string, 0, len(byOID))
	size := 12 + 20
	for oid, o := range byOID {
		oids = append(oids, oid)
		size += len(o.Data) + 24
	}
	sort.Strings(oids)
	out := make([]byte, 0, size)
	out = append(out, "PACK"...)
	out = append(out, 0, 0, 0, 2)
	n := len(oids)
	out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	for _, oid := range oids {
		o := byOID[oid]
		out = append(out, packObjectHeader(o.Kind, len(o.Data))...)
		out = appendStoredZlib(out, o.Data)
	}
	sum := sha1.Sum(out)
	out = append(out, sum[:]...)
	return out
}

// ReadPack reads a packfile of plain objects, keyed by recomputed id. A delta
// entry or a corrupt stream is an error (the suite's client writes plain
// packs only). The trailer SHA-1 is verified first.
func ReadPack(pack []byte) (map[string]GitObject, error) {
	if len(pack) < 32 {
		return nil, errString("pack is too short")
	}
	if !bytes.Equal(pack[:4], []byte("PACK")) {
		return nil, errString("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, errString("unsupported pack version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return nil, errString("pack trailer does not match its bytes")
	}
	objects := make(map[string]GitObject, count)
	off := 12
	for i := 0; i < count; i++ {
		if off >= trailerStart {
			return nil, errString("pack ends before its declared count")
		}
		first := pack[off]
		off++
		size := int(first & 0x0f)
		shift := uint(4)
		b := first
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, errString("pack entry header is truncated")
			}
			b = pack[off]
			off++
			size += int(b&0x7f) << shift
			shift += 7
		}
		code := (first >> 4) & 0x07
		if code == 6 || code == 7 {
			return nil, errString("pack carries delta objects, which this forge does not unpack")
		}
		kind, ok := kindFromCode(int(code))
		if !ok {
			return nil, errString("pack entry has unknown type code")
		}
		// The declared size bounds the allocation below; a pack that
		// claims a bigger object than the whole body allows is bogus
		// (and a huge claim would OOM the allocation before any
		// decompression happens).
		if size > maxBodyBytes {
			return nil, errString("pack entry declares an object larger than the body limit")
		}
		// Inflate from a bytes.Reader through a pooled zlib reader: the
		// reader's remaining length tells us exactly how many compressed
		// bytes the entry consumed. The buffer is pre-sized from the
		// header's declared size (plus one byte, so an over-long stream
		// is detected rather than silently truncated).
		r := bytes.NewReader(pack[off:trailerStart])
		var zr zlibResetter
		if pooled := zlibReaderPool.Get(); pooled != nil {
			zr = pooled.(zlibResetter)
			if err := zr.Reset(r, nil); err != nil {
				// A failed Reset may leave the reader unusable; drop it
				// rather than poisoning the pool.
				return nil, err
			}
		} else {
			fresh, err := zlib.NewReader(r)
			if err != nil {
				return nil, err
			}
			zr = fresh.(zlibResetter)
		}
		buf := make([]byte, size+1)
		n, err := io.ReadFull(zr, buf)
		zr.Close()
		zlibReaderPool.Put(zr)
		if err == nil {
			return nil, errString("pack entry inflated past its declared size")
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		if n != size {
			return nil, errString("pack entry inflated to the wrong size")
		}
		data := buf[:size]
		consumed := (trailerStart - off) - r.Len()
		off += consumed
		oid := HashObject(kind, data)
		if _, dup := objects[oid]; !dup {
			objects[oid] = GitObject{Kind: kind, Data: data}
		}
	}
	if off != trailerStart {
		return nil, errString("pack has stray bytes before its trailer")
	}
	return objects, nil
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

// Lookup resolves an oid to its object, or nil.
type Lookup func(oid string) *GitObject

// CollectReachable returns every object reachable from roots, following
// commit -> tree + parents, tag -> target, tree -> entries (gitlinks
// skipped). maxGen caps how many commit generations from a root are
// followed; trees and blobs of an allowed commit are always included.
// When a depth cap is given, shallow holds the boundary commits (allowed
// commits at the cap that still have parents). stopAt marks commits whose
// parents are not followed (a client's shallow boundary).
func CollectReachable(roots []string, lookup Lookup, maxGen int, stopAt map[string]bool) (map[string]bool, []string) {
	seen := make(map[string]bool)
	var shallow []string
	var qOids []string
	var qGens []int
	for _, root := range roots {
		if IsOID(root) {
			qOids = append(qOids, root)
			qGens = append(qGens, 1)
		}
	}
	for len(qOids) > 0 {
		oid := qOids[len(qOids)-1]
		gen := qGens[len(qGens)-1]
		qOids = qOids[:len(qOids)-1]
		qGens = qGens[:len(qGens)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Kind {
		case KindCommit:
			c := ParseCommit(obj.Data)
			if stopAt != nil && stopAt[oid] {
				// Held boundary: tree and blobs are held, parents are not.
			} else if maxGen > 0 && gen >= maxGen {
				if len(c.Parents) > 0 {
					shallow = append(shallow, oid)
				}
			} else {
				for _, p := range c.Parents {
					qOids = append(qOids, p)
					qGens = append(qGens, gen+1)
				}
			}
			qOids = append(qOids, c.Tree)
			qGens = append(qGens, gen)
		case KindTag:
			t := ParseTag(obj.Data)
			qOids = append(qOids, t.Object)
			qGens = append(qGens, gen)
		case KindTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				qOids = append(qOids, e.OID)
				qGens = append(qGens, gen)
			}
		}
	}
	return seen, shallow
}

// MergeBase returns the first common ancestor of two commits, or "".
func MergeBase(a, b string, lookup Lookup) string {
	ancestors := make(map[string]int)
	stack := []string{a}
	dists := []int{0}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		d := dists[len(dists)-1]
		stack = stack[:len(stack)-1]
		dists = dists[:len(dists)-1]
		if _, ok := ancestors[oid]; ok {
			continue
		}
		obj := lookup(oid)
		if obj == nil || obj.Kind != KindCommit {
			continue
		}
		ancestors[oid] = d
		for _, p := range ParseCommit(obj.Data).Parents {
			stack = append(stack, p)
			dists = append(dists, d+1)
		}
	}
	best := ""
	bestDist := int(^uint(0) >> 1)
	seenB := make(map[string]bool)
	stack = []string{b}
	dists = []int{0}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		d := dists[len(dists)-1]
		stack = stack[:len(stack)-1]
		dists = dists[:len(dists)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ad, ok := ancestors[oid]; ok && ad+d < bestDist {
			bestDist = ad + d
			best = oid
		}
		obj := lookup(oid)
		if obj == nil || obj.Kind != KindCommit {
			continue
		}
		for _, p := range ParseCommit(obj.Data).Parents {
			stack = append(stack, p)
			dists = append(dists, d+1)
		}
	}
	return best
}

// IsAncestor reports whether ancestor is an ancestor of commit (or the same).
func IsAncestor(ancestor, commit string, lookup Lookup) bool {
	if ancestor == commit {
		return true
	}
	// Fast path (the push workload's shape: a linear child): the ancestor
	// is the commit's direct parent, so no visited set is ever allocated.
	head := lookup(commit)
	if head == nil || head.Kind != KindCommit {
		return false
	}
	parents := ParseCommit(head.Data).Parents
	for _, p := range parents {
		if p == ancestor {
			return true
		}
	}
	if len(parents) == 0 {
		return false
	}
	queue := append([]string(nil), parents...)
	seen := map[string]bool{commit: true}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Kind != KindCommit {
			continue
		}
		for _, p := range ParseCommit(obj.Data).Parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// GlobalID builds a base64 Relay-style global id: opaque, stable, never the
// plain number.
func GlobalID(kind string, parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strings.Join(parts, ":")))
}

// DecodeGlobalID splits a global id into its kind and parts.
func DecodeGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	text := string(raw)
	i := strings.IndexByte(text, ':')
	if i < 0 {
		return "", nil, false
	}
	return text[:i], strings.Split(text[i+1:], ":"), true
}

// ---------------------------------------------------------------------------
// the data model
// ---------------------------------------------------------------------------

// Repo is a repository: metadata, git objects and refs, issues and pulls.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int

	Objects map[string]*GitObject
	Refs    map[string]string
	Labels  map[string]*Label
	// LabelsOrder keeps label insertion order (Go maps iterate randomly;
	// the labels list must be deterministic across runs).
	LabelsOrder []string

	// Issues in creation order (lists) plus a number index (single GETs).
	Issues        []*Issue
	IssuesByNum   map[int]*Issue
	Pulls         []*Pull
	PullsByNum    map[int]*Pull
	NextNumber    int // one counter for issues and pulls (GitHub shares it)
	Version       int // bumped on every mutation of refs or objects
	RefVersion    int // bumped only when a ref is set or deleted
	RefNamesVer   int // bumped only when a ref NAME is added or removed
	RepoJSON      string
	RepoJSONBuf   []byte
	HasRepoJSON   bool
	HasRepoJSONB  bool
	FetchV2Cache  map[string][]byte
	FetchV0Cache  map[string][]byte
	LsRefsCache   map[string][]byte
	AdUpTemplate  *AdTemplate
	AdRecvTemplate *AdTemplate
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Issue is one issue.
type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedSeq int
	Comments   []*Comment
	CachedJSON string
	CachedBuf  []byte
	HasCache   bool
}

// Comment is one issue comment.
type Comment struct {
	ID         int
	Body       string
	CreatedSeq int
}

// Pull is one pull request.
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string // "" when not merged
	MergeCommitSha string // "" when not merged
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	CreatedSeq     int
}

// FileChange is one path's diff between two trees.
type FileChange struct {
	Path      string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
	OldOID    string // "" when added
	NewOID    string // "" when removed
	HasOld    bool
	HasNew    bool
}

// FlattenTree flattens a tree into path -> {oid, mode}, following directories.
func FlattenTree(repo *Repo, treeOID string) map[string]TreeEntry {
	out := make(map[string]TreeEntry)
	// Explicit stack, not recursion: a pushed tree can nest arbitrarily
	// deep, and a recursive walk would grow the goroutine stack with it.
	type frame struct {
		oid  string
		base string
	}
	stack := []frame{{oid: treeOID, base: ""}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := repo.Objects[f.oid]
		if obj == nil || obj.Kind != KindTree {
			continue
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := e.Name
			if f.base != "" {
				path = f.base + "/" + e.Name
			}
			if e.Mode == "40000" {
				stack = append(stack, frame{oid: e.OID, base: path})
			} else {
				out[path] = e
			}
		}
	}
	return out
}

// DiffTrees diffs two trees: one entry per differing path, in path order.
// baseTree "" means an empty tree.
func DiffTrees(repo *Repo, baseTree, headTree string) []FileChange {
	var base map[string]TreeEntry
	if baseTree == "" {
		base = map[string]TreeEntry{}
	} else {
		base = FlattenTree(repo, baseTree)
	}
	head := FlattenTree(repo, headTree)
	paths := make(map[string]bool, len(base)+len(head))
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var changes []FileChange
	for _, path := range sorted {
		from, inBase := base[path]
		to, inHead := head[path]
		switch {
		case inBase && inHead:
			if from.OID == to.OID {
				continue
			}
			add, del := LineDiff(repo, from.OID, to.OID)
			changes = append(changes, FileChange{Path: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del, OldOID: from.OID, NewOID: to.OID, HasOld: true, HasNew: true})
		case inHead:
			add, _ := LineDiff(repo, "", to.OID)
			changes = append(changes, FileChange{Path: path, Status: "added", Additions: add, Changes: add, NewOID: to.OID, HasNew: true})
		default:
			_, del := LineDiff(repo, from.OID, "")
			changes = append(changes, FileChange{Path: path, Status: "removed", Deletions: del, Changes: del, OldOID: from.OID, HasOld: true})
		}
	}
	return changes
}

// LineDiff counts added vs removed lines for a blob change (an LCS over the
// middle, after trimming the common prefix and suffix). "" means no blob.
func LineDiff(repo *Repo, oldOID, newOID string) (additions, deletions int) {
	oldText := ""
	if oldOID != "" {
		if obj := repo.Objects[oldOID]; obj != nil {
			oldText = string(obj.Data)
		}
	}
	newText := ""
	if newOID != "" {
		if obj := repo.Objects[newOID]; obj != nil {
			newText = string(obj.Data)
		}
	}
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	oldEnd, newEnd := len(oldLines), len(newLines)
	for oldEnd > start && newEnd > start && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}
	oldMid := oldLines[start:oldEnd]
	newMid := newLines[start:newEnd]
	if len(oldMid) == 0 {
		return len(newMid), 0
	}
	if len(newMid) == 0 {
		return 0, len(oldMid)
	}
	if len(oldMid)*len(newMid) > 250000 {
		// Large middle: multiset difference instead of an LCS.
		oldCount := make(map[string]int, len(oldMid))
		for _, l := range oldMid {
			oldCount[l]++
		}
		shared := 0
		for _, l := range newMid {
			if oldCount[l] > 0 {
				oldCount[l]--
				shared++
			}
		}
		return len(newMid) - shared, len(oldMid) - shared
	}
	// LCS dynamic program, two rows.
	prev := make([]int, len(newMid)+1)
	cur := make([]int, len(newMid)+1)
	for i := len(oldMid) - 1; i >= 0; i-- {
		for j := len(newMid) - 1; j >= 0; j-- {
			if oldMid[i] == newMid[j] {
				cur[j] = prev[j+1] + 1
			} else if prev[j] > cur[j+1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j+1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[0]
	return len(newMid) - lcs, len(oldMid) - lcs
}

// ---------------------------------------------------------------------------
// the forge
// ---------------------------------------------------------------------------

func validRepoName(name string) bool {
	if len(name) < 1 || len(name) > 100 {
		return false
	}
	c := name[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c = name[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Forge is the whole in-memory forge. mu guards every field; the dispatcher
// holds it for the whole dispatch, so no method here locks.
type Forge struct {
	mu             sync.Mutex
	Repos          map[string]*Repo
	repoGen        int // bumped on every repo create/delete (hot cache key)
	nextRepoID     int
	nextEntityID   int
	nextCreatedSeq int
}

// NewForge returns an empty forge.
func NewForge() *Forge {
	return &Forge{
		Repos:          make(map[string]*Repo),
		nextRepoID:     5000,
		nextEntityID:   1,
		nextCreatedSeq: 1,
	}
}

// Lock acquires the forge mutex (the dispatcher calls this per request).
func (f *Forge) Lock()   { f.mu.Lock() }

// Unlock releases the forge mutex.
func (f *Forge) Unlock() { f.mu.Unlock() }

// GetRepo returns a repository by name, or nil.
func (f *Forge) GetRepo(name string) *Repo {
	return f.Repos[name]
}

// CreateRepo creates a repository under the viewer; nil when the name is
// taken or invalid.
func (f *Forge) CreateRepo(name string, private bool, description, defaultBranch string) *Repo {
	if !validRepoName(name) || f.Repos[name] != nil {
		return nil
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	repo := &Repo{
		ID:            f.nextRepoID,
		Owner:         ViewerLogin,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranch,
		CreatedSeq:    f.nextCreatedSeq,
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		IssuesByNum:   make(map[int]*Issue),
		PullsByNum:    make(map[int]*Pull),
		NextNumber:    1,
		Version:       1,
		RefVersion:    1,
		RefNamesVer:   1,
		FetchV2Cache:  make(map[string][]byte),
		FetchV0Cache:  make(map[string][]byte),
		LsRefsCache:   make(map[string][]byte),
	}
	f.nextRepoID++
	f.nextCreatedSeq++
	f.Repos[name] = repo
	f.repoGen++
	return repo
}

// DeleteRepo deletes a repository; false when it did not exist.
func (f *Forge) DeleteRepo(name string) bool {
	if f.Repos[name] == nil {
		return false
	}
	delete(f.Repos, name)
	f.repoGen++
	return true
}

// ListViewerRepos returns the viewer's repositories in creation order.
func (f *Forge) ListViewerRepos() []*Repo {
	out := make([]*Repo, 0, len(f.Repos))
	for _, r := range f.Repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// PutObject stores an object, returning its id. Bumps the repo version when
// it adds one.
func (f *Forge) PutObject(repo *Repo, kind ObjKind, data []byte) string {
	oid := HashObject(kind, data)
	if repo.Objects[oid] == nil {
		repo.Objects[oid] = &GitObject{Kind: kind, Data: data}
		repo.Version++
	}
	return oid
}

// PutPackObjects stores a pack's objects (the receive-pack path). Bumps the
// version once. Returns how many were new.
func (f *Forge) PutPackObjects(repo *Repo, objects map[string]GitObject) int {
	added := 0
	for oid, obj := range objects {
		if repo.Objects[oid] == nil {
			o := obj
			repo.Objects[oid] = &o
			added++
		}
	}
	if added > 0 {
		repo.Version++
	}
	return added
}

// SetRef sets a ref, bumping the generations when it actually changes. A new
// ref NAME also bumps RefNamesVer (advertisement templates key on it).
func (f *Forge) SetRef(repo *Repo, ref, oid string) {
	prev, exists := repo.Refs[ref]
	if exists && prev == oid {
		return
	}
	if !exists {
		repo.RefNamesVer++
	}
	repo.Refs[ref] = oid
	repo.Version++
	repo.RefVersion++
}

// DeleteRef deletes a ref (bumping the generations). False when absent.
func (f *Forge) DeleteRef(repo *Repo, ref string) bool {
	if _, ok := repo.Refs[ref]; !ok {
		return false
	}
	delete(repo.Refs, ref)
	repo.Version++
	repo.RefVersion++
	repo.RefNamesVer++
	return true
}

// MakeCommit creates a commit object on a repo and returns its id.
func (f *Forge) MakeCommit(repo *Repo, tree string, parents []string, message string) string {
	return f.PutObject(repo, KindCommit, BuildCommitBytes(tree, parents, message, TrialEpochSec))
}

// RefTip returns the tip of a ref, or "" when the ref does not exist.
func (f *Forge) RefTip(repo *Repo, ref string) string {
	return repo.Refs[ref]
}

// BranchTip returns the tip of a branch, or "".
func (f *Forge) BranchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

// Branch is one branch as {name, sha}.
type Branch struct {
	Name string
	Sha  string
}

// Branches returns all branches, default branch first, then name order.
func (f *Forge) Branches(repo *Repo) []Branch {
	var out []Branch
	for ref, sha := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, Branch{Name: ref[len("refs/heads/"):], Sha: sha})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == repo.DefaultBranch {
			return true
		}
		if out[j].Name == repo.DefaultBranch {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CommitsFrom returns the commits reachable from a tip, newest first
// (depth-first, parents in order).
func (f *Forge) CommitsFrom(repo *Repo, tip string) []string {
	var out []string
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Kind != KindCommit {
			continue
		}
		out = append(out, oid)
		parents := ParseCommit(obj.Data).Parents
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return out
}

// SetPath sets a path's content in a tree, creating intermediate directories;
// returns the new root tree id. blobOID "" deletes the path. rootTree "" is
// an empty tree.
func (f *Forge) SetPath(repo *Repo, rootTree, path, blobOID string) string {
	segments := splitPath(path)
	var build func(treeOID string, depth int) string
	build = func(treeOID string, depth int) string {
		entries := make(map[string]TreeEntry)
		if treeOID != "" {
			if obj := repo.Objects[treeOID]; obj != nil && obj.Kind == KindTree {
				if parsed, err := ParseTree(obj.Data); err == nil {
					for _, e := range parsed {
						entries[e.Name] = e
					}
				}
			}
		}
		name := segments[depth]
		if depth == len(segments)-1 {
			if blobOID == "" {
				delete(entries, name)
			} else {
				entries[name] = TreeEntry{Mode: "100644", Name: name, OID: blobOID}
			}
		} else {
			childTree := ""
			if child, ok := entries[name]; ok && child.Mode == "40000" {
				childTree = child.OID
			}
			next := build(childTree, depth+1)
			if obj := repo.Objects[next]; obj != nil && obj.Kind == KindTree {
				if parsed, err := ParseTree(obj.Data); err == nil && len(parsed) == 0 {
					delete(entries, name)
				} else {
					entries[name] = TreeEntry{Mode: "40000", Name: name, OID: next}
				}
			} else {
				entries[name] = TreeEntry{Mode: "40000", Name: name, OID: next}
			}
		}
		list := make([]TreeEntry, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		return f.PutObject(repo, KindTree, BuildTree(list))
	}
	return build(rootTree, 0)
}

func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ReadPathResult is what ReadPath finds: a file, a directory, or nothing.
type ReadPathResult struct {
	IsDir   bool
	OID     string
	Mode    string
	Entries []TreeEntry
	Found   bool
}

// ReadPath reads a path from a tree: the blob oid and mode, or a directory's
// entries, or not found.
func (f *Forge) ReadPath(repo *Repo, rootTree, path string) ReadPathResult {
	segments := splitPath(path)
	current := rootTree
	for i, seg := range segments {
		obj := repo.Objects[current]
		if obj == nil || obj.Kind != KindTree {
			return ReadPathResult{}
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return ReadPathResult{}
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return ReadPathResult{}
		}
		if i == len(segments)-1 {
			if found.Mode == "40000" {
				dir := repo.Objects[found.OID]
				if dir == nil || dir.Kind != KindTree {
					return ReadPathResult{}
				}
				dirEntries, err := ParseTree(dir.Data)
				if err != nil {
					return ReadPathResult{}
				}
				return ReadPathResult{IsDir: true, Entries: dirEntries, Found: true}
			}
			return ReadPathResult{OID: found.OID, Mode: found.Mode, Found: true}
		}
		if found.Mode != "40000" {
			return ReadPathResult{}
		}
		current = found.OID
	}
	root := repo.Objects[rootTree]
	if root == nil || root.Kind != KindTree {
		return ReadPathResult{}
	}
	entries, err := ParseTree(root.Data)
	if err != nil {
		return ReadPathResult{}
	}
	return ReadPathResult{IsDir: true, Entries: entries, Found: true}
}

// CreateLabel creates a label (or returns the existing one).
func (f *Forge) CreateLabel(repo *Repo, name, color string) *Label {
	if existing := repo.Labels[name]; existing != nil {
		return existing
	}
	label := &Label{Name: name, Color: color}
	repo.Labels[name] = label
	repo.LabelsOrder = append(repo.LabelsOrder, name)
	return label
}

// CreateIssue creates an issue; err is non-empty when a label is unknown.
func (f *Forge) CreateIssue(repo *Repo, title, body string, labels []string) (*Issue, string) {
	for _, l := range labels {
		if repo.Labels[l] == nil {
			return nil, "Label '" + l + "' does not exist"
		}
	}
	issue := &Issue{
		ID:         f.nextEntityID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string(nil), labels...),
		CreatedSeq: f.nextCreatedSeq,
	}
	f.nextEntityID++
	f.nextCreatedSeq++
	repo.NextNumber++
	repo.Issues = append(repo.Issues, issue)
	repo.IssuesByNum[issue.Number] = issue
	return issue, ""
}

// GetIssue returns an issue by number, or nil.
func (f *Forge) GetIssue(repo *Repo, number int) *Issue {
	return repo.IssuesByNum[number]
}

// CreateComment appends a comment to an issue.
func (f *Forge) CreateComment(repo *Repo, issue *Issue, body string) *Comment {
	comment := &Comment{ID: f.nextEntityID, Body: body, CreatedSeq: f.nextCreatedSeq}
	f.nextEntityID++
	f.nextCreatedSeq++
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// CreatePull creates a pull request; err is non-empty on failure.
func (f *Forge) CreatePull(repo *Repo, title, body, head, base string) (*Pull, string) {
	headSha := f.BranchTip(repo, head)
	baseSha := f.BranchTip(repo, base)
	if headSha == "" {
		return nil, "Head branch '" + head + "' does not exist"
	}
	if baseSha == "" {
		return nil, "Base branch '" + base + "' does not exist"
	}
	for _, p := range repo.Pulls {
		if p.HeadRef == head && p.BaseRef == base && p.State == "open" {
			return nil, "A pull request already exists for this head branch"
		}
	}
	pull := &Pull{
		ID:         f.nextEntityID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    head,
		BaseRef:    base,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		CreatedSeq: f.nextCreatedSeq,
	}
	f.nextEntityID++
	f.nextCreatedSeq++
	repo.NextNumber++
	repo.Pulls = append(repo.Pulls, pull)
	repo.PullsByNum[pull.Number] = pull
	return pull, ""
}

// GetPull returns a pull request by number, or nil.
func (f *Forge) GetPull(repo *Repo, number int) *Pull {
	return repo.PullsByNum[number]
}

func (f *Forge) lookup(repo *Repo) Lookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

// PullFiles returns the file changes of a pull request (merge-base tree vs
// head tip tree).
func (f *Forge) PullFiles(repo *Repo, pull *Pull) []FileChange {
	baseTip := f.BranchTip(repo, pull.BaseRef)
	if baseTip == "" {
		baseTip = pull.BaseSha
	}
	base := MergeBase(pull.HeadSha, baseTip, f.lookup(repo))
	baseTree := ""
	if base != "" {
		baseTree = ParseCommit(repo.Objects[base].Data).Tree
	}
	headTree := ParseCommit(repo.Objects[pull.HeadSha].Data).Tree
	return DiffTrees(repo, baseTree, headTree)
}

// PullConflicts reports whether a squash merge of this pull would conflict.
func (f *Forge) PullConflicts(repo *Repo, pull *Pull) bool {
	// The base branch may have moved since the pull was created (another
	// pull merged onto it): the conflict is against the base branch's
	// current tip, not the tip recorded at creation.
	baseTip := f.BranchTip(repo, pull.BaseRef)
	if baseTip == "" {
		return false
	}
	base := MergeBase(pull.HeadSha, baseTip, f.lookup(repo))
	if base == "" {
		return false
	}
	baseTree := ParseCommit(repo.Objects[base].Data).Tree
	headTree := ParseCommit(repo.Objects[pull.HeadSha].Data).Tree
	baseTipTree := ParseCommit(repo.Objects[baseTip].Data).Tree
	headChanges := make(map[string]FileChange)
	for _, c := range DiffTrees(repo, baseTree, headTree) {
		headChanges[c.Path] = c
	}
	baseChanges := make(map[string]FileChange)
	for _, c := range DiffTrees(repo, baseTree, baseTipTree) {
		baseChanges[c.Path] = c
	}
	for path, hc := range headChanges {
		bc, ok := baseChanges[path]
		if !ok {
			continue
		}
		if hc.NewOID != bc.NewOID {
			return true
		}
	}
	return false
}

// MergePull squash-merges a pull onto its base branch: one merge commit
// whose tree is the base tip tree with the head's changes applied. Returns
// the merge commit id, or "" when it conflicts or the pull is closed.
func (f *Forge) MergePull(repo *Repo, pull *Pull) string {
	if pull.State != "open" || pull.Merged {
		return ""
	}
	if f.PullConflicts(repo, pull) {
		return ""
	}
	baseTip := f.BranchTip(repo, pull.BaseRef)
	if baseTip == "" {
		return ""
	}
	base := MergeBase(pull.HeadSha, baseTip, f.lookup(repo))
	baseTree := ""
	if base != "" {
		baseTree = ParseCommit(repo.Objects[base].Data).Tree
	}
	headTree := ParseCommit(repo.Objects[pull.HeadSha].Data).Tree
	changes := DiffTrees(repo, baseTree, headTree)
	tree := ParseCommit(repo.Objects[baseTip].Data).Tree
	for _, c := range changes {
		tree = f.SetPath(repo, tree, c.Path, c.NewOID)
	}
	message := "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + pull.HeadRef
	mergeSha := f.MakeCommit(repo, tree, []string{baseTip}, message)
	f.SetRef(repo, "refs/heads/"+pull.BaseRef, mergeSha)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = TrialISO
	pull.MergeCommitSha = mergeSha
	pull.BaseSha = mergeSha
	repo.HasRepoJSON = false
	repo.HasRepoJSONB = false
	return mergeSha
}
