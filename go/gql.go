// The GraphQL v4 surface (/api/graphql): a hand-written lexer,
// recursive-descent parser and executor for the documents the suite sends,
// over the same forge store as REST. No dependency on any GraphQL library —
// the surface is small (a few dozen fields) and the suite's contract is
// pinned by the checks:
//
//   - POST {query, variables} or a JSON array (batching -> responses in order).
//   - A GraphQL-level failure (an unknown field) is HTTP 200 with an errors
//     array; the message names the field, e.g.
//     `Cannot query field "noSuchFieldOnViewer" on type "User".`
//   - data.repository is null with NO errors for an unknown repository.
//   - Global ids are opaque and stable: base64 of `Repository:owner/name`,
//     `Issue:owner/name:number`, `PullRequest:owner/name:number`.
//   - Issue state OPEN/CLOSED, pull-request state OPEN/CLOSED/MERGED,
//     mergeable MERGEABLE/CONFLICTING/UNKNOWN (the same conflict rule as the
//     REST merge).
//   - Issues pagination: first + after (an opaque base64 `cursor:N`),
//     pageInfo {hasNextPage, endCursor}; CREATED_AT ordering is the per-repo
//     creation sequence.
//   - Mutations are the same operations as REST (create/close issue, create/
//     merge pull request, add comment) and their effects are visible through
//     REST afterwards. Directives (@skip/@include) are parsed and ignored.
//
// Layout: the schema is one merged table per type (fields in introspection
// order plus each field's return type), from which the field sets and return
// lookups are derived. Resolvers are one switch function per type; the
// executor dispatches on the runtime object's type name.

package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// schema: one merged definition per type
// ---------------------------------------------------------------------------------------------

type typeDef struct {
	fields  []string
	returns map[string]string
}

