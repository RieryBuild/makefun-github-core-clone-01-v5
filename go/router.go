// Routing, authentication, and the response envelope.
//
// Routes:
//   GET  /healthz                                  -> {"ok": true} (no auth)
//   POST /mcp                                     -> MCP (auth)
//   POST /api/graphql                             -> GraphQL (auth)
//   *    /api/v3/...                              -> REST v3 (auth, except GET /api/v3[/] and /meta)
//   GET  /{owner}/{repo}.git/info/refs            -> smart-HTTP advertisements (no auth)
//   POST /{owner}/{repo}.git/git-upload-pack      -> v2 ls-refs / v2 or v0 fetch (no auth)
//   POST /{owner}/{repo}.git/git-receive-pack     -> v0 push (no auth)
//   *    anything else                            -> 404 {"message": "Not Found"}
//
// Authentication: when MAKEFUN_SHOP_TOKEN is set, the Bearer token must match
// it exactly; when it is unset (local development), any non-empty Bearer is
// accepted; no Authorization header is always a 401. Every response carries a
// non-empty X-GitHub-Request-Id (the transport sets it on structured replies;
// pre-built replies bake a rotating id into their bytes).
//
// Fast paths: the benchmark's hot requests are matched on the raw target
// bytes before any decoding or segment splitting:
//   - GET /healthz, GET /api/v3[/], GET /api/v3/meta answer from pools of
//     complete pre-built responses (built once at router creation);
//   - GET /repos/{o}/{r}/issues/{n} and POST /repos/{o}/{r}/issues scan the
//     target for owner/repo/number and call the focused REST handlers
//     directly. Any percent-escape in the scanned region, any non-digit tail
//     on the issue number, or any extra segment falls through to the generic
//     dispatch, so the fast paths can never diverge from it.
//
// The hot path allocates nothing after warm-up: the target is scanned as
// bytes, the repo lookup hits a one-entry cache keyed by the raw name bytes,
// and the issue number is parsed from digits in place.

package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Request is one fully-parsed request, in transport-neutral form. The body
// is complete. Target and header values are sub-slices of the connection
// buffer, valid only for the duration of the dispatch.
type Request struct {
	Method      []byte
	Target      []byte
	QueryString []byte // raw query string, without the '?'
	Auth        []byte // nil when the header is absent
	GitProtocol []byte // nil when absent
	Host        []byte
	KeepAlive   bool
	Body        []byte // nil when the request had no body
	pathCache   string
	pathDone    bool
}

// Path returns the decoded pathname (percent-escapes decoded, like
// URL.pathname), computed lazily.
func (r *Request) Path() string {
	if !r.pathDone {
		r.pathDone = true
		raw := r.Target
		if i := bytes.IndexByte(raw, '?'); i >= 0 {
			raw = raw[:i]
		}
		if bytes.IndexByte(raw, '%') < 0 {
			r.pathCache = string(raw)
		} else if decoded, err := url.PathUnescape(string(raw)); err == nil {
			r.pathCache = decoded
		} else {
			r.pathCache = string(raw)
		}
	}
	return r.pathCache
}

// Reply is what the dispatch returns: a structured reply (the transport
// builds the status line and headers) or a complete pre-built response.
type Reply struct {
	Status      int
	ContentType string
	Body        []byte
	Extra       [][2]string
	Prebuilt    []byte // non-nil: the whole response bytes, written as-is
}

