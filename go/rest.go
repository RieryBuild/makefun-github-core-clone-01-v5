// The REST v3 surface (/api/v3): repositories, branches, commits, git refs,
// contents, issues, labels and pull requests, over the forge's in-memory
// store.
//
// The suite's client pins down the contract:
//   - every reply is JSON with a non-empty X-GitHub-Request-Id (set by the
//     transport / baked into pre-built responses);
//   - unknown route -> 404 {"message"}; malformed JSON body -> 400 {"message"};
//   - a repository under another owner -> 404, never 403;
//   - POST /user/repos with a taken name -> 422 {"message": "...already exists..."};
//     the default branch is 'main' when the body omits it;
//   - GET /user/repos paginates with a Link header carrying an absolute
//     same-origin next URL;
//   - issues and pull requests share one per-repo number counter;
//   - contents commits are real git objects (blob/tree/commit with the given
//     message, trial-epoch timestamps, the forge identity), an update with a
//     stale sha -> 409, a PUT to a missing branch creates that branch from a
//     commit whose parent is the default tip;
//   - a squash merge writes one commit on the base branch (parent = base tip,
//     tree = base tree with the head's changes, message
//     "Merge pull request #N from <head>") and a conflicting merge -> 405.
//
// Hot paths: the router calls GetIssueReply / CreateIssueReply directly for
// GET/POST /repos/{o}/{r}/issues[/n], bypassing the generic segment dispatch.
// GET /issues/{n} answers from a pool of complete pre-built responses
// (status line, headers and body joined, rotating request ids), built lazily
// on the first GET of an issue and dropped whenever the issue mutates.

package main

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// RestCtx is one REST request in dispatch form.
type RestCtx struct {
	Method      string
	Segments    []string // path segments after /api/v3 (already URL-decoded)
	QueryString string
	Body        []byte // nil when the request had no body
	Host        string // the Host header, for absolute pagination URLs
	RawTarget   string // path with query string
	paramsCache *queryParams
}

// RestResult is one REST reply.
type RestResult struct {
	Status  int
	Body    []byte
	Headers [][2]string
}

// queryParams is a parsed query string: ordered pairs plus a first-value map.
type queryParams struct {
	pairs []queryPair
	byKey map[string]string
}

type queryPair struct {
	key, val string
}

func parseQuery(qs string) *queryParams {
	qp := &queryParams{byKey: make(map[string]string)}
	if qs == "" {
		return qp
	}
	for _, seg := range strings.Split(qs, "&") {
		if seg == "" {
			continue
		}
		key, val := seg, ""
		if i := strings.IndexByte(seg, '='); i >= 0 {
			key, val = seg[:i], seg[i+1:]
		}
		key = queryUnescape(key)
		val = queryUnescape(val)
		qp.pairs = append(qp.pairs, queryPair{key, val})
		if _, ok := qp.byKey[key]; !ok {
			qp.byKey[key] = val
		}
	}
	return qp
}

// queryUnescape decodes %XX and '+' (as space), like URLSearchParams; a
// malformed escape is left literal.
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

func (c *RestCtx) params() *queryParams {
	if c.paramsCache == nil {
		c.paramsCache = parseQuery(c.QueryString)
	}
	return c.paramsCache
}

func (q *queryParams) get(key string) string {
	return q.byKey[key]
}

func (q *queryParams) getOr(key, def string) string {
	if v, ok := q.byKey[key]; ok {
		return v
	}
	return def
}

func errResult(status int, message string) RestResult {
	body := []byte(`{"message":`)
	body = jsonQuote(body, message)
	body = append(body, '}')
	return RestResult{Status: status, Body: body}
}

func jsonBytesResult(status int, body []byte, headers ...[2]string) RestResult {
	return RestResult{Status: status, Body: body, Headers: headers}
}

// parseRestBody parses a REST request body as a JSON object; on failure it
// returns the 400 result to send.
func parseRestBody(body []byte) (objBody, *RestResult) {
	parsed, ok := parseObjBody(body)
	if !ok {
		r := errResult(400, "Problems parsing JSON")
		return nil, &r
	}
	return parsed, nil
}

// ---------------------------------------------------------------------------
// serializers (hand-rolled appends; field order matches the reference)
// ---------------------------------------------------------------------------