var typeDefs = map[string]typeDef{
	"Query": {
		fields:  []string{"viewer", "repository", "node", "__type", "__schema"},
		returns: map[string]string{"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type", "__schema": "__Schema"},
	},
	"Mutation": {
		fields:  []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
		returns: map[string]string{"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload", "createPullRequest": "CreatePullRequestPayload", "mergePullRequest": "MergePullRequestPayload", "addComment": "AddCommentPayload"},
	},
	"Repository": {
		fields:  []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "url"},
		returns: map[string]string{"id": "ID", "databaseId": "Int", "name": "String", "nameWithOwner": "String", "owner": "User", "defaultBranchRef": "Ref", "viewerPermission": "String", "hasIssuesEnabled": "Boolean", "issue": "Issue", "issues": "IssueConnection", "pullRequests": "PullRequestConnection", "pullRequest": "PullRequest", "description": "String", "isPrivate": "Boolean", "createdAt": "String", "url": "String"},
	},
	"Issue": {
		fields:  []string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "url", "author", "repository", "comments", "labels"},
		returns: map[string]string{"id": "ID", "number": "Int", "title": "String", "body": "String", "state": "IssueState", "createdAt": "String", "updatedAt": "String", "url": "String", "author": "User", "repository": "Repository", "comments": "IssueCommentConnection", "labels": "Label"},
	},
	"PullRequest": {
		fields:  []string{"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "headRef", "baseRef", "mergedAt", "mergeCommit", "createdAt", "url", "author", "repository"},
		returns: map[string]string{"id": "ID", "number": "Int", "title": "String", "body": "String", "state": "PullRequestState", "merged": "Boolean", "headRefName": "String", "baseRefName": "String", "mergeable": "MergeableState", "files": "PullRequestChangedFileConnection", "headRef": "Ref", "baseRef": "Ref", "mergedAt": "String", "mergeCommit": "Commit", "createdAt": "String", "url": "String", "author": "User", "repository": "Repository"},
	},
	"User": {
		fields:  []string{"login", "id", "name", "email"},
		returns: map[string]string{"login": "String", "id": "ID", "name": "String", "email": "String"},
	},
	"IssueConnection": {
		fields:  []string{"nodes", "pageInfo", "totalCount"},
		returns: map[string]string{"nodes": "Issue", "pageInfo": "PageInfo", "totalCount": "Int"},
	},
	"PullRequestConnection": {
		fields:  []string{"nodes", "pageInfo", "totalCount"},
		returns: map[string]string{"nodes": "PullRequest", "pageInfo": "PageInfo", "totalCount": "Int"},
	},
	"IssueCommentConnection": {
		fields:  []string{"nodes", "pageInfo", "totalCount"},
		returns: map[string]string{"nodes": "IssueComment", "pageInfo": "PageInfo", "totalCount": "Int"},
	},
	"PullRequestChangedFileConnection": {
		fields:  []string{"nodes", "pageInfo", "totalCount"},
		returns: map[string]string{"nodes": "PullRequestChangedFile", "pageInfo": "PageInfo", "totalCount": "Int"},
	},
	"PageInfo": {
		fields:  []string{"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
		returns: map[string]string{"hasNextPage": "Boolean", "endCursor": "String", "hasPreviousPage": "Boolean", "startCursor": "String"},
	},
	"Ref": {
		fields:  []string{"name", "target"},
		returns: map[string]string{"name": "String", "target": "Commit"},
	},
	"Commit": {
		fields:  []string{"oid", "message"},
		returns: map[string]string{"oid": "String", "message": "String"},
	},
	"IssueComment": {
		fields:  []string{"id", "body", "author", "createdAt"},
		returns: map[string]string{"id": "ID", "body": "String", "author": "User", "createdAt": "String"},
	},
	"PullRequestChangedFile": {
		fields:  []string{"path"},
		returns: map[string]string{"path": "String"},
	},
	"Label": {
		fields:  []string{"name", "color"},
		returns: map[string]string{"name": "String", "color": "String"},
	},
	"CreateIssuePayload":        {fields: []string{"issue"}, returns: map[string]string{"issue": "Issue"}},
	"CloseIssuePayload":         {fields: []string{"issue"}, returns: map[string]string{"issue": "Issue"}},
	"CreatePullRequestPayload":  {fields: []string{"pullRequest"}, returns: map[string]string{"pullRequest": "PullRequest"}},
	"MergePullRequestPayload":   {fields: []string{"pullRequest"}, returns: map[string]string{"pullRequest": "PullRequest"}},
	"AddCommentPayload":         {fields: []string{"commentEdge"}, returns: map[string]string{"commentEdge": "CommentEdge"}},
	"CommentEdge":               {fields: []string{"node"}, returns: map[string]string{"node": "IssueComment"}},
	"__Type": {
		fields:  []string{"name", "kind", "fields", "enumValues"},
		returns: map[string]string{"name": "String", "kind": "String", "fields": "__Field", "enumValues": "__EnumValue"},
	},
	"__Field":     {fields: []string{"name"}, returns: map[string]string{"name": "String"}},
	"__EnumValue": {fields: []string{"name"}, returns: map[string]string{"name": "String"}},
	"__Schema": {
		fields:  []string{"types", "queryType", "mutationType"},
		returns: map[string]string{"types": "__Type", "queryType": "__Type", "mutationType": "__Type"},
	},
	// The Node interface: a node is always addressed by its id, and a query
	// may read that id directly before narrowing with an inline fragment.
	"Node": {fields: []string{"id"}, returns: map[string]string{"id": "ID"}},
}

var enumValues = map[string][]string{
	"SearchType":       {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":   {"ASC", "DESC"},
	"IssueOrderField":  {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
}

// fieldSets is the per-type field set for O(1) validation lookups.
var fieldSets = func() map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(typeDefs))
	for name, def := range typeDefs {
		set := make(map[string]bool, len(def.fields))
		for _, f := range def.fields {
			set[f] = true
		}
		out[name] = set
	}
	return out
}()

// ---------------------------------------------------------------------------------------------
// lexer (streaming: one token of lookahead, no full token array)
// ---------------------------------------------------------------------------------------------

type tokKind int

const (
	tokName tokKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
	tokEOF
)

type gqlTok struct {
	kind  tokKind
	value string
}

var gqlEOF = gqlTok{kind: tokEOF}

const gqlPuncts = "!$():=@[]{}|&"

func gqlIsDigit(c byte) bool { return c >= '0' && c <= '9' }

func gqlIsNameStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func gqlIsNameChar(c byte) bool { return gqlIsNameStart(c) || gqlIsDigit(c) }

type gqlLexer struct {
	src      string
	pos      int
	buffered *gqlTok
}

func (l *gqlLexer) peek() gqlTok {
	if l.buffered == nil {
		t := l.scan()
		l.buffered = &t
	}
	return *l.buffered
}

func (l *gqlLexer) next() gqlTok {
	t := l.peek()
	l.buffered = nil
	return t
}

func (l *gqlLexer) scan() gqlTok {
	src := l.src
	i := l.pos
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		// BOM (U+FEFF) is ignorable whitespace.
		if c == 0xEF && i+2 < len(src) && src[i+1] == 0xBB && src[i+2] == 0xBF {
			i += 3
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	if i >= len(src) {
		l.pos = i
		return gqlEOF
	}
	c := src[i]
	if c == '.' && strings.HasPrefix(src[i:], "...") {
		l.pos = i + 3
		return gqlTok{kind: tokPunct, value: "..."}
	}
	if strings.IndexByte(gqlPuncts, c) >= 0 {
		l.pos = i + 1
		return gqlTok{kind: tokPunct, value: string(c)}
	}
	if c == '"' {
		var sb strings.Builder
		i++
		for i < len(src) && src[i] != '"' {
			if src[i] == '\\' && i+1 < len(src) {
				esc := src[i+1]
				if esc == 'u' {
					if i+5 >= len(src) {
						panic("incomplete unicode escape in string")
					}
					n, err := strconv.ParseInt(src[i+2:i+6], 16, 32)
					if err != nil {
						panic("invalid unicode escape in string")
					}
					sb.WriteRune(rune(n))
					i += 6
				} else {
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						panic(fmt.Sprintf("invalid escape sequence \\%c in string", esc))
					}
					i += 2
				}
			} else {
				sb.WriteByte(src[i])
				i++
			}
		}
		l.pos = i + 1
		return gqlTok{kind: tokString, value: sb.String()}
	}
	if gqlIsDigit(c) || c == '-' {
		j := i
		if src[j] == '-' {
			j++
		}
		isFloat := false
		for j < len(src) && gqlIsDigit(src[j]) {
			j++
		}
		if j < len(src) && src[j] == '.' {
			isFloat = true
			j++
			for j < len(src) && gqlIsDigit(src[j]) {
				j++
			}
		}
		if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
			isFloat = true
			j++
			if j < len(src) && (src[j] == '+' || src[j] == '-') {
				j++
			}
			for j < len(src) && gqlIsDigit(src[j]) {
				j++
			}
		}
		l.pos = j
		if isFloat {
			return gqlTok{kind: tokFloat, value: src[i:j]}
		}
		return gqlTok{kind: tokInt, value: src[i:j]}
	}
	if gqlIsNameStart(c) {
		j := i
		for j < len(src) && gqlIsNameChar(src[j]) {
			j++
		}
		l.pos = j
		return gqlTok{kind: tokName, value: src[i:j]}
	}
	panic(fmt.Sprintf("Unexpected character %q at offset %d", c, i))
}

// ---------------------------------------------------------------------------------------------
// parser (recursive descent over the lexer)
// ---------------------------------------------------------------------------------------------

type gqlValueKind int

const (
	valVariable gqlValueKind = iota
	valString
	valInt
	valFloat
	valBoolean
	valNull
	valEnum
	valList
	valObject
)

type gqlValue struct {
	kind   gqlValueKind
	str    string
	num    float64
	b      bool
	list   []*gqlValue
	fields []gqlObjField
}

type gqlObjField struct {
	name  string
	value *gqlValue
}

type gqlSelKind int

const (
	selField gqlSelKind = iota
	selSpread
	selInline
)

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	kind          gqlSelKind
	name          string // field name, or fragment name for spreads
	alias         string
	args          []gqlArg
	selections    []*gqlSelection
	typeCondition string
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValue
}

type gqlOperation struct {
	kind      string // "query" | "mutation" | "subscription"
	name      string
	variables []gqlVarDef
	selections []*gqlSelection
}

