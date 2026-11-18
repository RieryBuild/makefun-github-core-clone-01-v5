// The git smart-HTTP surface: protocol v2 upload-pack (advertisement,
// ls-refs, fetch) and v0 receive-pack (advertisement, push), over the
// forge's in-memory objects and refs.
//
// Wire notes the suite's client pins down:
//   - v2 advertisement: `# service=...` preamble, flush, `version 2`,
//     capability lines, flush. Capabilities the client needs:
//     ls-refs=unborn, fetch=shallow, agent, object-format=sha1.
//   - v0 advertisement: `# service=...` preamble, flush, `<oid> <ref>\0<caps>`
//     first line, the ref lines, peeled `^{}` lines for annotated tags,
//     flush. Empty repo: the zero id with the `capabilities^{}` pseudo-ref.
//     Receive-pack must advertise report-status, side-band-64k and
//     delete-refs; upload-pack must advertise side-band-64k.
//   - v2 fetch answers sections: acknowledgments (NAK), shallow-info when a
//     depth is asked for, then packfile with sideband band-1 packets. v0
//     fetch answers NAK, shallow lines, then the sideband pack. A fetch
//     with nothing new still answers a packfile section carrying an empty
//     (0-entry) pack.
//   - receive-pack answers the report-status stream inside sideband band 1:
//     `unpack ok`, then one `ok <ref>` / `ng <ref> <reason>` line per
//     command, in command order. A non-fast-forward refusal is exactly
//     `ng <ref> non-fast-forward` and the ref does not move; the pack's
//     objects are stored even when a ref is refused.
//
// Performance notes:
//   - The v0 advertisement is built from a per-repo TEMPLATE: its shape
//     depends only on the ref NAMES (which change rarely), so the template
//     is cached on RefNamesVer and each request only copies it and splices
//     the current tip oids into their slots. A push moves a tip every time
//     but never changes the ref set, so the advertisement stays a ~300-byte
//     copy plus two 40-byte writes. (When a ref points at an annotated tag
//     the peeled line's existence depends on the tip, so that case falls
//     back to a fresh build.)
//   - Fetch packs and ls-refs answers are cached on (repo version, raw
//     request body): the clone workload's 50 identical clones share one
//     pack build.
//   - The receive-pack path parses the command pkt-lines directly out of
//     the body buffer instead of going through the generic decoder: no
//     regex, no intermediate string array for the pack bytes.

package main

import (
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

// pktLenAt reads a 4-hex pkt-line length at offset; -1 when not hex.
func pktLenAt(body []byte, off int) int {
	if off+4 > len(body) {
		return -1
	}
	a, b, c, d := hexVal(body[off]), hexVal(body[off+1]), hexVal(body[off+2]), hexVal(body[off+3])
	if a < 0 || b < 0 || c < 0 || d < 0 {
		return -1
	}
	return a<<12 | b<<8 | c<<4 | d
}

func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = appendPktLen(out, len(payload)+4)
	return append(out, payload...)
}

func appendPktLen(out []byte, n int) []byte {
	const hexd = "0123456789abcdef"
	return append(out, hexd[n>>12&0xf], hexd[n>>8&0xf], hexd[n>>4&0xf], hexd[n&0xf])
}

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

// pktLines joins pkt-line payloads; a nil payload means a flush, and the
// single byte 0x01 as a payload means a delim packet. Callers build the
// common cases with the helpers below instead.
func pktLines(items [][]byte) []byte {
	var out []byte
	for _, item := range items {
		if item == nil {
			out = append(out, flushPkt...)
		} else {
			out = pktLineInto(out, item)
		}
	}
	return out
}

func pktLineInto(out, payload []byte) []byte {
	out = appendPktLen(out, len(payload)+4)
	return append(out, payload...)
}

const (
	lineFlush = "\x00flush"
	lineDelim = "\x00delim"
)

// decodePktLines decodes a whole body into pkt-line payloads (data packets
// only, in order) plus the undecoded remainder (the pack bytes after a
// receive-pack command list).
func decodePktLines(body []byte) (lines []string, rest []byte) {
	off := 0
	for off+4 <= len(body) {
		length := pktLenAt(body, off)
		if length == -1 {
			break
		}
		if length == 0 {
			off += 4
			lines = append(lines, lineFlush)
			continue
		}
		if length == 1 {
			off += 4
			lines = append(lines, lineDelim)
			continue
		}
		if length < 4 || off+length > len(body) {
			break
		}
		lines = append(lines, string(body[off+4:off+length]))
		off += length
	}
	return lines, body[off:]
}