// RepoJSON returns the repo serializer, cached on the repo and rebuilt only
// when its fields change.
func RepoJSON(repo *Repo) string {
	if !repo.HasRepoJSON {
		var b []byte
		b = append(b, `{"id":`...)
		b = itoa(b, repo.ID)
		b = append(b, `,"name":`...)
		b = jsonQuote(b, repo.Name)
		b = append(b, `,"full_name":`...)
		b = jsonQuote(b, repo.Owner+"/"+repo.Name)
		b = append(b, `,"owner":{"login":`...)
		b = jsonQuote(b, repo.Owner)
		b = append(b, `,"id":`...)
		b = itoa(b, ViewerID)
		b = append(b, `,"type":"User"},"private":`...)
		b = strconv.AppendBool(b, repo.Private)
		b = append(b, `,"description":`...)
		b = jsonQuote(b, repo.Description)
		b = append(b, `,"default_branch":`...)
		b = jsonQuote(b, repo.DefaultBranch)
		b = append(b, `,"created_at":`...)
		b = jsonQuote(b, TrialISO)
		b = append(b, `,"updated_at":`...)
		b = jsonQuote(b, TrialISO)
		b = append(b, `,"html_url":`...)
		b = jsonQuote(b, "https://example.invalid/"+repo.Owner+"/"+repo.Name)
		b = append(b, `,"url":`...)
		b = jsonQuote(b, "/api/v3/repos/"+repo.Owner+"/"+repo.Name)
		b = append(b, `,"clone_url":`...)
		b = jsonQuote(b, "https://example.invalid/"+repo.Owner+"/"+repo.Name+".git")
		b = append(b, '}')
		repo.RepoJSON = string(b)
		repo.HasRepoJSON = true
	}
	return repo.RepoJSON
}

// RepoJSONBuf returns the repo JSON as bytes, built once beside RepoJSON.
func RepoJSONBuf(repo *Repo) []byte {
	if !repo.HasRepoJSONB {
		repo.RepoJSONBuf = []byte(RepoJSON(repo))
		repo.HasRepoJSONB = true
	}
	return repo.RepoJSONBuf
}

// InvalidateRepo drops a repo's cached serializations (called on mutation).
func InvalidateRepo(repo *Repo) {
	repo.HasRepoJSON = false
	repo.HasRepoJSONB = false
}

func issueJSON(repo *Repo, issue *Issue) string {
	var b []byte
	b = append(b, `{"id":`...)
	b = itoa(b, issue.ID)
	b = append(b, `,"number":`...)
	b = itoa(b, issue.Number)
	b = append(b, `,"title":`...)
	b = jsonQuote(b, issue.Title)
	b = append(b, `,"body":`...)
	b = jsonQuote(b, issue.Body)
	b = append(b, `,"state":`...)
	b = jsonQuote(b, issue.State)
	b = append(b, `,"labels":[`...)
	for i, name := range issue.Labels {
		if i > 0 {
			b = append(b, ',')
		}
		color := "000000"
		if l := repo.Labels[name]; l != nil {
			color = l.Color
		}
		b = append(b, `{"name":`...)
		b = jsonQuote(b, name)
		b = append(b, `,"color":`...)
		b = jsonQuote(b, color)
		b = append(b, '}')
	}
	b = append(b, `],"user":{"login":`...)
	b = jsonQuote(b, ViewerLogin)
	b = append(b, `,"id":`...)
	b = itoa(b, ViewerID)
	b = append(b, `,"type":"User"},"created_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"updated_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"html_url":`...)
	b = jsonQuote(b, "https://example.invalid/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number))
	b = append(b, `,"url":`...)
	b = jsonQuote(b, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number))
	b = append(b, '}')
	return string(b)
}

// IssueJSONCached returns an issue's cached serialization as a string.
func IssueJSONCached(repo *Repo, issue *Issue) string {
	if !issue.HasCache {
		issue.CachedJSON = issueJSON(repo, issue)
		issue.CachedBuf = []byte(issue.CachedJSON)
		issue.HasCache = true
	}
	return issue.CachedJSON
}

// IssueBuf returns an issue's cached serialization as bytes.
func IssueBuf(repo *Repo, issue *Issue) []byte {
	if !issue.HasCache {
		issue.CachedJSON = issueJSON(repo, issue)
		issue.CachedBuf = []byte(issue.CachedJSON)
		issue.HasCache = true
	}
	return issue.CachedBuf
}

// ---------------------------------------------------------------------------
// the issue full-response pool (the read workload's hot path)
// ---------------------------------------------------------------------------

type issuePool struct {
	bufs [] []byte
	next int
}

var issuePools = make(map[*Issue]*issuePool)

// IssueResponsePool returns the next pre-built complete GET-issue response
// for an issue (status line, headers and body joined, one buffer per
// request id in the pool). Built lazily on the first GET: a pool costs
// ~64x the response size, so it is only built for issues actually read.
func IssueResponsePool(repo *Repo, issue *Issue) []byte {
	pool := issuePools[issue]
	if pool == nil {
		body := IssueBuf(repo, issue)
		bufs := make([][]byte, requestIDPoolSize)
		for i := 0; i < requestIDPoolSize; i++ {
			head := "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\nx-github-request-id: " + requestIDs[i] + "\r\ncontent-length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
			full := make([]byte, 0, len(head)+len(body))
			full = append(full, head...)
			full = append(full, body...)
			bufs[i] = full
		}
		pool = &issuePool{bufs: bufs}
		issuePools[issue] = pool
	}
	buf := pool.bufs[pool.next]
	pool.next = (pool.next + 1) & (requestIDPoolSize - 1)
	return buf
}