type gqlFragment struct {
	name          string
	typeCondition string
	selections    []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	lexer *gqlLexer
	depth int // current selection-set nesting (bounded by maxGqlDepth)
}

// maxGqlDepth caps query nesting: the parse/validate/execute walks are
// recursive, and an unbounded nesting level would grow the goroutine stack
// with the query. The suite's queries nest a handful of levels.
const maxGqlDepth = 100

func (p *gqlParser) peek() gqlTok  { return p.lexer.peek() }
func (p *gqlParser) next() gqlTok  { return p.lexer.next() }

func (p *gqlParser) expectPunct(value string) {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		panic(fmt.Sprintf("Expected %s, got %s", value, t.value))
	}
}

func (p *gqlParser) expectName() string {
	t := p.next()
	if t.kind != tokName {
		panic(fmt.Sprintf("Expected a name, got %s", t.value))
	}
	return t.value
}

func (p *gqlParser) parseDocument() *gqlDocument {
	doc := &gqlDocument{fragments: make(map[string]*gqlFragment)}
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			doc.operations = append(doc.operations, &gqlOperation{kind: "query", selections: p.parseSelectionSet()})
		} else if t.kind == tokName && t.value == "fragment" {
			p.next()
			name := p.expectName()
			p.expectName() // 'on'
			cond := p.expectName()
			p.parseDirectives()
			doc.fragments[name] = &gqlFragment{name: name, typeCondition: cond, selections: p.parseSelectionSet()}
		} else if t.kind == tokName && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
			kind := p.next().value
			op := &gqlOperation{kind: kind}
			if p.peek().kind == tokName {
				op.name = p.next().value
			}
			if p.peek().kind == tokPunct && p.peek().value == "(" {
				op.variables = p.parseVariableDefinitions()
			}
			p.parseDirectives()
			op.selections = p.parseSelectionSet()
			doc.operations = append(doc.operations, op)
		} else {
			panic(fmt.Sprintf("Unexpected token %s", t.value))
		}
	}
	return doc
}

func (p *gqlParser) parseVariableDefinitions() []gqlVarDef {
	p.expectPunct("(")
	var defs []gqlVarDef
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		p.expectPunct("$")
		name := p.expectName()
		p.expectPunct(":")
		p.parseType()
		def := gqlVarDef{name: name}
		if p.peek().kind == tokPunct && p.peek().value == "=" {
			p.next()
			def.defaultValue = p.parseValue()
		}
		defs = append(defs, def)
	}
	p.expectPunct(")")
	return defs
}

func (p *gqlParser) parseType() {
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.next()
		p.parseType()
		p.expectPunct("]")
	} else {
		p.expectName()
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
	}
}

func (p *gqlParser) parseSelectionSet() []*gqlSelection {
	p.depth++
	if p.depth > maxGqlDepth {
		panic("query is nested too deeply")
	}
	defer func() { p.depth-- }()
	p.expectPunct("{")
	var sels []*gqlSelection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		sels = append(sels, p.parseSelection())
	}
	p.expectPunct("}")
	return sels
}

func (p *gqlParser) parseSelection() *gqlSelection {
	if p.peek().kind == tokPunct && p.peek().value == "..." {
		p.next()
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			cond := p.expectName()
			p.parseDirectives()
			return &gqlSelection{kind: selInline, typeCondition: cond, selections: p.parseSelectionSet()}
		}
		if p.peek().kind == tokName {
			name := p.next().value
			p.parseDirectives()
			return &gqlSelection{kind: selSpread, name: name}
		}
		p.parseDirectives()
		return &gqlSelection{kind: selInline, selections: p.parseSelectionSet()}
	}
	name := p.expectName()
	sel := &gqlSelection{kind: selField, name: name}
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.next()
		sel.alias = name
		sel.name = p.expectName()
	}
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		sel.args = p.parseArguments()
	}
	p.parseDirectives()
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		sel.selections = p.parseSelectionSet()
	}
	return sel
}

func (p *gqlParser) parseArguments() []gqlArg {
	p.expectPunct("(")
	var args []gqlArg
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		name := p.expectName()
		p.expectPunct(":")
		args = append(args, gqlArg{name: name, value: p.parseValue()})
	}
	p.expectPunct(")")
	return args
}

// parseDirectives parses directives and discards them: the suite never
// relies on @skip/@include.
func (p *gqlParser) parseDirectives() {
	for p.peek().kind == tokPunct && p.peek().value == "@" {
		p.next()
		p.expectName()
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.parseArguments()
		}
	}
}

func (p *gqlParser) parseValue() *gqlValue {
	t := p.peek()
	if t.kind == tokPunct && t.value == "$" {
		p.next()
		return &gqlValue{kind: valVariable, str: p.expectName()}
	}
	if t.kind == tokPunct && t.value == "[" {
		p.next()
		v := &gqlValue{kind: valList}
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			v.list = append(v.list, p.parseValue())
		}
		p.expectPunct("]")
		return v
	}
	if t.kind == tokPunct && t.value == "{" {
		p.next()
		v := &gqlValue{kind: valObject}
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name := p.expectName()
			p.expectPunct(":")
			v.fields = append(v.fields, gqlObjField{name: name, value: p.parseValue()})
		}
		p.expectPunct("}")
		return v
	}
	if t.kind == tokString {
		p.next()
		return &gqlValue{kind: valString, str: t.value}
	}
	if t.kind == tokInt {
		p.next()
		n, _ := strconv.ParseFloat(t.value, 64)
		return &gqlValue{kind: valInt, num: n}
	}
	if t.kind == tokFloat {
		p.next()
		n, _ := strconv.ParseFloat(t.value, 64)
		return &gqlValue{kind: valFloat, num: n}
	}
	if t.kind == tokName {
		p.next()
		switch t.value {
		case "true":
			return &gqlValue{kind: valBoolean, b: true}
		case "false":
			return &gqlValue{kind: valBoolean, b: false}
		case "null":
			return &gqlValue{kind: valNull}
		default:
			return &gqlValue{kind: valEnum, str: t.value}
		}
	}
	panic(fmt.Sprintf("Unexpected value token %s", t.value))
}

