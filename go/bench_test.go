// In-process benchmarks for the v5 climb (NOT part of the entry binary:
// _test.go files are excluded from `go build`). These measure dispatch CPU
// per operation with nanosecond precision — the load-insensitive signal the
// hill-climb verdicts rest on.
//
//   go test -bench=. -benchtime=2000x -run=^$ .
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"runtime/pprof"
	"strings"
	"testing"
)

// oidOf returns the git object id of a typed object, using the same
// construction as the kit's hashObject (header + NUL + data, SHA-1).
func oidOf(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func buildTreeEntry(mode, name, oid string) []byte {
	raw, _ := hex.DecodeString(oid)
	out := []byte(mode + " " + name + "\x00")
	return append(out, raw...)
}

func buildCommitBytes(tree string, parents []string, msg string, ts int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author adaiversen <adaiversen@example.invalid> %d +0000\n", ts)
	fmt.Fprintf(&b, "committer adaiversen <adaiversen@example.invalid> %d +0000\n", ts)
	b.WriteString("\n")
	b.WriteString(msg)
	return []byte(b.String())
}

const zeroOID = "0000000000000000000000000000000000000000"

// pushPayloads builds n sequential 1 KiB push bodies exactly like the kit's
// bench.ts buildPushPayloads (blob + tree + commit per push, chained).
func pushPayloads(n int) ([][]byte, []string) {
	payloads := make([][]byte, n)
	tips := make([]string, n)
	parent := ""
	for i := 0; i < n; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("bench-data %d ", i), 80)[:1024])
		blobOid := oidOf("blob", blob)
		tree := buildTreeEntry("100644", "bench-data.txt", blobOid)
		treeOid := oidOf("tree", tree)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		commit := buildCommitBytes(treeOid, parents, fmt.Sprintf("bench: push %d\n", i+1), 1700000000+i)
		commitOid := oidOf("commit", commit)
		pack := WritePack([]PackInput{
			{Kind: KindBlob, Data: blob},
			{Kind: KindTree, Data: tree},
			{Kind: KindCommit, Data: commit},
		})
		old := parent
		if old == "" {
			old = zeroOID
		}
		cmd := fmt.Sprintf("%s %s refs/heads/main\x00report-status side-band-64k ofs-delta agent=forge/1.0\n", old, commitOid)
		var body bytes.Buffer
		fmt.Fprintf(&body, "%04x%s", len(cmd)+4, cmd)
		body.WriteString("0000")
		body.Write(pack)
		payloads[i] = body.Bytes()
		tips[i] = commitOid
		parent = commitOid
	}
	return payloads, tips
}

// BenchmarkPushDispatch measures the v0 advertisement + receive-pack handler
// per push (the server-side cost of the push workload, transport excluded).
func BenchmarkPushDispatch(b *testing.B) {
	// Two independent chains, each starting from the zero OID: one for
	// warmup on a throwaway repo, one for the timed repo.
	warmPayloads, _ := pushPayloads(200)
	payloads, tips := pushPayloads(b.N)
	forge := NewForge()
	repo := forge.CreateRepo("push-bench", false, "", "main")
	warm := NewForge()
	wrepo := warm.CreateRepo("warm", false, "", "main")
	for i := 0; i < 200; i++ {
		V0Advertisement(wrepo, "git-receive-pack")
		HandleReceivePack(warm, wrepo, warmPayloads[i])
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		V0Advertisement(repo, "git-receive-pack")
		if status, _ := HandleReceivePack(forge, repo, payloads[i]); status != 200 {
			b.Fatalf("push %d: status %d", i, status)
		}
	}
	if repo.Refs["refs/heads/main"] != tips[b.N-1] {
		b.Fatalf("tip mismatch")
	}
}

// BenchmarkReadDispatch measures a pooled GET-issue dispatch (the read
// workload's server-side cost, transport excluded).
func BenchmarkReadDispatch(b *testing.B) {
	forge := NewForge()
	repo := forge.CreateRepo("read-bench", false, "", "main")
	iss, _ := forge.CreateIssue(repo, "bench issue", "body text here", nil)
	d := NewDispatcher(forge)
	d.expectedAuth = []byte("Bearer local-practice-token")
	req := &Request{
		Method: []byte("GET"),
		Target: []byte("/api/v3/repos/adaiversen/read-bench/issues/" + fmt.Sprint(iss.Number)),
		Auth:   []byte("Bearer local-practice-token"),
	}
	// Prime the pool and verify the reply is the issue, not a fast error.
	// The pooled path returns Prebuilt bytes (Status unset by design).
	prime := d.Dispatch(req)
	primeBody := prime.Body
	if prime.Prebuilt != nil {
		primeBody = prime.Prebuilt
	}
	if !bytes.Contains(primeBody, []byte("bench issue")) {
		b.Fatalf("read dispatch prime: status %d body %.80s", prime.Status, primeBody)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Dispatch(req)
	}
}