// InvalidateIssue drops an issue's cached serializations AND its response
// pool (on mutation).
func InvalidateIssue(issue *Issue) {
	issue.HasCache = false
	issue.CachedJSON = ""
	issue.CachedBuf = nil
	delete(issuePools, issue)
}

func pullJSON(repo *Repo, pull *Pull) string {
	var b []byte
	b = append(b, `{"id":`...)
	b = itoa(b, pull.ID)
	b = append(b, `,"number":`...)
	b = itoa(b, pull.Number)
	b = append(b, `,"title":`...)
	b = jsonQuote(b, pull.Title)
	b = append(b, `,"body":`...)
	b = jsonQuote(b, pull.Body)
	b = append(b, `,"state":`...)
	b = jsonQuote(b, pull.State)
	b = append(b, `,"merged":`...)
	b = strconv.AppendBool(b, pull.Merged)
	b = append(b, `,"merged_at":`...)
	if pull.MergedAt == "" {
		b = append(b, "null"...)
	} else {
		b = jsonQuote(b, pull.MergedAt)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if pull.MergeCommitSha == "" {
		b = append(b, "null"...)
	} else {
		b = jsonQuote(b, pull.MergeCommitSha)
	}
	b = append(b, `,"head":{"ref":`...)
	b = jsonQuote(b, pull.HeadRef)
	b = append(b, `,"sha":`...)
	b = jsonQuote(b, pull.HeadSha)
	b = append(b, `,"label":`...)
	b = jsonQuote(b, repo.Owner+":"+pull.HeadRef)
	b = append(b, `},"base":{"ref":`...)
	b = jsonQuote(b, pull.BaseRef)
	b = append(b, `,"sha":`...)
	b = jsonQuote(b, pull.BaseSha)
	b = append(b, `,"label":`...)
	b = jsonQuote(b, repo.Owner+":"+pull.BaseRef)
	b = append(b, `},"user":{"login":`...)
	b = jsonQuote(b, ViewerLogin)
	b = append(b, `,"id":`...)
	b = itoa(b, ViewerID)
	b = append(b, `,"type":"User"},"created_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"updated_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"html_url":`...)
	b = jsonQuote(b, "https://example.invalid/"+repo.Owner+"/"+repo.Name+"/pull/"+strconv.Itoa(pull.Number))
	b = append(b, `,"url":`...)
	b = jsonQuote(b, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls/"+strconv.Itoa(pull.Number))
	b = append(b, '}')
	return string(b)
}

func commentJSON(repo *Repo, issue *Issue, comment *Comment) string {
	var b []byte
	b = append(b, `{"id":`...)
	b = itoa(b, comment.ID)
	b = append(b, `,"body":`...)
	b = jsonQuote(b, comment.Body)
	b = append(b, `,"user":{"login":`...)
	b = jsonQuote(b, ViewerLogin)
	b = append(b, `,"id":`...)
	b = itoa(b, ViewerID)
	b = append(b, `,"type":"User"},"created_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"updated_at":`...)
	b = jsonQuote(b, TrialISO)
	b = append(b, `,"html_url":`...)
	b = jsonQuote(b, "https://example.invalid/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)+"#issuecomment-"+strconv.Itoa(comment.ID))
	b = append(b, `,"url":`...)
	b = jsonQuote(b, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)+"/comments/"+strconv.Itoa(comment.ID))
	b = append(b, '}')
	return string(b)
}

// ---------------------------------------------------------------------------
// pagination
// ---------------------------------------------------------------------------

// paginate slices items for the requested page and builds the next-page Link
// header value ("" when there is no next page).
func paginate(total int, ctx *RestCtx) (start, end int, link string) {
	q := ctx.params()
	perPage := 30
	if raw, ok := q.byKey["per_page"]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := 1
	if raw, ok := q.byKey["page"]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	if start+perPage >= total {
		return start, end, ""
	}
	// Rebuild the query with per_page and page set, keeping the other
	// params in their original order (matching URLSearchParams.set).
	var pairs []queryPair
	seenPerPage, seenPage := false, false
	for _, p := range q.pairs {
		switch p.key {
		case "per_page":
			if !seenPerPage {
				pairs = append(pairs, queryPair{"per_page", strconv.Itoa(perPage)})
				seenPerPage = true
			}
		case "page":
			if !seenPage {
				pairs = append(pairs, queryPair{"page", strconv.Itoa(page + 1)})
				seenPage = true
			}
		default:
			pairs = append(pairs, p)
		}
	}
	if !seenPerPage {
		pairs = append(pairs, queryPair{"per_page", strconv.Itoa(perPage)})
	}
	if !seenPage {
		pairs = append(pairs, queryPair{"page", strconv.Itoa(page + 1)})
	}
	var qs strings.Builder
	for i, p := range pairs {
		if i > 0 {
			qs.WriteByte('&')
		}
		qs.WriteString(url.QueryEscape(p.key))
		qs.WriteByte('=')
		qs.WriteString(url.QueryEscape(p.val))
	}
	path := ctx.RawTarget
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	next := "http://" + ctx.Host + path + "?" + qs.String()
	return start, end, "<" + next + `>; rel="next"`
}

func linkHeader(link string) [][2]string {
	if link == "" {
		return nil
	}
	return [][2]string{{"link", link}}
}

// ---------------------------------------------------------------------------
// focused handlers (the router's fast paths call these directly)
// ---------------------------------------------------------------------------

// GetIssueReply answers GET /repos/{o}/{r}/issues/{n}: the read workload's
// hot path. Returns a pre-built full response when one is available.
func GetIssueReply(repo *Repo, number int) (RestResult, []byte) {
	issue := repo.IssuesByNum[number]
	if issue == nil {
		return errResult(404, "Not Found"), nil
	}
	return RestResult{}, IssueResponsePool(repo, issue)
}

// CreateIssueReply answers POST /repos/{o}/{r}/issues: the write workload's
// hot path.
func CreateIssueReply(forge *Forge, repo *Repo, body []byte) RestResult {
	parsed, bad := parseRestBody(body)
	if bad != nil {
		return *bad
	}
	title, ok := parsed.str("title")
	if !ok || title == "" {
		return errResult(422, "title is required")
	}
	text := parsed.strOr("body", "")
	labels := parsed.strList("labels")
	if labels == nil {
		labels = []string{}
	}
	created, errMsg := forge.CreateIssue(repo, title, text, labels)
	if errMsg != "" {
		return errResult(422, errMsg)
	}
	return RestResult{Status: 201, Body: IssueBuf(repo, created)}
}

// ---------------------------------------------------------------------------
// entry point (the generic segment dispatch)
// ---------------------------------------------------------------------------

// HandleRest dispatches a REST v3 request (segments after /api/v3).
func HandleRest(forge *Forge, ctx *RestCtx) RestResult {
	method, segments := ctx.Method, ctx.Segments
	if len(segments) == 0 {
		if method == "GET" {
			return RestResult{
				Status: 200,
				Body:   []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`),
				Headers: [][2]string{{"x-oauth-scopes", "repo, user"}},
			}
		}
		return errResult(404, "Not Found")
	}
	if segments[0] == "meta" && method == "GET" {
		return RestResult{Status: 200, Body: []byte(`{"installed_version":"1.0.0"}`)}
	}
	if segments[0] == "user" {
		return handleUser(forge, ctx)
	}
	if segments[0] == "repos" {
		return handleRepos(forge, ctx)
	}
	return errResult(404, "Not Found")
}

// ---------------------------------------------------------------------------
// /user/*
// ---------------------------------------------------------------------------

func handleUser(forge *Forge, ctx *RestCtx) RestResult {
	method, segments := ctx.Method, ctx.Segments
	if len(segments) == 1 {
		if method == "GET" {
			return RestResult{Status: 200, Body: []byte(`{"login":"` + ViewerLogin + `","id":` + strconv.Itoa(ViewerID) + `,"name":"ada iversen","email":"adaiversen@example.invalid","type":"User"}`)}
		}
		return errResult(404, "Not Found")
	}
	if segments[1] == "repos" {
		if method == "GET" {
			repos := forge.ListViewerRepos()
			start, end, link := paginate(len(repos), ctx)
			var b []byte
			b = append(b, '[')
			for i := start; i < end; i++ {
				if i > start {
					b = append(b, ',')
				}
				b = append(b, RepoJSON(repos[i])...)
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b, Headers: linkHeader(link)}
		}
		if method == "POST" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			name, ok := parsed.str("name")
			if !ok || name == "" {
				return errResult(422, "name is required")
			}
			repo := forge.CreateRepo(name, parsed.boolOr("private", false), parsed.strOr("description", ""), parsed.strOr("default_branch", ""))
			if repo == nil {
				if forge.GetRepo(name) != nil {
					return errResult(422, "Repository already exists")
				}
				return errResult(422, "Invalid repository name")
			}
			return RestResult{Status: 201, Body: []byte(RepoJSON(repo))}
		}
		return errResult(404, "Not Found")
	}
	return errResult(404, "Not Found")
}

// ---------------------------------------------------------------------------
// /repos/{owner}/{repo}/*
// ---------------------------------------------------------------------------

func handleRepos(forge *Forge, ctx *RestCtx) RestResult {
	method, segments := ctx.Method, ctx.Segments
	if len(segments) < 3 {
		return errResult(404, "Not Found")
	}
	owner, name := segments[1], segments[2]
	if owner != ViewerLogin {
		return errResult(404, "Not Found")
	}
	repo := forge.GetRepo(name)
	if repo == nil {
		return errResult(404, "Not Found")
	}
	rest := segments[3:]

	if len(rest) == 0 {
		switch method {
		case "GET":
			return RestResult{Status: 200, Body: RepoJSONBuf(repo)}
		case "PATCH":
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			changed := false
			if desc, ok := parsed.str("description"); ok {
				repo.Description = desc
				changed = true
			}
			if priv, ok := parsed.boolean("private"); ok {
				repo.Private = priv
				changed = true
			}
			if changed {
				InvalidateRepo(repo)
			}
			return RestResult{Status: 200, Body: RepoJSONBuf(repo)}
		case "DELETE":
			forge.DeleteRepo(name)
			return RestResult{Status: 204, Body: []byte{}}
		}
		return errResult(404, "Not Found")
	}

	switch rest[0] {
	case "branches":
		if len(rest) == 1 {
			if method != "GET" {
				return errResult(404, "Not Found")
			}
			branches := forge.Branches(repo)
			var b []byte
			b = append(b, '[')
			for i, br := range branches {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"name":`...)
				b = jsonQuote(b, br.Name)
				b = append(b, `,"commit":{"sha":`...)
				b = jsonQuote(b, br.Sha)
				b = append(b, "}}"...)
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b}
		}
		branch := strings.Join(rest[1:], "/")
		sha := forge.BranchTip(repo, branch)
		if sha == "" {
			return errResult(404, "Not Found")
		}
		if method == "GET" {
			var b []byte
			b = append(b, `{"name":`...)
			b = jsonQuote(b, branch)
			b = append(b, `,"commit":{"sha":`...)
			b = jsonQuote(b, sha)
			b = append(b, "}}"...)
			return RestResult{Status: 200, Body: b}
		}
		return errResult(404, "Not Found")

	case "commits":
		if method != "GET" {
			return errResult(404, "Not Found")
		}
		ref := ctx.params().getOr("sha", repo.DefaultBranch)
		var tip string
		if IsOID(ref) {
			if repo.Objects[ref] != nil {
				tip = ref
			}
		} else {
			tip = forge.BranchTip(repo, ref)
		}
		if tip == "" {
			return errResult(404, "Not Found")
		}
		commits := forge.CommitsFrom(repo, tip)
		var b []byte
		b = append(b, '[')
		for i, oid := range commits {
			if i > 0 {
				b = append(b, ',')
			}
			c := ParseCommit(repo.Objects[oid].Data)
			b = append(b, `{"sha":`...)
			b = jsonQuote(b, oid)
			b = append(b, `,"commit":{"message":`...)
			b = jsonQuote(b, c.Message)
			b = append(b, `,"author":{},"committer":{},"tree":{"sha":`...)
			b = jsonQuote(b, c.Tree)
			b = append(b, "}}}"...)
		}
		b = append(b, ']')
		return RestResult{Status: 200, Body: b}

	case "git":
		if len(rest) >= 3 && rest[1] == "refs" && method == "GET" {
			ref := "refs/" + strings.Join(rest[2:], "/")
			sha, ok := repo.Refs[ref]
			if !ok {
				return errResult(404, "Not Found")
			}
			var b []byte
			b = append(b, `{"ref":`...)
			b = jsonQuote(b, ref)
			b = append(b, `,"object":{"sha":`...)
			b = jsonQuote(b, sha)
			b = append(b, `,"type":`...)
			kind := "commit"
			if obj := repo.Objects[sha]; obj != nil {
				kind = obj.Kind.name()
			}
			b = jsonQuote(b, kind)
			b = append(b, `}}`...)
			return RestResult{Status: 200, Body: b}
		}
		if len(rest) == 2 && rest[1] == "refs" && method == "POST" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			ref, okRef := parsed.str("ref")
			sha, okSha := parsed.str("sha")
			if !okRef || !okSha || !IsOID(sha) {
				return errResult(422, "ref and sha are required")
			}
			if _, exists := repo.Refs[ref]; exists {
				return errResult(422, "Reference already exists")
			}
			if repo.Objects[sha] == nil {
				return errResult(422, "Object does not exist")
			}
			forge.SetRef(repo, ref, sha)
			var b []byte
			b = append(b, `{"ref":`...)
			b = jsonQuote(b, ref)
			b = append(b, `,"object":{"sha":`...)
			b = jsonQuote(b, sha)
			b = append(b, `,"type":`...)
			b = jsonQuote(b, repo.Objects[sha].Kind.name())
			b = append(b, `}}`...)
			return RestResult{Status: 201, Body: b}
		}
		return errResult(404, "Not Found")

	case "contents":
		return handleContents(forge, repo, ctx, strings.Join(rest[1:], "/"))

	case "labels":
		if method == "GET" {
			var b []byte
			b = append(b, '[')
			for i, name := range repo.LabelsOrder {
				if i > 0 {
					b = append(b, ',')
				}
				l := repo.Labels[name]
				b = append(b, `{"name":`...)
				b = jsonQuote(b, l.Name)
				b = append(b, `,"color":`...)
				b = jsonQuote(b, l.Color)
				b = append(b, '}')
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b}
		}
		if method == "POST" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			name, ok := parsed.str("name")
			if !ok || name == "" {
				return errResult(422, "name is required")
			}
			label := forge.CreateLabel(repo, name, parsed.strOr("color", "000000"))
			var b []byte
			b = append(b, `{"name":`...)
			b = jsonQuote(b, label.Name)
			b = append(b, `,"color":`...)
			b = jsonQuote(b, label.Color)
			b = append(b, '}')
			return RestResult{Status: 201, Body: b}
		}
		return errResult(404, "Not Found")

	case "issues":
		return handleIssues(forge, repo, ctx, rest[1:])

	case "pulls":
		return handlePulls(forge, repo, ctx, rest[1:])
	}
	return errResult(404, "Not Found")
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