func evalGqlValue(node *gqlValue, variables map[string]any) any {
	switch node.kind {
	case valVariable:
		if v, ok := variables[node.str]; ok {
			return v
		}
		return nil
	case valList:
		out := make([]any, len(node.list))
		for i, child := range node.list {
			out[i] = evalGqlValue(child, variables)
		}
		return out
	case valObject:
		out := make(map[string]any, len(node.fields))
		for _, f := range node.fields {
			out[f.name] = evalGqlValue(f.value, variables)
		}
		return out
	case valString, valEnum:
		return node.str
	case valInt, valFloat:
		return node.num
	case valBoolean:
		return node.b
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------------------------
// validation: an unknown field fails the whole operation with a named error
// ---------------------------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

func validateSelections(typeName string, selections []*gqlSelection, fragments map[string]*gqlFragment, errors *[]gqlError, visited map[string]bool) {
	for _, sel := range selections {
		switch sel.kind {
		case selField:
			name := sel.name
			if name == "__typename" {
				continue
			}
			if name == "__type" || name == "__schema" {
				if typeName != "Query" {
					*errors = append(*errors, gqlError{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, typeName)})
					continue
				}
				if len(sel.selections) > 0 {
					next := "__Type"
					if name == "__schema" {
						next = "__Schema"
					}
					validateSelections(next, sel.selections, fragments, errors, visited)
				}
				continue
			}
			if !fieldSets[typeName][name] {
				*errors = append(*errors, gqlError{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, typeName)})
				continue
			}
			if len(sel.selections) > 0 {
				if returnType, ok := typeDefs[typeName].returns[name]; ok {
					validateSelections(returnType, sel.selections, fragments, errors, visited)
				}
			}
		case selInline:
			cond := sel.typeCondition
			if cond == "" {
				cond = typeName
			}
			validateSelections(cond, sel.selections, fragments, errors, visited)
		case selSpread:
			frag, ok := fragments[sel.name]
			if !ok {
				*errors = append(*errors, gqlError{Message: fmt.Sprintf("Unknown fragment %q.", sel.name)})
				continue
			}
			if visited[frag.name] {
				continue
			}
			visited[frag.name] = true
			validateSelections(frag.typeCondition, frag.selections, fragments, errors, visited)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// runtime values
// ---------------------------------------------------------------------------------------------

// gqlObj is a runtime GraphQL object: its type name plus its fields. Domain
// pointers ride under the underscore keys; scalar fields are read straight
// off the map by the passthrough resolvers.
type gqlObj struct {
	typ    string
	fields map[string]any
}

func newGqlObj(typ string) *gqlObj {
	return &gqlObj{typ: typ, fields: make(map[string]any)}
}

func (o *gqlObj) repo() *Repo {
	if r, ok := o.fields["_repo"].(*Repo); ok {
		return r
	}
	return nil
}

func (o *gqlObj) issue() *Issue {
	if i, ok := o.fields["_issue"].(*Issue); ok {
		return i
	}
	return nil
}

func (o *gqlObj) pull() *Pull {
	if p, ok := o.fields["_pull"].(*Pull); ok {
		return p
	}
	return nil
}

func repoNode(repo *Repo) *gqlObj {
	o := newGqlObj("Repository")
	o.fields["_repo"] = repo
	return o
}

func issueNode(repo *Repo, issue *Issue) *gqlObj {
	o := newGqlObj("Issue")
	o.fields["_repo"] = repo
	o.fields["_issue"] = issue
	return o
}

func pullNode(repo *Repo, pull *Pull) *gqlObj {
	o := newGqlObj("PullRequest")
	o.fields["_repo"] = repo
	o.fields["_pull"] = pull
	return o
}

func userNode(login string) *gqlObj {
	o := newGqlObj("User")
	o.fields["login"] = login
	return o
}

func cursorFor(index int) string {
	return b64Encode("cursor:" + strconv.Itoa(index))
}

// indexAfterCursor is the first index after a `cursor:N` (0 for a missing
// or malformed cursor).
func indexAfterCursor(cursor any, total int) int {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0
	}
	text, err := b64Decode(s)
	if err != nil || !strings.HasPrefix(text, "cursor:") {
		return 0
	}
	digits := text[len("cursor:"):]
	if digits == "" {
		return 0
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0
		}
	}
	n, _ := strconv.Atoi(digits)
	if n+1 > total {
		return total
	}
	return n + 1
}

func pageInfoFor(count, total int, first *int, start int) *gqlObj {
	o := newGqlObj("PageInfo")
	hasNext := first != nil && start+count < total
	var endCursor, startCursor any
	if count > 0 {
		endCursor = cursorFor(start + count - 1)
		startCursor = cursorFor(start)
	}
	o.fields["hasNextPage"] = hasNext
	o.fields["endCursor"] = endCursor
	o.fields["hasPreviousPage"] = start > 0
	o.fields["startCursor"] = startCursor
	return o
}

func connection(typeName string, all []any, first *int, after any) *gqlObj {
	start := indexAfterCursor(after, len(all))
	nodes := all[start:]
	if first != nil && len(nodes) > *first {
		nodes = nodes[:*first]
	}
	o := newGqlObj(typeName)
	o.fields["nodes"] = nodes
	o.fields["pageInfo"] = pageInfoFor(len(nodes), len(all), first, start)
	o.fields["totalCount"] = len(all)
	return o
}

func firstArg(args map[string]any) *int {
	if n, ok := asInt(args["first"]); ok {
		return &n
	}
	return nil
}

// asInt converts a GraphQL argument value (int literal, float64 from JSON
// variables, or json.Number) to an int.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// repoFromIDPart splits an `owner/name` global-id part into its repo (nil
// when malformed or unknown).
func repoFromIDPart(forge *Forge, part string) *Repo {
	i := strings.IndexByte(part, '/')
	if i < 0 {
		return nil
	}
	if part[:i] != ViewerLogin {
		return nil
	}
	return forge.GetRepo(part[i+1:])
}

func commentNode(repo *Repo, issue *Issue, comment *Comment) *gqlObj {
	o := newGqlObj("IssueComment")
	o.fields["id"] = GlobalID("IssueComment", repo.Owner+"/"+repo.Name, strconv.Itoa(issue.Number), strconv.Itoa(comment.ID))
	o.fields["body"] = comment.Body
	o.fields["_repo"] = repo
	o.fields["_issue"] = issue
	o.fields["_comment"] = comment
	return o
}

// ---------------------------------------------------------------------------------------------
// resolvers: one switch per type. A resolver returns an error on a mutation
// failure; the executor turns it into a field error and nulls the field.
// The second return reports "no such field" (undefined in the reference).
// ---------------------------------------------------------------------------------------------

type gqlResolver func(obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error)

func resolveQuery(_obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error) {
	switch field {
	case "viewer":
		return userNode(ViewerLogin), true, nil
	case "repository":
		owner, ok1 := asString(args["owner"])
		name, ok2 := asString(args["name"])
		if !ok1 || !ok2 || owner != ViewerLogin {
			return nil, true, nil
		}
		repo := forge.GetRepo(name)
		if repo == nil {
			return nil, true, nil
		}
		return repoNode(repo), true, nil
	case "node":
		id, ok := asString(args["id"])
		if !ok {
			return nil, true, nil
		}
		kind, parts, ok := DecodeGlobalID(id)
		if !ok {
			return nil, true, nil
		}
		switch kind {
		case "Repository":
			if len(parts) < 1 {
				return nil, true, nil
			}
			repo := repoFromIDPart(forge, parts[0])
			if repo == nil {
				return nil, true, nil
			}
			return repoNode(repo), true, nil
		case "Issue", "PullRequest":
			if len(parts) < 2 {
				return nil, true, nil
			}
			repo := repoFromIDPart(forge, parts[0])
			number, err := strconv.Atoi(parts[1])
			if repo == nil || err != nil {
				return nil, true, nil
			}
			if kind == "Issue" {
				issue := forge.GetIssue(repo, number)
				if issue == nil {
					return nil, true, nil
				}
				return issueNode(repo, issue), true, nil
			}
			pull := forge.GetPull(repo, number)
			if pull == nil {
				return nil, true, nil
			}
			return pullNode(repo, pull), true, nil
		}
		return nil, true, nil
	case "__type":
		name, ok := asString(args["name"])
		if !ok {
			return nil, true, nil
		}
		if values, ok := enumValues[name]; ok {
			o := newGqlObj("__Type")
			o.fields["name"] = name
			o.fields["kind"] = "ENUM"
			o.fields["fields"] = nil
			list := make([]any, len(values))
			for i, v := range values {
				e := newGqlObj("__EnumValue")
				e.fields["name"] = v
				list[i] = e
			}
			o.fields["enumValues"] = list
			return o, true, nil
		}
		if def, ok := typeDefs[name]; ok {
			o := newGqlObj("__Type")
			o.fields["name"] = name
			o.fields["kind"] = "OBJECT"
			list := make([]any, len(def.fields))
			for i, f := range def.fields {
				field := newGqlObj("__Field")
				field.fields["name"] = f
				list[i] = field
			}
			o.fields["fields"] = list
			o.fields["enumValues"] = nil
			return o, true, nil
		}
		return nil, true, nil
	case "__schema":
		o := newGqlObj("__Schema")
		o.fields["types"] = []any{}
		o.fields["queryType"] = nil
		o.fields["mutationType"] = nil
		return o, true, nil
	}
	return nil, false, nil
}

func resolveMutation(_obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error) {
	input := asMap(args["input"])
	switch field {
	case "createIssue":
		repositoryID, ok1 := asString(input["repositoryId"])
		title, ok2 := asString(input["title"])
		if !ok1 || !ok2 {
			return nil, true, fmt.Errorf("createIssue requires repositoryId and title")
		}
		kind, parts, ok := DecodeGlobalID(repositoryID)
		if !ok || kind != "Repository" || len(parts) < 1 {
			return nil, true, fmt.Errorf("Unknown repository")
		}
		repo := repoFromIDPart(forge, parts[0])
		if repo == nil {
			return nil, true, fmt.Errorf("Unknown repository")
		}
		body, _ := asString(input["body"])
		created, errMsg := forge.CreateIssue(repo, title, body, []string{})
		if errMsg != "" {
			return nil, true, fmt.Errorf("%s", errMsg)
		}
		o := newGqlObj("CreateIssuePayload")
		o.fields["issue"] = issueNode(repo, created)
		return o, true, nil
	case "closeIssue":
		issueID, ok := asString(input["issueId"])
		if !ok {
			return nil, true, fmt.Errorf("closeIssue requires issueId")
		}
		kind, parts, ok := DecodeGlobalID(issueID)
		if !ok || kind != "Issue" || len(parts) < 2 {
			return nil, true, fmt.Errorf("Unknown issue")
		}
		repo := repoFromIDPart(forge, parts[0])
		number, err := strconv.Atoi(parts[1])
		var issue *Issue
		if repo != nil && err == nil {
			issue = forge.GetIssue(repo, number)
		}
		if issue == nil {
			return nil, true, fmt.Errorf("Unknown issue")
		}
		issue.State = "closed"
		// The REST pool for this issue must go too, or GET would serve stale JSON.
		InvalidateIssue(issue)
		o := newGqlObj("CloseIssuePayload")
		o.fields["issue"] = issueNode(repo, issue)
		return o, true, nil
	case "createPullRequest":
		repositoryID, ok1 := asString(input["repositoryId"])
		title, ok2 := asString(input["title"])
		baseRefName, ok3 := asString(input["baseRefName"])
		headRefName, ok4 := asString(input["headRefName"])
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, true, fmt.Errorf("createPullRequest requires repositoryId, title, baseRefName and headRefName")
		}
		kind, parts, ok := DecodeGlobalID(repositoryID)
		if !ok || kind != "Repository" || len(parts) < 1 {
			return nil, true, fmt.Errorf("Unknown repository")
		}
		repo := repoFromIDPart(forge, parts[0])
		if repo == nil {
			return nil, true, fmt.Errorf("Unknown repository")
		}
		body, _ := asString(input["body"])
		created, errMsg := forge.CreatePull(repo, title, body, headRefName, baseRefName)
		if errMsg != "" {
			return nil, true, fmt.Errorf("%s", errMsg)
		}
		o := newGqlObj("CreatePullRequestPayload")
		o.fields["pullRequest"] = pullNode(repo, created)
		return o, true, nil
	case "mergePullRequest":
		pullRequestID, ok := asString(input["pullRequestId"])
		if !ok {
			return nil, true, fmt.Errorf("mergePullRequest requires pullRequestId")
		}
		kind, parts, ok := DecodeGlobalID(pullRequestID)
		if !ok || kind != "PullRequest" || len(parts) < 2 {
			return nil, true, fmt.Errorf("Unknown pull request")
		}
		repo := repoFromIDPart(forge, parts[0])
		number, err := strconv.Atoi(parts[1])
		var pull *Pull
		if repo != nil && err == nil {
			pull = forge.GetPull(repo, number)
		}
		if pull == nil {
			return nil, true, fmt.Errorf("Unknown pull request")
		}
		if sha := forge.MergePull(repo, pull); sha == "" {
			return nil, true, fmt.Errorf("Merge conflict")
		}
		o := newGqlObj("MergePullRequestPayload")
		o.fields["pullRequest"] = pullNode(repo, pull)
		return o, true, nil
	case "addComment":
		subjectID, ok1 := asString(input["subjectId"])
		body, ok2 := asString(input["body"])
		if !ok1 || !ok2 {
			return nil, true, fmt.Errorf("addComment requires subjectId and body")
		}
		kind, parts, ok := DecodeGlobalID(subjectID)
		if !ok || kind != "Issue" || len(parts) < 2 {
			return nil, true, fmt.Errorf("Unknown subject")
		}
		repo := repoFromIDPart(forge, parts[0])
		number, err := strconv.Atoi(parts[1])
		var issue *Issue
		if repo != nil && err == nil {
			issue = forge.GetIssue(repo, number)
		}
		if issue == nil {
			return nil, true, fmt.Errorf("Unknown issue")
		}
		comment := forge.CreateComment(repo, issue, body)
		edge := newGqlObj("CommentEdge")
		edge.fields["node"] = commentNode(repo, issue, comment)
		o := newGqlObj("AddCommentPayload")
		o.fields["commentEdge"] = edge
		return o, true, nil
	}
	return nil, false, nil
}