var (
	notFoundBody     = []byte(`{"message":"Not Found"}`)
	unauthBody       = []byte(`{"message":"Requires authentication"}`)
	badServiceBody   = []byte(`{"message":"Unknown service"}`)
	emptyBody        = []byte(`{"message":"Empty request"}`)
	unknownCmdBody   = []byte(`{"message":"Unknown command"}`)
	malformedBody    = []byte(`{"message":"Malformed request"}`)
	healthzBody      = []byte(`{"ok":true}`)
	apiRootBody      = []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`)
	metaBody         = []byte(`{"installed_version":"1.0.0"}`)
	internalErrBody  = []byte(`{"message":"Internal Server Error"}`)
)

func jsonReply(status int, body []byte) Reply {
	return Reply{Status: status, ContentType: "application/json", Body: body}
}

func toReply(result RestResult) Reply {
	return Reply{Status: result.Status, ContentType: "application/json", Body: result.Body, Extra: result.Headers}
}

// staticPool is a pool of complete pre-built responses, one per request id.
type staticPool struct {
	bufs [][]byte
	next int
}

func buildStaticPool(statusLine, extraHeaders string, body []byte) *staticPool {
	bufs := make([][]byte, requestIDPoolSize)
	for i := 0; i < requestIDPoolSize; i++ {
		head := statusLine + "\r\ncontent-type: application/json\r\nx-github-request-id: " + requestIDs[i] + "\r\n" + extraHeaders + "content-length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
		full := make([]byte, 0, len(head)+len(body))
		full = append(full, head...)
		full = append(full, body...)
		bufs[i] = full
	}
	return &staticPool{bufs: bufs}
}

func (p *staticPool) nextBuf() []byte {
	buf := p.bufs[p.next]
	p.next = (p.next + 1) & (requestIDPoolSize - 1)
	return buf
}

// Dispatcher routes requests to the forge's surfaces.
type Dispatcher struct {
	forge        *Forge
	expectedAuth []byte // "Bearer <token>", or nil when no token is configured
	expectedTok  string

	healthzPool *staticPool
	apiRootPool *staticPool
	metaPool    *staticPool

	// One-entry repo cache for the hot path: the last repo name seen (raw
	// bytes) and its repo, so a repeated hot request never allocates a
	// string for the map lookup.
	lastRepoName []byte
	lastRepo     *Repo
	lastRepoGen  int
}

// NewDispatcher builds a dispatcher for a forge.
func NewDispatcher(forge *Forge) *Dispatcher {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	d := &Dispatcher{forge: forge, expectedTok: token}
	if token != "" {
		d.expectedAuth = []byte("Bearer " + token)
	}
	d.healthzPool = buildStaticPool("HTTP/1.1 200 OK", "", healthzBody)
	d.apiRootPool = buildStaticPool("HTTP/1.1 200 OK", "x-oauth-scopes: repo, user\r\n", apiRootBody)
	d.metaPool = buildStaticPool("HTTP/1.1 200 OK", "", metaBody)
	return d
}

// authenticate reports whether the Bearer token is valid for this request.
// The exact-match fast path covers every suite call; the trim-and-compare
// fallback keeps the semantics identical to a full parse.
func (d *Dispatcher) authenticate(auth []byte) bool {
	if auth == nil {
		return false
	}
	if d.expectedAuth != nil {
		if bytes.Equal(auth, d.expectedAuth) {
			return true
		}
		if !bytes.HasPrefix(auth, []byte("Bearer ")) {
			return false
		}
		return strings.TrimSpace(string(auth[7:])) == d.expectedTok
	}
	if !bytes.HasPrefix(auth, []byte("Bearer ")) {
		return false
	}
	return strings.TrimSpace(string(auth[7:])) != ""
}

// queryParam reads one query parameter without building a map: it walks the
// `&`-separated segments and matches `name` (bare, meaning "") or
// `name=value` exactly, so `service` never matches `xservice`. The first
// occurrence wins. Percent-escapes are decoded only when the value carries
// one; '+' is left as-is (the values read here never use it).
func queryParam(qs []byte, name string) (string, bool) {
	i := 0
	for i <= len(qs) {
		end := bytes.IndexByte(qs[i:], '&')
		var seg []byte
		if end < 0 {
			seg = qs[i:]
			i = len(qs) + 1
		} else {
			seg = qs[i : i+end]
			i += end + 1
		}
		if string(seg) == name {
			return "", true
		}
		if bytes.HasPrefix(seg, []byte(name+"=")) {
			raw := seg[len(name)+1:]
			if bytes.IndexByte(raw, '%') < 0 {
				return string(raw), true
			}
			if decoded, err := url.QueryUnescape(string(raw)); err == nil {
				return decoded, true
			}
			return string(raw), true
		}
		if end < 0 {
			break
		}
	}
	return "", false
}

// scanRepoTarget scans a `/api/v3/repos/{owner}/{repo}/...` target. It
// returns false when the target does not match, or when any scanned segment
// carries a percent-escape (fall through to the generic dispatch, which
// decodes).
func scanRepoTarget(target []byte) (owner, repo []byte, restStart int, ok bool) {
	const prefix = "/api/v3/repos/"
	if !bytes.HasPrefix(target, []byte(prefix)) {
		return nil, nil, 0, false
	}
	i := len(prefix)
	ownerStart := i
	for i < len(target) && target[i] != '/' {
		if target[i] == '%' {
			return nil, nil, 0, false
		}
		i++
	}
	if i == ownerStart || i >= len(target) {
		return nil, nil, 0, false
	}
	owner = target[ownerStart:i]
	i++
	repoStart := i
	for i < len(target) && target[i] != '/' {
		if target[i] == '%' {
			return nil, nil, 0, false
		}
		i++
	}
	if i == repoStart {
		return nil, nil, 0, false
	}
	return owner, target[repoStart:i], i, true
}

// cachedRepo resolves a repo name, using the one-entry hot cache. The
// cache is valid while the forge's repo generation is unchanged (no repo
// created or deleted since it was filled), so a repeated hot request never
// allocates a string for the map lookup.
func (d *Dispatcher) cachedRepo(name []byte) *Repo {
	if d.lastRepo != nil && d.lastRepoGen == d.forge.repoGen && bytes.Equal(d.lastRepoName, name) {
		return d.lastRepo
	}
	repo := d.forge.GetRepo(string(name))
	d.lastRepoGen = d.forge.repoGen
	if repo != nil {
		d.lastRepoName = append(d.lastRepoName[:0], name...)
		d.lastRepo = repo
	} else {
		d.lastRepo = nil
	}
	return repo
}

// tryGetIssue is the GET-issue fast path: GET /api/v3/repos/{o}/{r}/issues/{n}
// (n = digits only, then end or '?'). ok=false means "not this shape".
func (d *Dispatcher) tryGetIssue(req *Request) (Reply, bool) {
	owner, repoName, restStart, ok := scanRepoTarget(req.Target)
	if !ok {
		return Reply{}, false
	}
	target := req.Target
	if !bytes.HasPrefix(target[restStart:], []byte("/issues/")) {
		return Reply{}, false
	}
	i := restStart + len("/issues/")
	numStart := i
	for i < len(target) && target[i] >= '0' && target[i] <= '9' {
		i++
	}
	if i == numStart {
		return Reply{}, false
	}
	if i < len(target) && target[i] != '?' {
		return Reply{}, false
	}
	if !bytes.Equal(owner, []byte(ViewerLogin)) {
		return jsonReply(404, notFoundBody), true
	}
	repo := d.cachedRepo(repoName)
	if repo == nil {
		return jsonReply(404, notFoundBody), true
	}
	number := 0
	for j := numStart; j < i; j++ {
		number = number*10 + int(target[j]-'0')
	}
	result, prebuilt := GetIssueReply(repo, number)
	if prebuilt != nil {
		return Reply{Prebuilt: prebuilt}, true
	}
	return toReply(result), true
}

// tryPostIssue is the POST-issue fast path: POST /api/v3/repos/{o}/{r}/issues
// (then end or '?'). ok=false means "not this shape".
func (d *Dispatcher) tryPostIssue(req *Request) (Reply, bool) {
	owner, repoName, restStart, ok := scanRepoTarget(req.Target)
	if !ok {
		return Reply{}, false
	}
	target := req.Target
	if !bytes.HasPrefix(target[restStart:], []byte("/issues")) {
		return Reply{}, false
	}
	after := restStart + len("/issues")
	if after < len(target) && target[after] != '?' {
		return Reply{}, false
	}
	if !bytes.Equal(owner, []byte(ViewerLogin)) {
		return jsonReply(404, notFoundBody), true
	}
	repo := d.cachedRepo(repoName)
	if repo == nil {
		return jsonReply(404, notFoundBody), true
	}
	return toReply(CreateIssueReply(d.forge, repo, req.Body)), true
}

// gitProtocolHasVersion2 reports whether a Git-Protocol header value
// carries the version=2 token. The value is a comma-separated list, so an
// exact match would miss `version=2,agent=...` and wrongly fall back to v0.
func gitProtocolHasVersion2(v []byte) bool {
	for len(v) > 0 {
		token := v
		if i := bytes.IndexByte(v, ','); i >= 0 {
			token, v = v[:i], v[i+1:]
		} else {
			v = nil
		}
		if bytes.Equal(bytes.TrimSpace(token), []byte("version=2")) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) handleGit(req *Request) Reply {
	method := string(req.Method)
	path := req.Path()
	// The second path segment must end in '.git' (owner and repo non-empty
	// with no '/' inside either; action non-empty) — the edge cases a plain
	// indexOf('.git/') gets wrong, like a name that itself contains '.git'.
	if len(path) == 0 || path[0] != '/' {
		return jsonReply(404, notFoundBody)
	}
	s1 := strings.IndexByte(path[1:], '/')
	if s1 < 0 {
		return jsonReply(404, notFoundBody)
	}
	s1++
	if s1 <= 1 {
		return jsonReply(404, notFoundBody)
	}
	s2 := strings.IndexByte(path[s1+1:], '/')
	if s2 < 0 {
		return jsonReply(404, notFoundBody)
	}
	s2 += s1 + 1
	segment := path[s1+1 : s2]
	if len(segment) <= 4 || !strings.HasSuffix(segment, ".git") {
		return jsonReply(404, notFoundBody)
	}
	action := path[s2+1:]
	if action == "" {
		return jsonReply(404, notFoundBody)
	}
	owner := path[1:s1]
	repoName := segment[:len(segment)-4]
	if owner != ViewerLogin {
		return jsonReply(404, notFoundBody)
	}
	repo := d.forge.GetRepo(repoName)
	if repo == nil {
		return jsonReply(404, notFoundBody)
	}

	if action == "info/refs" && method == "GET" {
		service, _ := queryParam(req.QueryString, "service")
		if service == "git-upload-pack" {
			if gitProtocolHasVersion2(req.GitProtocol) {
				return Reply{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v2AdvertisementBytes}
			}
			return Reply{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: V0Advertisement(repo, "git-upload-pack")}
		}
		if service == "git-receive-pack" {
			return Reply{Status: 200, ContentType: "application/x-git-receive-pack-advertisement", Body: V0Advertisement(repo, "git-receive-pack")}
		}
		return jsonReply(400, badServiceBody)
	}

	if action == "git-upload-pack" && method == "POST" {
		body := req.Body
		if len(body) == 0 {
			return jsonReply(400, emptyBody)
		}
		lines, _ := decodePktLines(body)
		first := ""
		if len(lines) > 0 {
			first = lines[0]
		}
		if gitProtocolHasVersion2(req.GitProtocol) {
			if strings.HasPrefix(first, "command=ls-refs") {
				return Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: HandleLsRefs(repo, body)}
			}
			if strings.HasPrefix(first, "command=fetch") {
				return Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: HandleFetchV2(repo, body)}
			}
			return jsonReply(400, unknownCmdBody)
		}
		if strings.HasPrefix(first, "want ") {
			return Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: HandleFetchV0(repo, body)}
		}
		return jsonReply(400, malformedBody)
	}

	if action == "git-receive-pack" && method == "POST" {
		body := req.Body
		if len(body) == 0 {
			return jsonReply(400, emptyBody)
		}
		if !FirstPushLineOk(body) {
			return jsonReply(400, malformedBody)
		}
		status, report := HandleReceivePack(d.forge, repo, body)
		return Reply{Status: status, ContentType: "application/x-git-receive-pack-result", Body: report}
	}

	return jsonReply(404, notFoundBody)
}

func (d *Dispatcher) handleApiV3(req *Request) Reply {
	method := string(req.Method)
	path := req.Path()
	isPublicRoot := method == "GET" && (path == "/api/v3" || path == "/api/v3/" || path == "/api/v3/meta")
	if !isPublicRoot && !d.authenticate(req.Auth) {
		return jsonReply(401, unauthBody)
	}
	var body []byte
	if method != "GET" && method != "HEAD" {
		body = req.Body
	}
	// Manual segment split: one pass, no intermediate arrays, and
	// PathUnescape only for segments that actually carry a percent-escape.
	rest := path[len("/api/v3"):]
	var segments []string
	start := 0
	for i := 0; i <= len(rest); i++ {
		if i == len(rest) || rest[i] == '/' {
			if i > start {
				seg := rest[start:i]
				if strings.IndexByte(seg, '%') >= 0 {
					if decoded, err := url.PathUnescape(seg); err == nil {
						seg = decoded
					}
				}
				segments = append(segments, seg)
			}
			start = i + 1
		}
	}
	ctx := &RestCtx{
		Method:      method,
		Segments:    segments,
		QueryString: string(req.QueryString),
		Body:        body,
		Host:        string(req.Host),
		RawTarget:   string(req.Target),
	}
	return toReply(HandleRest(d.forge, ctx))
}

// Dispatch routes one request. It holds the forge mutex for the whole call;
// the transport writes the reply after Dispatch returns, outside the lock.
func (d *Dispatcher) Dispatch(req *Request) (reply Reply) {
	d.forge.Lock()
	defer d.forge.Unlock()
	defer func() {
		if r := recover(); r != nil {
			// A handler panic is a 500, never a dead connection. Log the
			// panic with a stack trace so a bug here is diagnosable.
			fmt.Fprintf(os.Stderr, "[forge] dispatch panic: %v\n%s\n", r, debug.Stack())
			reply = jsonReply(500, internalErrBody)
		}
	}()

	method := string(req.Method)
	target := req.Target

	// Static fast paths (exact raw targets; a query string falls through to
	// the generic dispatch, which answers the same way). HEAD takes the
	// structured form so the transport withholds the body; GET takes the pool.
	if method == "GET" || method == "HEAD" {
		if bytes.Equal(target, []byte("/healthz")) {
			if method == "GET" {
				return Reply{Prebuilt: d.healthzPool.nextBuf()}
			}
			return jsonReply(200, healthzBody)
		}
		if bytes.Equal(target, []byte("/api/v3")) || bytes.Equal(target, []byte("/api/v3/")) {
			if method == "GET" {
				return Reply{Prebuilt: d.apiRootPool.nextBuf()}
			}
			return Reply{Status: 200, ContentType: "application/json", Body: apiRootBody, Extra: [][2]string{{"x-oauth-scopes", "repo, user"}}}
		}
		if bytes.Equal(target, []byte("/api/v3/meta")) {
			if method == "GET" {
				return Reply{Prebuilt: d.metaPool.nextBuf()}
			}
			return jsonReply(200, metaBody)
		}
	}

	path := req.Path()

	if path == "/mcp" && method == "POST" {
		if !d.authenticate(req.Auth) {
			return jsonReply(401, unauthBody)
		}
		status, body := HandleMcp(d.forge, req.Body)
		return Reply{Status: status, ContentType: "application/json", Body: body}
	}

	if path == "/api/graphql" && method == "POST" {
		if !d.authenticate(req.Auth) {
			return jsonReply(401, unauthBody)
		}
		status, body := HandleGraphql(d.forge, req.Body)
		return Reply{Status: status, ContentType: "application/json", Body: body}
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		// Hot REST paths first. Public roots never need auth; the fast paths
		// do (auth precedes the 404, as in the generic dispatch).
		if method == "GET" || method == "POST" {
			isPublicRoot := method == "GET" && (path == "/api/v3" || path == "/api/v3/" || path == "/api/v3/meta")
			if !isPublicRoot {
				if !d.authenticate(req.Auth) {
					return jsonReply(401, unauthBody)
				}
				if method == "GET" {
					if fast, ok := d.tryGetIssue(req); ok {
						return fast
					}
				} else {
					if fast, ok := d.tryPostIssue(req); ok {
						return fast
					}
				}
			}
		}
		return d.handleApiV3(req)
	}

	if len(path) > 0 && path[0] == '/' {
		// handleGit answers 404 for non-git paths too; that is the same 404
		// the generic dispatch would return, so it is safe to return directly.
		return d.handleGit(req)
	}

	return jsonReply(404, notFoundBody)
}