func resolveRefTip(forge *Forge, repo *Repo, ref string, hasRef bool) string {
	if !hasRef {
		return forge.BranchTip(repo, repo.DefaultBranch)
	}
	if IsOID(ref) {
		if repo.Objects[ref] != nil {
			return ref
		}
		return ""
	}
	return forge.BranchTip(repo, ref)
}

func handleContents(forge *Forge, repo *Repo, ctx *RestCtx, path string) RestResult {
	method := ctx.Method
	if method == "GET" {
		q := ctx.params()
		ref, hasRef := q.byKey["ref"]
		tip := resolveRefTip(forge, repo, ref, hasRef)
		if tip == "" {
			return errResult(404, "Not Found")
		}
		commit := ParseCommit(repo.Objects[tip].Data)
		if path == "" {
			root := forge.ReadPath(repo, commit.Tree, "")
			if !root.Found || !root.IsDir {
				return errResult(404, "Not Found")
			}
			return RestResult{Status: 200, Body: dirListingJSON(root.Entries, "")}
		}
		found := forge.ReadPath(repo, commit.Tree, path)
		if !found.Found {
			return errResult(404, "Not Found")
		}
		if found.IsDir {
			return RestResult{Status: 200, Body: dirListingJSON(found.Entries, path)}
		}
		blob := repo.Objects[found.OID]
		if blob == nil {
			return errResult(404, "Not Found")
		}
		var b []byte
		b = append(b, `{"type":"file","encoding":"base64","size":`...)
		b = itoa(b, len(blob.Data))
		b = append(b, `,"name":`...)
		b = jsonQuote(b, baseName(path))
		b = append(b, `,"path":`...)
		b = jsonQuote(b, path)
		b = append(b, `,"content":`...)
		b = jsonQuote(b, base64.StdEncoding.EncodeToString(blob.Data))
		b = append(b, `,"sha":`...)
		b = jsonQuote(b, found.OID)
		b = append(b, '}')
		return RestResult{Status: 200, Body: b}
	}

	if method == "PUT" {
		parsed, bad := parseRestBody(ctx.Body)
		if bad != nil {
			return *bad
		}
		message := parsed.strOr("message", "Update file")
		contentB64, ok := parsed.str("content")
		if !ok {
			return errResult(422, "content is required")
		}
		bytes, err := base64.StdEncoding.DecodeString(contentB64)
		if err != nil {
			return errResult(422, "content is not valid base64")
		}
		branch := parsed.strOr("branch", repo.DefaultBranch)
		sha, hasSha := parsed.str("sha")
		_ = hasSha
		defaultTip := forge.BranchTip(repo, repo.DefaultBranch)
		branchTip := forge.BranchTip(repo, branch)
		// A missing branch is created from a commit whose parent is the
		// default tip.
		var baseTree string
		var parents []string
		if branchTip == "" {
			if defaultTip != "" {
				baseTree = ParseCommit(repo.Objects[defaultTip].Data).Tree
				parents = []string{defaultTip}
			}
		} else {
			baseTree = ParseCommit(repo.Objects[branchTip].Data).Tree
			parents = []string{branchTip}
		}
		var existing ReadPathResult
		if baseTree != "" {
			existing = forge.ReadPath(repo, baseTree, path)
		}
		if existing.Found && !existing.IsDir {
			if !hasSha || sha != existing.OID {
				return errResult(409, "sha does not match the current file")
			}
		}
		blobOID := forge.PutObject(repo, KindBlob, bytes)
		tree := forge.SetPath(repo, baseTree, path, blobOID)
		commitOID := forge.MakeCommit(repo, tree, parents, message)
		forge.SetRef(repo, "refs/heads/"+branch, commitOID)
		status := 201
		if existing.Found && !existing.IsDir {
			status = 200
		}
		var b []byte
		b = append(b, `{"content":{"name":`...)
		b = jsonQuote(b, baseName(path))
		b = append(b, `,"path":`...)
		b = jsonQuote(b, path)
		b = append(b, `,"sha":`...)
		b = jsonQuote(b, blobOID)
		b = append(b, `,"size":`...)
		b = itoa(b, len(bytes))
		b = append(b, `,"type":"file","encoding":"base64","content":`...)
		b = jsonQuote(b, base64.StdEncoding.EncodeToString(bytes))
		b = append(b, `},"commit":{"sha":`...)
		b = jsonQuote(b, commitOID)
		b = append(b, `,"message":`...)
		b = jsonQuote(b, message)
		b = append(b, "}}"...)
		return RestResult{Status: status, Body: b}
	}

	if method == "DELETE" {
		parsed, bad := parseRestBody(ctx.Body)
		if bad != nil {
			return *bad
		}
		message := parsed.strOr("message", "Delete file")
		branch := parsed.strOr("branch", repo.DefaultBranch)
		sha, hasSha := parsed.str("sha")
		tip := forge.BranchTip(repo, branch)
		if tip == "" {
			return errResult(404, "Not Found")
		}
		tree := ParseCommit(repo.Objects[tip].Data).Tree
		existing := forge.ReadPath(repo, tree, path)
		if !existing.Found || existing.IsDir {
			return errResult(404, "Not Found")
		}
		if hasSha && sha != existing.OID {
			return errResult(409, "sha does not match the current file")
		}
		nextTree := forge.SetPath(repo, tree, path, "")
		commitOID := forge.MakeCommit(repo, nextTree, []string{tip}, message)
		forge.SetRef(repo, "refs/heads/"+branch, commitOID)
		var b []byte
		b = append(b, `{"commit":{"sha":`...)
		b = jsonQuote(b, commitOID)
		b = append(b, `,"message":`...)
		b = jsonQuote(b, message)
		b = append(b, "}}"...)
		return RestResult{Status: 200, Body: b}
	}

	return errResult(404, "Not Found")
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func dirListingJSON(entries []TreeEntry, prefix string) []byte {
	var b []byte
	b = append(b, '[')
	for i, e := range entries {
		if i > 0 {
			b = append(b, ',')
		}
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		b = append(b, `{"type":`...)
		b = jsonQuote(b, typ)
		b = append(b, `,"name":`...)
		b = jsonQuote(b, e.Name)
		b = append(b, `,"path":`...)
		b = jsonQuote(b, p)
		b = append(b, `,"sha":`...)
		b = jsonQuote(b, e.OID)
		b = append(b, '}')
	}
	b = append(b, ']')
	return b
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

func handleIssues(forge *Forge, repo *Repo, ctx *RestCtx, rest []string) RestResult {
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			q := ctx.params()
			state := q.getOr("state", "open")
			direction := q.getOr("direction", "desc")
			var list []*Issue
			for _, issue := range repo.Issues {
				if state == "all" || issue.State == state {
					list = append(list, issue)
				}
			}
			sort.Slice(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].CreatedSeq < list[j].CreatedSeq
				}
				return list[i].CreatedSeq > list[j].CreatedSeq
			})
			start, end, link := paginate(len(list), ctx)
			var b []byte
			b = append(b, '[')
			for i := start; i < end; i++ {
				if i > start {
					b = append(b, ',')
				}
				b = append(b, IssueJSONCached(repo, list[i])...)
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b, Headers: linkHeader(link)}
		}
		if method == "POST" {
			return CreateIssueReply(forge, repo, ctx.Body)
		}
		return errResult(404, "Not Found")
	}

	number, ok := atoiDigits(rest[0])
	if !ok {
		return errResult(404, "Not Found")
	}
	issue := forge.GetIssue(repo, number)
	if issue == nil {
		return errResult(404, "Not Found")
	}

	if len(rest) == 1 {
		if method == "GET" {
			// The structured form: the router's fast path serves the pooled
			// pre-built response directly; this generic dispatch can only
			// return a body for the transport to wrap.
			return RestResult{Status: 200, Body: IssueBuf(repo, issue)}
		}
		if method == "PATCH" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			if title, ok := parsed.str("title"); ok {
				issue.Title = title
			}
			if body, ok := parsed.str("body"); ok {
				issue.Body = body
			}
			if state, ok := parsed.str("state"); ok && (state == "open" || state == "closed") {
				issue.State = state
			}
			InvalidateIssue(issue)
			return RestResult{Status: 200, Body: IssueBuf(repo, issue)}
		}
		return errResult(404, "Not Found")
	}

	if rest[1] == "comments" {
		if method == "GET" {
			var b []byte
			b = append(b, '[')
			for i, c := range issue.Comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, commentJSON(repo, issue, c)...)
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b}
		}
		if method == "POST" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			body, ok := parsed.str("body")
			if !ok || body == "" {
				return errResult(422, "body is required")
			}
			comment := forge.CreateComment(repo, issue, body)
			return RestResult{Status: 201, Body: []byte(commentJSON(repo, issue, comment))}
		}
		return errResult(404, "Not Found")
	}

	return errResult(404, "Not Found")
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