func resolveRepository(obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error) {
	repo := obj.repo()
	switch field {
	case "id":
		return GlobalID("Repository", repo.Owner+"/"+repo.Name), true, nil
	case "databaseId":
		return repo.ID, true, nil
	case "name":
		return repo.Name, true, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, true, nil
	case "owner":
		return userNode(ViewerLogin), true, nil
	case "defaultBranchRef":
		tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return nil, true, nil
		}
		ref := newGqlObj("Ref")
		ref.fields["name"] = repo.DefaultBranch
		target := newGqlObj("Commit")
		target.fields["oid"] = tip
		target.fields["_repo"] = repo
		ref.fields["target"] = target
		return ref, true, nil
	case "viewerPermission":
		return "ADMIN", true, nil
	case "hasIssuesEnabled":
		return true, true, nil
	case "issue":
		number, ok := asInt(args["number"])
		if !ok {
			return nil, true, nil
		}
		issue := forge.GetIssue(repo, number)
		if issue == nil {
			return nil, true, nil
		}
		return issueNode(repo, issue), true, nil
	case "issues":
		var states []string
		if arr, ok := args["states"].([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states = append(states, strings.ToLower(str))
				}
			}
		}
		orderBy := asMap(args["orderBy"])
		ascending := orderBy["direction"] == "ASC"
		list := append([]*Issue(nil), repo.Issues...)
		if states != nil {
			wanted := make(map[string]bool, len(states))
			for _, s := range states {
				wanted[s] = true
			}
			filtered := list[:0]
			for _, issue := range list {
				if wanted[issue.State] {
					filtered = append(filtered, issue)
				}
			}
			list = filtered
		}
		sort.Slice(list, func(i, j int) bool {
			if ascending {
				return list[i].CreatedSeq < list[j].CreatedSeq
			}
			return list[i].CreatedSeq > list[j].CreatedSeq
		})
		nodes := make([]any, len(list))
		for i, issue := range list {
			nodes[i] = issueNode(repo, issue)
		}
		return connection("IssueConnection", nodes, firstArg(args), args["after"]), true, nil
	case "pullRequests":
		var states []string
		if arr, ok := args["states"].([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states = append(states, strings.ToLower(str))
				}
			}
		}
		list := append([]*Pull(nil), repo.Pulls...)
		if states != nil {
			wanted := make(map[string]bool, len(states))
			for _, s := range states {
				wanted[s] = true
			}
			filtered := list[:0]
			for _, pull := range list {
				state := pull.State
				if pull.Merged {
					state = "merged"
				}
				if wanted[state] {
					filtered = append(filtered, pull)
				}
			}
			list = filtered
		}
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
		nodes := make([]any, len(list))
		for i, pull := range list {
			nodes[i] = pullNode(repo, pull)
		}
		return connection("PullRequestConnection", nodes, firstArg(args), args["after"]), true, nil
	case "pullRequest":
		number, ok := asInt(args["number"])
		if !ok {
			return nil, true, nil
		}
		pull := forge.GetPull(repo, number)
		if pull == nil {
			return nil, true, nil
		}
		return pullNode(repo, pull), true, nil
	case "description":
		return repo.Description, true, nil
	case "isPrivate":
		return repo.Private, true, nil
	case "createdAt":
		return TrialISO, true, nil
	case "url":
		return "https://example.invalid/" + repo.Owner + "/" + repo.Name, true, nil
	}
	return nil, false, nil
}