// BenchmarkWriteDispatch measures a POST-issue dispatch (the write workload's
// server-side cost, transport excluded).
func BenchmarkWriteDispatch(b *testing.B) {
	forge := NewForge()
	forge.CreateRepo("write-bench", false, "", "main")
	d := NewDispatcher(forge)
	d.expectedAuth = []byte("Bearer local-practice-token")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := &Request{
			Method: []byte("POST"),
			Target: []byte("/api/v3/repos/adaiversen/write-bench/issues"),
			Auth:   []byte("Bearer local-practice-token"),
			Body:   []byte(fmt.Sprintf(`{"title":"issue %d","body":""}`, i)),
		}
		d.Dispatch(req)
	}
}

// cloneFixture builds a 200-commit repo (one big pack, like the bench setup)
// and returns the repo plus the v2 fetch body a depth-1 clone sends.
func cloneFixture(b testing.TB) (*Forge, *Repo, []byte, []byte) {
	forge := NewForge()
	repo := forge.CreateRepo("bench-clone", false, "", "main")
	var objs []PackInput
	parent := ""
	var tip string
	for i := 0; i < 200; i++ {
		blob := []byte(strings.Repeat(fmt.Sprintf("clone-data %d ", i), 8)[:64])
		blobOid := oidOf("blob", blob)
		tree := buildTreeEntry("100644", "clone-data.txt", blobOid)
		treeOid := oidOf("tree", tree)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		commit := buildCommitBytes(treeOid, parents, fmt.Sprintf("clone: commit %d\n", i+1), 1700000000+i)
		commitOid := oidOf("commit", commit)
		objs = append(objs,
			PackInput{Kind: KindBlob, Data: blob},
			PackInput{Kind: KindTree, Data: tree},
			PackInput{Kind: KindCommit, Data: commit})
		parent = commitOid
		tip = commitOid
	}
	pack := WritePack(objs)
	cmd := fmt.Sprintf("%s %s refs/heads/main\x00report-status side-band-64k ofs-delta agent=forge/1.0\n", zeroOID, tip)
	var body bytes.Buffer
	fmt.Fprintf(&body, "%04x%s", len(cmd)+4, cmd)
	body.WriteString("0000")
	body.Write(pack)
	if status, _ := HandleReceivePack(forge, repo, body.Bytes()); status != 200 {
		b.Fatalf("setup push: status %d", status)
	}
	// The v2 fetch body a depth-1 clone sends (command + want + deepen + done).
	fetchBody := pktLineInto(nil, []byte("command=fetch\n"))
	fetchBody = pktLineInto(fetchBody, []byte("object-format=sha1\n"))
	fetchBody = pktLineInto(fetchBody, []byte("want "+tip+"\n"))
	fetchBody = pktLineInto(fetchBody, []byte("deepen 1\n"))
	fetchBody = pktLineInto(fetchBody, []byte("filter blob:none\n"))
	fetchBody = pktLineInto(fetchBody, []byte("done\n"))
	lsRefsBody := pktLineInto(nil, []byte("command=ls-refs\n"))
	lsRefsBody = pktLineInto(lsRefsBody, []byte("peel\n"))
	lsRefsBody = pktLineInto(lsRefsBody, []byte("unborn\n"))
	lsRefsBody = pktLineInto(lsRefsBody, []byte("symrefs\n"))
	lsRefsBody = pktLineInto(lsRefsBody, []byte("agent=forge/1.0\n"))
	lsRefsBody = append(lsRefsBody, []byte("0000")...)
	return forge, repo, fetchBody, lsRefsBody
}

// BenchmarkCloneFetchCached measures the cached v2 fetch + ls-refs path —
// what 49 of the 50 concurrent clones pay (the first populates the cache).
func BenchmarkCloneFetchCached(b *testing.B) {
	_, repo, fetchBody, lsRefsBody := cloneFixture(b)
	HandleLsRefs(repo, lsRefsBody)
	HandleFetchV2(repo, fetchBody)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HandleLsRefs(repo, lsRefsBody)
		HandleFetchV2(repo, fetchBody)
	}
}