func handlePulls(forge *Forge, repo *Repo, ctx *RestCtx, rest []string) RestResult {
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			state := ctx.params().getOr("state", "open")
			var list []*Pull
			for _, p := range repo.Pulls {
				if state == "all" || p.State == state {
					list = append(list, p)
				}
			}
			start, end, link := paginate(len(list), ctx)
			var b []byte
			b = append(b, '[')
			for i := start; i < end; i++ {
				if i > start {
					b = append(b, ',')
				}
				b = append(b, pullJSON(repo, list[i])...)
			}
			b = append(b, ']')
			return RestResult{Status: 200, Body: b, Headers: linkHeader(link)}
		}
		if method == "POST" {
			parsed, bad := parseRestBody(ctx.Body)
			if bad != nil {
				return *bad
			}
			title, ok := parsed.str("title")
			if !ok || title == "" {
				return errResult(422, "title is required")
			}
			head, okHead := parsed.str("head")
			base, okBase := parsed.str("base")
			if !okHead || !okBase {
				return errResult(422, "head and base are required")
			}
			headRef := stripOwnerPrefix(head)
			baseRef := stripOwnerPrefix(base)
			created, errMsg := forge.CreatePull(repo, title, parsed.strOr("body", ""), headRef, baseRef)
			if errMsg != "" {
				return errResult(422, errMsg)
			}
			return RestResult{Status: 201, Body: []byte(pullJSON(repo, created))}
		}
		return errResult(404, "Not Found")
	}

	number, ok := atoiDigits(rest[0])
	if !ok {
		return errResult(404, "Not Found")
	}
	pull := forge.GetPull(repo, number)
	if pull == nil {
		return errResult(404, "Not Found")
	}

	if len(rest) == 1 {
		if method == "GET" {
			return RestResult{Status: 200, Body: []byte(pullJSON(repo, pull))}
		}
		return errResult(404, "Not Found")
	}

	if rest[1] == "files" {
		if method != "GET" {
			return errResult(404, "Not Found")
		}
		files := forge.PullFiles(repo, pull)
		var b []byte
		b = append(b, '[')
		for i, f := range files {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"filename":`...)
			b = jsonQuote(b, f.Path)
			b = append(b, `,"status":`...)
			b = jsonQuote(b, f.Status)
			b = append(b, `,"additions":`...)
			b = itoa(b, f.Additions)
			b = append(b, `,"deletions":`...)
			b = itoa(b, f.Deletions)
			b = append(b, `,"changes":`...)
			b = itoa(b, f.Changes)
			b = append(b, '}')
		}
		b = append(b, ']')
		return RestResult{Status: 200, Body: b}
	}

	if rest[1] == "merge" {
		if method != "PUT" && method != "POST" {
			return errResult(404, "Not Found")
		}
		_, bad := parseRestBody(ctx.Body)
		if bad != nil {
			return *bad
		}
		if pull.State != "open" {
			return errResult(405, "Pull request is not open")
		}
		if forge.PullConflicts(repo, pull) {
			return errResult(405, "Merge conflict")
		}
		sha := forge.MergePull(repo, pull)
		if sha == "" {
			return errResult(405, "Merge conflict")
		}
		var b []byte
		b = append(b, `{"merged":true,"sha":`...)
		b = jsonQuote(b, sha)
		b = append(b, `,"message":`...)
		b = jsonQuote(b, "Merge pull request #"+strconv.Itoa(pull.Number)+" from "+pull.HeadRef)
		b = append(b, '}')
		return RestResult{Status: 200, Body: b}
	}

	return errResult(404, "Not Found")
}

// stripOwnerPrefix turns "owner:branch" into "branch" (the first colon wins).
func stripOwnerPrefix(ref string) string {
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