func resolveIssue(obj *gqlObj, field string, args map[string]any, _forge *Forge) (any, bool, error) {
	repo, issue := obj.repo(), obj.issue()
	switch field {
	case "id":
		return GlobalID("Issue", repo.Owner+"/"+repo.Name, strconv.Itoa(issue.Number)), true, nil
	case "number":
		return issue.Number, true, nil
	case "title":
		return issue.Title, true, nil
	case "body":
		return issue.Body, true, nil
	case "state":
		if issue.State == "open" {
			return "OPEN", true, nil
		}
		return "CLOSED", true, nil
	case "createdAt":
		return TrialISO, true, nil
	case "updatedAt":
		return TrialISO, true, nil
	case "url":
		return "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number), true, nil
	case "author":
		return userNode(ViewerLogin), true, nil
	case "repository":
		return repoNode(repo), true, nil
	case "comments":
		nodes := make([]any, len(issue.Comments))
		for i, c := range issue.Comments {
			nodes[i] = commentNode(repo, issue, c)
		}
		return connection("IssueCommentConnection", nodes, firstArg(args), args["after"]), true, nil
	case "labels":
		nodes := make([]any, len(issue.Labels))
		for i, name := range issue.Labels {
			color := "000000"
			if l := repo.Labels[name]; l != nil {
				color = l.Color
			}
			l := newGqlObj("Label")
			l.fields["name"] = name
			l.fields["color"] = color
			nodes[i] = l
		}
		return nodes, true, nil
	}
	return nil, false, nil
}