// BenchmarkCloneFetchCold measures the first (uncached) v2 fetch — the pack
// build + sideband cost for a depth-1 clone of a 200-commit repo.
func BenchmarkCloneFetchCold(b *testing.B) {
	_, repo, fetchBody, _ := cloneFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		repo.FetchV2Cache = map[string][]byte{}
		HandleFetchV2(repo, fetchBody)
	}
}

// TestColdFetchObjectCount is a diagnostic: a depth-1 fetch of a 200-commit
// repo must pack exactly 3 objects (tip commit + its tree + its blob).
// More means the depth limit is not cutting the walk off.
func TestColdFetchObjectCount(t *testing.T) {
	_, repo, fetchBody, _ := cloneFixture(t)
	out := HandleFetchV2(repo, fetchBody)
	// The sideband payload is the pack: strip pkt-line framing (band 1).
	hexVal := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		default:
			return int(c-'A') + 10
		}
	}
	var pack []byte
	rest := out
	for len(rest) >= 4 {
		n := hexVal(rest[0])<<12 | hexVal(rest[1])<<8 | hexVal(rest[2])<<4 | hexVal(rest[3])
		if n == 0 {
			break // flush
		}
		if n == 1 {
			rest = rest[4:] // delim
			continue
		}
		if n < 5 || len(rest) < n {
			break
		}
		if rest[4] == 1 {
			pack = append(pack, rest[5:n]...)
		}
		rest = rest[n:]
	}
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		t.Fatalf("no pack in fetch reply (%d bytes)", len(pack))
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	t.Logf("cold depth-1 fetch packed %d objects (%d pack bytes)", count, len(pack))
	if count != 3 {
		t.Fatalf("expected 3 objects, got %d", count)
	}
}

// TestProfilePush profiles ONLY the timed push loop (payload building happens
// before profiling starts). Run: go test -run TestProfilePush -v .
// Writes /tmp/push-timed.prof.
func TestProfilePush(t *testing.T) {
	payloads, _ := pushPayloads(2000)
	forge := NewForge()
	repo := forge.CreateRepo("pp", false, "", "main")
	f, err := os.Create("/tmp/push-timed.prof")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pprof.StartCPUProfile(f)
	for i := 0; i < 2000; i++ {
		V0Advertisement(repo, "git-receive-pack")
		if status, _ := HandleReceivePack(forge, repo, payloads[i]); status != 200 {
			t.Fatalf("push %d: status %d", i, status)
		}
	}
	pprof.StopCPUProfile()
}

// TestProfileColdFetch profiles ONLY the timed cold-fetch loop. Writes
// /tmp/cold-timed.prof.
func TestProfileColdFetch(t *testing.T) {
	_, repo, fetchBody, _ := cloneFixture(t)
	f, err := os.Create("/tmp/cold-timed.prof")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pprof.StartCPUProfile(f)
	for i := 0; i < 2000; i++ {
		repo.FetchV2Cache = map[string][]byte{}
		HandleFetchV2(repo, fetchBody)
	}
	pprof.StopCPUProfile()
}

// TestPackRoundTrip verifies both pack writers round-trip through ReadPack
// with byte-identical objects: the deflated WritePack and the stored-block
// writePackKnown, including an empty blob and a >64KB blob (which spans
// several stored blocks).
func TestPackRoundTrip(t *testing.T) {
	big := []byte(strings.Repeat("abcdefgh", 10000)) // 80,000 bytes
	inputs := []PackInput{
		{Kind: KindBlob, Data: []byte("hello round-trip\n")},
		{Kind: KindBlob, Data: []byte(strings.Repeat("compressible ", 200))},
		{Kind: KindBlob, Data: []byte{}},
		{Kind: KindBlob, Data: big},
	}
	byOID := make(map[string]PackInput, len(inputs))
	for _, in := range inputs {
		byOID[HashObject(in.Kind, in.Data)] = in
	}
	for name, pack := range map[string][]byte{
		"deflated": WritePack(inputs),
		"stored":   writePackKnown(byOID),
	} {
		got, err := ReadPack(pack)
		if err != nil {
			t.Fatalf("%s: ReadPack: %v", name, err)
		}
		if len(got) != len(inputs) {
			t.Fatalf("%s: got %d objects, want %d", name, len(got), len(inputs))
		}
		for oid, want := range byOID {
			g, ok := got[oid]
			if !ok {
				t.Fatalf("%s: missing object %s", name, oid)
			}
			if g.Kind != want.Kind || !bytes.Equal(g.Data, want.Data) {
				t.Fatalf("%s: object %s differs", name, oid)
			}
		}
	}
}