// ---------------------------------------------------------------------------
// refs
// ---------------------------------------------------------------------------

type refLine struct {
	oid         string
	name        string
	peeled      string
	hasPeeled   bool
	symrefTarget string
	unborn      bool
}

// refLines returns every ref line of a repo, HEAD first, then refs in name
// order, with peeled tags.
func refLines(repo *Repo) []refLine {
	var lines []refLine
	defaultRef := "refs/heads/" + repo.DefaultBranch
	defaultTip, hasDefault := repo.Refs[defaultRef]
	if hasDefault {
		lines = append(lines, refLine{oid: defaultTip, name: "HEAD", symrefTarget: defaultRef})
	} else {
		lines = append(lines, refLine{oid: ZeroOID, name: "HEAD", symrefTarget: defaultRef, unborn: true})
	}
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs[name]
		line := refLine{oid: oid, name: name}
		if obj := repo.Objects[oid]; obj != nil && obj.Kind == KindTag {
			if peeled := peelTag(repo, oid); peeled != "" {
				line.peeled, line.hasPeeled = peeled, true
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// peelTag follows a tag chain to its commit, or "" when it ends elsewhere.
func peelTag(repo *Repo, oid string) string {
	current := oid
	for hops := 0; hops < 10; hops++ {
		obj := repo.Objects[current]
		if obj == nil {
			return ""
		}
		if obj.Kind == KindCommit {
			return current
		}
		if obj.Kind != KindTag {
			return ""
		}
		current = ParseTag(obj.Data).Object
	}
	return ""
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

// v2AdvertisementBytes is the v2 capability advertisement (info/refs with
// Git-Protocol: version=2). It never varies, so it is built once and shared.
var v2AdvertisementBytes = pktLines([][]byte{
	[]byte("# service=git-upload-pack\n"),
	nil,
	[]byte("version 2\n"),
	[]byte("ls-refs=unborn\n"),
	[]byte("fetch=shallow\n"),
	[]byte("agent=forge/1.0\n"),
	[]byte("object-format=sha1\n"),
	nil,
})

const uploadV0Caps = "side-band-64k thin-pack ofs-delta shallow multi_ack agent=forge/1.0"
const receiveV0Caps = "report-status report-status-v2 side-band-64k ofs-delta delete-refs agent=forge/1.0"

// AdTemplate is a v0 advertisement with ZeroOID in every oid slot, plus the
// slot table to splice live tips into. The shape depends only on the ref
// names, so it is keyed on RefNamesVer.
type AdTemplate struct {
	namesVersion int
	buf          []byte
	slots        []adSlot
	fixed        []byte // non-nil when the template IS the answer (no slots)
}

type adSlot struct {
	offset int
	ref    string // "" means HEAD (the default branch tip)
	peeled bool
}

// buildAdTemplate builds a v0 advertisement template for one service.
// Returns nil when a ref tip is an annotated tag (the peeled line's
// existence would depend on the tip) — the caller builds fresh then.
func buildAdTemplate(repo *Repo, service string) *AdTemplate {
	caps := uploadV0Caps
	if service == "git-receive-pack" {
		caps = receiveV0Caps
	}
	symref := "symref=HEAD:refs/heads/" + repo.DefaultBranch
	all := refLines(repo)
	var lines []refLine
	for _, l := range all {
		if l.name != "HEAD" {
			lines = append(lines, l)
		}
	}
	for _, l := range lines {
		if obj := repo.Objects[l.oid]; obj != nil && obj.Kind == KindTag {
			return nil
		}
	}
	var buf []byte
	var slots []adSlot
	writePkt := func(text string, slotRef *string, peeled bool) {
		buf = appendPktLen(buf, len(text)+4)
		if slotRef != nil {
			slots = append(slots, adSlot{offset: len(buf), ref: *slotRef, peeled: peeled})
		}
		buf = append(buf, text...)
	}
	writeFlush := func() { buf = append(buf, flushPkt...) }
	headRef := ""
	writePkt("# service="+service+"\n", nil, false)
	writeFlush()
	if len(lines) == 0 {
		writePkt(ZeroOID+" capabilities^{}\x00"+caps+" "+symref+"\n", nil, false)
	} else {
		writePkt(ZeroOID+" HEAD\x00"+caps+" "+symref+"\n", &headRef, false)
		for _, l := range lines {
			name := l.name
			writePkt(ZeroOID+" "+l.name+"\n", &name, false)
			if l.hasPeeled {
				writePkt(ZeroOID+" "+l.name+"^{}\n", &name, true)
			}
		}
	}
	writeFlush()
	t := &AdTemplate{namesVersion: repo.RefNamesVer, buf: buf, slots: slots}
	if len(slots) == 0 {
		t.fixed = buf
	}
	return t
}

// V0Advertisement returns a v0 ref advertisement for one service, from the
// cached template.
func V0Advertisement(repo *Repo, service string) []byte {
	var template **AdTemplate
	if service == "git-upload-pack" {
		template = &repo.AdUpTemplate
	} else {
		template = &repo.AdRecvTemplate
	}
	t := *template
	if t == nil || t.namesVersion != repo.RefNamesVer {
		t = buildAdTemplate(repo, service)
		*template = t
		if t == nil {
			return buildAdFresh(repo, service)
		}
	}
	if t.fixed != nil {
		return t.fixed
	}
	out := make([]byte, len(t.buf))
	copy(out, t.buf)
	for _, slot := range t.slots {
		var oid string
		if slot.ref == "" {
			oid = repo.Refs["refs/heads/"+repo.DefaultBranch]
			if oid == "" {
				oid = ZeroOID
			}
		} else {
			tip := repo.Refs[slot.ref]
			if tip == "" {
				tip = ZeroOID
			}
			if slot.peeled {
				if peeled := peelTag(repo, tip); peeled != "" {
					oid = peeled
				} else {
					oid = tip
				}
			} else {
				oid = tip
			}
		}
		copy(out[slot.offset:slot.offset+40], oid)
	}
	return out
}

// buildAdFresh builds a v0 advertisement from scratch (tag tips only).
func buildAdFresh(repo *Repo, service string) []byte {
	caps := uploadV0Caps
	if service == "git-receive-pack" {
		caps = receiveV0Caps
	}
	symref := "symref=HEAD:refs/heads/" + repo.DefaultBranch
	all := refLines(repo)
	var lines []refLine
	var head refLine
	for _, l := range all {
		if l.name == "HEAD" {
			head = l
		} else {
			lines = append(lines, l)
		}
	}
	var items [][]byte
	items = append(items, []byte("# service="+service+"\n"), nil)
	if len(lines) == 0 {
		items = append(items, []byte(ZeroOID+" capabilities^{}\x00"+caps+" "+symref+"\n"))
	} else {
		headOID := head.oid
		if head.unborn {
			headOID = ZeroOID
		}
		items = append(items, []byte(headOID+" HEAD\x00"+caps+" "+symref+"\n"))
		for _, l := range lines {
			items = append(items, []byte(l.oid+" "+l.name+"\n"))
			if l.hasPeeled {
				items = append(items, []byte(l.peeled+" "+l.name+"^{}\n"))
			}
		}
	}
	items = append(items, nil)
	return pktLines(items)
}

// ---------------------------------------------------------------------------
// v2 ls-refs
// ---------------------------------------------------------------------------

const maxCacheEntries = 128

func cacheSet(m map[string][]byte, key string, value []byte) {
	if len(m) >= maxCacheEntries {
		for k := range m {
			delete(m, k)
		}
	}
	m[key] = value
}

func cacheKey(repo *Repo, body []byte) string {
	return strconv.Itoa(repo.Version) + "|" + string(body)
}

// HandleLsRefs answers a v2 ls-refs command.
func HandleLsRefs(repo *Repo, body []byte) []byte {
	key := cacheKey(repo, body)
	if cached, ok := repo.LsRefsCache[key]; ok {
		return cached
	}
	lines, _ := decodePktLines(body)
	var prefixes []string
	unborn := false
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(text, "ref-prefix ") {
			prefixes = append(prefixes, text[len("ref-prefix "):])
		} else if text == "unborn" {
			unborn = true
		}
	}
	var out []string
	for _, l := range refLines(repo) {
		if len(prefixes) > 0 {
			matched := false
			for _, p := range prefixes {
				if strings.HasPrefix(l.name, p) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if l.unborn {
			if unborn {
				text := "unborn " + l.name
				if l.symrefTarget != "" {
					text += " symref-target:" + l.symrefTarget
				}
				out = append(out, text+"\n")
			}
			continue
		}
		text := l.oid + " " + l.name
		if l.symrefTarget != "" {
			text += " symref-target:" + l.symrefTarget
		}
		if l.hasPeeled {
			text += " peeled:" + l.peeled
		}
		out = append(out, text+"\n")
	}
	items := make([][]byte, 0, len(out)+1)
	for _, s := range out {
		items = append(items, []byte(s))
	}
	items = append(items, nil)
	response := pktLines(items)
	cacheSet(repo.LsRefsCache, key, response)
	return response
}

// ---------------------------------------------------------------------------
// fetch (v2 and v0)
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // 0 means no depth limit
}

func parseV2FetchArgs(body []byte) fetchArgs {
	lines, _ := decodePktLines(body)
	var args fetchArgs
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil && n > 0 {
				args.depth = n
			}
		}
	}
	return args
}

func parseV0FetchArgs(body []byte) fetchArgs {
	lines, _ := decodePktLines(body)
	var args fetchArgs
	seenFlush := false
	for _, line := range lines {
		if line == lineFlush {
			seenFlush = true
			continue
		}
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			rest := strings.TrimSpace(text[5:])
			if i := strings.IndexByte(rest, ' '); i >= 0 {
				rest = rest[:i]
			}
			args.wants = append(args.wants, rest)
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil && n > 0 {
				args.depth = n
			}
		case seenFlush && strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		}
	}
	return args
}

// planFetch computes the objects a fetch must send, and the shallow-info
// lines for a depth request.
func planFetch(repo *Repo, args fetchArgs) (pack []byte, shallow, unshallow []string) {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	// What the client already holds: the closure of its haves, stopping at
	// its shallow boundaries (a shallow clone holds a boundary commit and
	// its tree, not the boundary's parents).
	var stopAt map[string]bool
	if len(args.shallow) > 0 {
		stopAt = make(map[string]bool, len(args.shallow))
		for _, s := range args.shallow {
			stopAt[s] = true
		}
	}
	held, _ := CollectReachable(args.haves, lookup, 0, stopAt)
	objects, shallowSet := CollectReachable(args.wants, lookup, args.depth, nil)
	packObjects := make(map[string]PackInput, len(objects))
	for oid := range objects {
		if held[oid] {
			continue
		}
		if obj := repo.Objects[oid]; obj != nil {
			packObjects[oid] = PackInput{Kind: obj.Kind, Data: obj.Data}
		}
	}
	pack = writePackKnown(packObjects)
	if args.depth > 0 {
		shallow = shallowSet
		shallowLookup := make(map[string]bool, len(shallow))
		for _, s := range shallow {
			shallowLookup[s] = true
		}
		for _, old := range args.shallow {
			if objects[old] && !shallowLookup[old] {
				unshallow = append(unshallow, old)
			}
		}
	}
	return pack, shallow, unshallow
}

// sidebandPack splits pack bytes into sideband band-1 pkt-lines (32 KiB of
// data each), built into ONE buffer: the frame sizes are known up front, so
// no per-chunk concat is needed.
func sidebandPack(pack []byte) []byte {
	const chunk = 32768
	if len(pack) == 0 {
		return nil
	}
	chunks := (len(pack) + chunk - 1) / chunk
	total := 0
	for i := 0; i < chunks; i++ {
		n := chunk
		if rem := len(pack) - i*chunk; rem < n {
			n = rem
		}
		total += 4 + 1 + n
	}
	out := make([]byte, total)
	pos := 0
	const hexd = "0123456789abcdef"
	for i := 0; i < chunks; i++ {
		start := i * chunk
		n := chunk
		if rem := len(pack) - start; rem < n {
			n = rem
		}
		// 4-hex length of (n + 5): 4 header + 1 band byte + n data bytes.
		v := n + 5
		out[pos] = hexd[v>>12&0xf]
		out[pos+1] = hexd[v>>8&0xf]
		out[pos+2] = hexd[v>>4&0xf]
		out[pos+3] = hexd[v&0xf]
		pos += 4
		out[pos] = 1
		pos++
		copy(out[pos:pos+n], pack[start:start+n])
		pos += n
	}
	return out
}

// HandleFetchV2 answers a v2 fetch command.
func HandleFetchV2(repo *Repo, body []byte) []byte {
	key := cacheKey(repo, body)
	if cached, ok := repo.FetchV2Cache[key]; ok {
		return cached
	}
	args := parseV2FetchArgs(body)
	pack, shallow, unshallow := planFetch(repo, args)
	var out []byte
	out = pktLineInto(out, []byte("acknowledgments\n"))
	out = pktLineInto(out, []byte("NAK\n"))
	out = append(out, delimPkt...)
	if args.depth > 0 {
		out = pktLineInto(out, []byte("shallow-info\n"))
		for _, oid := range shallow {
			out = pktLineInto(out, []byte("shallow "+oid+"\n"))
		}
		for _, oid := range unshallow {
			out = pktLineInto(out, []byte("unshallow "+oid+"\n"))
		}
		out = append(out, delimPkt...)
	}
	out = pktLineInto(out, []byte("packfile\n"))
	out = append(out, sidebandPack(pack)...)
	out = append(out, flushPkt...)
	cacheSet(repo.FetchV2Cache, key, out)
	return out
}

// HandleFetchV0 answers a v0 fetch.
func HandleFetchV0(repo *Repo, body []byte) []byte {
	key := cacheKey(repo, body)
	if cached, ok := repo.FetchV0Cache[key]; ok {
		return cached
	}
	args := parseV0FetchArgs(body)
	pack, shallow, unshallow := planFetch(repo, args)
	var out []byte
	for _, oid := range shallow {
		out = pktLineInto(out, []byte("shallow "+oid+"\n"))
	}
	for _, oid := range unshallow {
		out = pktLineInto(out, []byte("unshallow "+oid+"\n"))
	}
	out = pktLineInto(out, []byte("NAK\n"))
	out = append(out, sidebandPack(pack)...)
	out = append(out, flushPkt...)
	cacheSet(repo.FetchV0Cache, key, out)
	return out
}

// ---------------------------------------------------------------------------
// receive-pack (v0 push)
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// FirstPushLineOk validates the first pkt-line of a receive-pack body:
// `<40-hex> <40-hex> refs/...`, ignoring a trailing newline and anything
// after a NUL (the capability list). Checked without a regex.
func FirstPushLineOk(body []byte) bool {
	length := pktLenAt(body, 0)
	if length < 4 || length > len(body) {
		return false
	}
	line := string(body[4:length])
	if i := strings.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSuffix(line, "\n")
	if len(line) < 87 {
		return false
	}
	if line[40] != ' ' || line[81] != ' ' {
		return false
	}
	if !strings.HasPrefix(line[82:], "refs/") {
		return false
	}
	return IsOID(line[:40]) && IsOID(line[41:81])
}

// parsePushBody parses a receive-pack body into its ref commands and the
// pack bytes, reading the pkt-lines straight out of the buffer: no generic
// decode, no regex, and the pack bytes are never copied (the rest is a
// sub-slice view). Lines that do not parse as `<old> <new> <ref>` are
// skipped.
func parsePushBody(body []byte) (commands []pushCommand, pack []byte) {
	off := 0
	for off+4 <= len(body) {
		length := pktLenAt(body, off)
		if length == -1 {
			break
		}
		if length == 0 {
			off += 4
			break
		}
		if length == 1 || length == 2 {
			off += 4
			continue
		}
		if length < 4 || off+length > len(body) {
			break
		}
		line := string(body[off+4 : off+length])
		off += length
		text := strings.TrimSuffix(line, "\n")
		if i := strings.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		sp1 := strings.IndexByte(text, ' ')
		sp2 := -1
		if sp1 >= 0 {
			sp2 = strings.IndexByte(text[sp1+1:], ' ')
			if sp2 >= 0 {
				sp2 += sp1 + 1
			}
		}
		if sp1 < 0 || sp2 < 0 || strings.IndexByte(text[sp2+1:], ' ') >= 0 {
			continue
		}
		oldOID, newOID, ref := text[:sp1], text[sp1+1:sp2], text[sp2+1:]
		if !IsOID(oldOID) || !IsOID(newOID) {
			continue
		}
		commands = append(commands, pushCommand{oldOID: oldOID, newOID: newOID, ref: ref})
	}
	return commands, body[off:]
}

type pushReportLine struct {
	ref    string
	ok     bool
	reason string
}

// HandleReceivePack runs receive-pack: parse the ref commands, unpack the
// pack, apply the ref updates. Returns the HTTP status and the report body.
func HandleReceivePack(forge *Forge, repo *Repo, body []byte) (int, []byte) {
	commands, pack := parsePushBody(body)
	var report []pushReportLine
	unpackOk := true
	updatesRef := false
	for _, c := range commands {
		if c.newOID != ZeroOID {
			updatesRef = true
			break
		}
	}
	if updatesRef && len(pack) > 0 {
		objects, err := ReadPack(pack)
		if err != nil {
			unpackOk = false
			for _, c := range commands {
				report = append(report, pushReportLine{ref: c.ref, reason: "unpack failed"})
			}
			return 200, pushReportBody(unpackOk, report)
		}
		forge.PutPackObjects(repo, objects)
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	for _, cmd := range commands {
		current, exists := repo.Refs[cmd.ref]
		if cmd.newOID == ZeroOID {
			// Delete.
			switch {
			case !exists:
				report = append(report, pushReportLine{ref: cmd.ref, reason: "non-fast-forward"})
			case cmd.oldOID != ZeroOID && cmd.oldOID != current:
				report = append(report, pushReportLine{ref: cmd.ref, reason: "non-fast-forward"})
			default:
				forge.DeleteRef(repo, cmd.ref)
				report = append(report, pushReportLine{ref: cmd.ref, ok: true})
			}
			continue
		}
		if repo.Objects[cmd.newOID] == nil {
			report = append(report, pushReportLine{ref: cmd.ref, reason: "non-fast-forward"})
			continue
		}
		if cmd.oldOID == ZeroOID {
			// Create.
			if exists {
				report = append(report, pushReportLine{ref: cmd.ref, reason: "already exists"})
			} else {
				forge.SetRef(repo, cmd.ref, cmd.newOID)
				report = append(report, pushReportLine{ref: cmd.ref, ok: true})
			}
			continue
		}
		if !exists || cmd.oldOID != current {
			report = append(report, pushReportLine{ref: cmd.ref, reason: "non-fast-forward"})
			continue
		}
		if !IsAncestor(cmd.oldOID, cmd.newOID, lookup) {
			report = append(report, pushReportLine{ref: cmd.ref, reason: "non-fast-forward"})
			continue
		}
		forge.SetRef(repo, cmd.ref, cmd.newOID)
		report = append(report, pushReportLine{ref: cmd.ref, ok: true})
	}
	return 200, pushReportBody(unpackOk, report)
}

// pushReportBody builds the report-status stream inside sideband band 1,
// into one buffer.
func pushReportBody(unpackOk bool, lines []pushReportLine) []byte {
	unpackText := "unpack ok\n"
	if !unpackOk {
		unpackText = "unpack error\n"
	}
	// Inner pkt-line lengths, computed arithmetically so the whole report
	// is built in one allocation with no intermediate strings.
	innerLen := 4 + len(unpackText)
	for _, e := range lines {
		if e.ok {
			innerLen += 4 + 3 + len(e.ref) + 1 // "ok " + ref + "\n"
		} else {
			innerLen += 4 + 3 + len(e.ref) + 1 + len(e.reason) + 1 // "ng " + ref + " " + reason + "\n"
		}
	}
	out := make([]byte, 0, 4+1+innerLen+4)
	out = appendPktLen(out, innerLen+5)
	out = append(out, 1)
	out = appendPktLen(out, len(unpackText)+4)
	out = append(out, unpackText...)
	for _, e := range lines {
		if e.ok {
			out = appendPktLen(out, len(e.ref)+8)
			out = append(out, "ok "...)
			out = append(out, e.ref...)
			out = append(out, '\n')
		} else {
			out = appendPktLen(out, len(e.ref)+len(e.reason)+9)
			out = append(out, "ng "...)
			out = append(out, e.ref...)
			out = append(out, ' ')
			out = append(out, e.reason...)
			out = append(out, '\n')
		}
	}
	out = append(out, flushPkt...)
	return out
}