func resolvePullRequest(obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error) {
	repo, pull := obj.repo(), obj.pull()
	switch field {
	case "id":
		return GlobalID("PullRequest", repo.Owner+"/"+repo.Name, strconv.Itoa(pull.Number)), true, nil
	case "number":
		return pull.Number, true, nil
	case "title":
		return pull.Title, true, nil
	case "body":
		return pull.Body, true, nil
	case "state":
		if pull.Merged {
			return "MERGED", true, nil
		}
		if pull.State == "open" {
			return "OPEN", true, nil
		}
		return "CLOSED", true, nil
	case "merged":
		return pull.Merged, true, nil
	case "headRefName":
		return pull.HeadRef, true, nil
	case "baseRefName":
		return pull.BaseRef, true, nil
	case "mergeable":
		if pull.State != "open" {
			return "UNKNOWN", true, nil
		}
		if forge.PullConflicts(repo, pull) {
			return "CONFLICTING", true, nil
		}
		return "MERGEABLE", true, nil
	case "files":
		changes := forge.PullFiles(repo, pull)
		nodes := make([]any, len(changes))
		for i, c := range changes {
			f := newGqlObj("PullRequestChangedFile")
			f.fields["path"] = c.Path
			nodes[i] = f
		}
		return connection("PullRequestChangedFileConnection", nodes, firstArg(args), args["after"]), true, nil
	case "headRef":
		ref := newGqlObj("Ref")
		ref.fields["name"] = pull.HeadRef
		target := newGqlObj("Commit")
		target.fields["oid"] = pull.HeadSha
		target.fields["_repo"] = repo
		ref.fields["target"] = target
		return ref, true, nil
	case "baseRef":
		ref := newGqlObj("Ref")
		ref.fields["name"] = pull.BaseRef
		target := newGqlObj("Commit")
		target.fields["oid"] = pull.BaseSha
		target.fields["_repo"] = repo
		ref.fields["target"] = target
		return ref, true, nil
	case "mergedAt":
		if pull.MergedAt == "" {
			return nil, true, nil
		}
		return pull.MergedAt, true, nil
	case "mergeCommit":
		if pull.MergeCommitSha == "" {
			return nil, true, nil
		}
		c := newGqlObj("Commit")
		c.fields["oid"] = pull.MergeCommitSha
		c.fields["_repo"] = repo
		return c, true, nil
	case "createdAt":
		return TrialISO, true, nil
	case "url":
		return "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number), true, nil
	case "author":
		return userNode(ViewerLogin), true, nil
	case "repository":
		return repoNode(repo), true, nil
	}
	return nil, false, nil
}

func resolveUser(obj *gqlObj, field string, _args map[string]any, _forge *Forge) (any, bool, error) {
	login, _ := obj.fields["login"].(string)
	switch field {
	case "login":
		return login, true, nil
	case "id":
		return GlobalID("User", login), true, nil
	case "name":
		return "ada iversen", true, nil
	case "email":
		return "adaiversen@example.invalid", true, nil
	}
	return nil, false, nil
}

// resolvePassthrough reads straight off the object (connections, payloads,
// edges, page info, refs, labels).
func resolvePassthrough(obj *gqlObj, field string, _args map[string]any, _forge *Forge) (any, bool, error) {
	v, ok := obj.fields[field]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func resolveCommit(obj *gqlObj, field string, _args map[string]any, _forge *Forge) (any, bool, error) {
	switch field {
	case "oid":
		return obj.fields["oid"], true, nil
	case "message":
		repo := obj.repo()
		oid, _ := obj.fields["oid"].(string)
		if repo == nil {
			return "", true, nil
		}
		object := repo.Objects[oid]
		if object == nil || object.Kind != KindCommit {
			return "", true, nil
		}
		return ParseCommit(object.Data).Message, true, nil
	}
	return nil, false, nil
}

func resolveIssueComment(obj *gqlObj, field string, _args map[string]any, _forge *Forge) (any, bool, error) {
	switch field {
	case "id":
		return obj.fields["id"], true, nil
	case "body":
		return obj.fields["body"], true, nil
	case "author":
		return userNode(ViewerLogin), true, nil
	case "createdAt":
		return TrialISO, true, nil
	}
	return nil, false, nil
}

func resolveIntrospection(obj *gqlObj, field string, _args map[string]any, _forge *Forge) (any, bool, error) {
	switch field {
	case "name", "kind", "fields", "enumValues", "types", "queryType", "mutationType":
		v, ok := obj.fields[field]
		if !ok {
			// __Type objects carry only the fields their kind has; the
			// missing ones read as null.
			return nil, true, nil
		}
		return v, true, nil
	}
	return nil, false, nil
}

// resolveField dispatches to the per-type resolver.
func resolveField(typeName string, obj *gqlObj, field string, args map[string]any, forge *Forge) (any, bool, error) {
	var fn gqlResolver
	switch typeName {
	case "Query":
		fn = resolveQuery
	case "Mutation":
		fn = resolveMutation
	case "Repository":
		fn = resolveRepository
	case "Issue":
		fn = resolveIssue
	case "PullRequest":
		fn = resolvePullRequest
	case "User":
		fn = resolveUser
	case "IssueConnection", "PullRequestConnection", "IssueCommentConnection",
		"PullRequestChangedFileConnection", "CreateIssuePayload", "CloseIssuePayload",
		"CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload",
		"CommentEdge", "PageInfo", "Ref", "Label", "PullRequestChangedFile":
		fn = resolvePassthrough
	case "Commit":
		fn = resolveCommit
	case "IssueComment":
		fn = resolveIssueComment
	case "Node":
		// The Node interface exposes only id, answered by the concrete
		// type's own id resolver (execution normally keeps the concrete
		// type, so this is a backstop).
		if field != "id" || obj.typ == "" || obj.typ == "Node" {
			return nil, false, nil
		}
		return resolveField(obj.typ, obj, field, args, forge)
	case "__Type", "__Field", "__EnumValue", "__Schema":
		fn = resolveIntrospection
	default:
		return nil, false, nil
	}
	return fn(obj, field, args, forge)
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

func executeSelections(forge *Forge, obj *gqlObj, typeName string, selections []*gqlSelection, variables map[string]any, fragments map[string]*gqlFragment, errors *[]gqlError) map[string]any {
	out := make(map[string]any, len(selections))
	for _, sel := range selections {
		switch sel.kind {
		case selSpread:
			frag, ok := fragments[sel.name]
			if !ok {
				continue
			}
			// A named spread applies only when the runtime type matches
			// its condition (or the condition is the Node interface,
			// which every node type implements). Without this, an Issue
			// fragment spread on a PullRequest would resolve Issue
			// fields against a PR object and nil-panic.
			if frag.typeCondition != "" && frag.typeCondition != typeName && frag.typeCondition != "Node" {
				continue
			}
			nested := executeSelections(forge, obj, typeName, frag.selections, variables, fragments, errors)
			for k, v := range nested {
				out[k] = v
			}
			continue
		case selInline:
			cond := sel.typeCondition
			if cond != "" && cond != typeName && cond != "Node" {
				continue
			}
			// The selections always execute against the concrete
			// runtime type: for a Node fragment the concrete type's
			// own id resolver answers (there is no separate Node
			// object), and __typename stays the concrete type.
			nested := executeSelections(forge, obj, typeName, sel.selections, variables, fragments, errors)
			for k, v := range nested {
				out[k] = v
			}
			continue
		}
		name := sel.name
		responseKey := name
		if sel.alias != "" {
			responseKey = sel.alias
		}
		if name == "__typename" {
			out[responseKey] = typeName
			continue
		}
		args := make(map[string]any, len(sel.args))
		for _, arg := range sel.args {
			args[arg.name] = evalGqlValue(arg.value, variables)
		}
		value, found, err := resolveField(typeName, obj, name, args, forge)
		if err != nil {
			*errors = append(*errors, gqlError{Message: err.Error()})
			out[responseKey] = nil
			continue
		}
		if !found {
			*errors = append(*errors, gqlError{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, typeName)})
			out[responseKey] = nil
			continue
		}
		out[responseKey] = completeValue(forge, value, sel.selections, variables, fragments, errors)
	}
	return out
}

func completeValue(forge *Forge, value any, selections []*gqlSelection, variables map[string]any, fragments map[string]*gqlFragment, errors *[]gqlError) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = completeValue(forge, item, selections, variables, fragments, errors)
		}
		return out
	case *gqlObj:
		if len(selections) == 0 {
			return nil
		}
		return executeSelections(forge, v, v.typ, selections, variables, fragments, errors)
	default:
		return value
	}
}

type gqlResponse struct {
	Data   any        `json:"data"`
	Errors []gqlError `json:"errors,omitempty"`
}

func executeGqlRequest(forge *Forge, request any) gqlResponse {
	req, ok := request.(map[string]any)
	if !ok {
		return gqlResponse{Data: nil, Errors: []gqlError{{Message: "Request must be a JSON object with a query field."}}}
	}
	query, ok := req["query"].(string)
	if !ok || query == "" {
		return gqlResponse{Data: nil, Errors: []gqlError{{Message: "Must provide a query string."}}}
	}
	var document *gqlDocument
	var syntaxErr string
	func() {
		defer func() {
			if r := recover(); r != nil {
				document = nil
				syntaxErr = fmt.Sprint(r)
			}
		}()
		document = (&gqlParser{lexer: &gqlLexer{src: query}}).parseDocument()
	}()
	if document == nil {
		if syntaxErr == "" {
			syntaxErr = "Syntax error"
		}
		return gqlResponse{Data: nil, Errors: []gqlError{{Message: syntaxErr}}}
	}
	var operation *gqlOperation
	if name, ok := req["operationName"].(string); ok {
		for _, op := range document.operations {
			if op.name == name {
				operation = op
				break
			}
		}
	} else if len(document.operations) > 0 {
		operation = document.operations[0]
	}
	if operation == nil {
		return gqlResponse{Data: nil, Errors: []gqlError{{Message: "Unknown operation."}}}
	}
	variables := map[string]any{}
	provided := asMap(req["variables"])
	for _, def := range operation.variables {
		if v, ok := provided[def.name]; ok {
			variables[def.name] = v
		} else if def.defaultValue != nil {
			variables[def.name] = evalGqlValue(def.defaultValue, map[string]any{})
		} else {
			variables[def.name] = nil
		}
	}
	var errors []gqlError
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	validateSelections(rootType, operation.selections, document.fragments, &errors, map[string]bool{})
	if len(errors) > 0 {
		return gqlResponse{Data: nil, Errors: errors}
	}
	root := newGqlObj(rootType)
	data := executeSelections(forge, root, rootType, operation.selections, variables, document.fragments, &errors)
	if len(errors) > 0 {
		return gqlResponse{Data: data, Errors: errors}
	}
	return gqlResponse{Data: data}
}

// HandleGraphql handles one GraphQL request body (a single object or an
// array for batching). Returns the HTTP status and the JSON body.
func HandleGraphql(forge *Forge, body []byte) (int, []byte) {
	if len(body) == 0 {
		return 400, []byte(`{"errors":[{"message":"Must provide a query string."}]}`)
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return 400, []byte(`{"errors":[{"message":"Problems parsing JSON"}]}`)
	}
	if arr, ok := parsed.([]any); ok {
		responses := make([]gqlResponse, len(arr))
		for i, req := range arr {
			responses[i] = executeGqlRequest(forge, req)
		}
		return 200, marshalNoHTML(responses)
	}
	return 200, marshalNoHTML(executeGqlRequest(forge, parsed))
}
