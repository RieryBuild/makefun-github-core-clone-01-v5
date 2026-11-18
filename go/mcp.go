// The MCP surface (POST /mcp): Streamable HTTP, JSON-RPC 2.0, no session
// handshake.
//
// The suite pins down:
//   - the modern era: `server/discover` returns {supportedVersions:
//     ['2026-07-28','2025-11-25'], capabilities: {tools: {}}, serverInfo:
//     {name, version}} (the serverInfo also under
//     _meta['io.modelcontextprotocol/serverInfo']); the legacy era (initialize +
//     notifications/initialized) is the fallback and is implemented too.
//   - tools/list returns >= 6 tools, byte-identical across calls (the list is
//     built once at package load from the spec table below).
//   - every tool result carries BOTH content:[{type:'text', text: JSON of the
//     result}] and structuredContent: <result>.
//   - an unknown tool -> JSON-RPC -32601, never HTTP 500; a malformed JSON body
//     -> HTTP 400 with JSON-RPC -32700 and id null; missing required arguments
//     -> an isError:true result.
//   - a notification (no id) -> 202 with an empty body.
//   - authentication is decided by the router before this module runs (401
//     without a valid Bearer).

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const mcpServerName = "forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25"}

// ---------------------------------------------------------------------------------------------
// the tool catalogue: a compact spec table, expanded once at package load
// ---------------------------------------------------------------------------------------------

type mcpToolSpec struct {
	name        string
	description string
	props       [][3]string // name, type, description
	required    []string
}

var mcpToolSpecs = []mcpToolSpec{
	{
		"create_repo", "Create a repository owned by the authenticated user.",
		[][3]string{
			{"name", "string", "The repository name."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		},
		[]string{"name"},
	},
	{
		"get_repo", "Get a repository by owner and name.",
		[][3]string{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
		},
		[]string{"owner", "repo"},
	},
	{
		"create_issue", "Create an issue in a repository.",
		[][3]string{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The issue title."},
			{"body", "string", "The issue body."},
		},
		[]string{"owner", "repo", "title"},
	},
	{
		"list_issues", "List the issues of a repository, optionally filtered by state.",
		[][3]string{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"state", "string", "open, closed, or all."},
		},
		[]string{"owner", "repo"},
	},
	{
		"create_pull_request", "Create a pull request from a head branch onto a base branch.",
		[][3]string{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The pull request title."},
			{"head", "string", "The head branch name."},
			{"base", "string", "The base branch name."},
			{"body", "string", "The pull request body."},
		},
		[]string{"owner", "repo", "title", "head", "base"},
	},
	{
		"merge_pull_request", "Squash-merge a pull request onto its base branch.",
		[][3]string{
			{"owner", "string", "The repository owner login."},
			{"repo", "string", "The repository name."},
			{"pull_number", "integer", "The pull request number."},
		},
		[]string{"owner", "repo", "pull_number"},
	},
}

// mcpToolDef is one tool as tools/list returns it. Struct field order fixes
// the JSON key order, so the list is byte-identical across calls.
type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema mcpSchema `json:"inputSchema"`
}

type mcpSchema struct {
	Type       string                       `json:"type"`
	Properties map[string]mcpProp           `json:"properties"`
	Required   []string                     `json:"required"`
}

type mcpProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// mcpTools is the tools/list payload, built once.
var mcpTools = func() []mcpToolDef {
	tools := make([]mcpToolDef, len(mcpToolSpecs))
	for i, spec := range mcpToolSpecs {
		props := make(map[string]mcpProp, len(spec.props))
		for _, p := range spec.props {
			props[p[0]] = mcpProp{Type: p[1], Description: p[2]}
		}
		tools[i] = mcpToolDef{
			Name:        spec.name,
			Description: spec.description,
			InputSchema: mcpSchema{Type: "object", Properties: props, Required: spec.required},
		}
	}
	return tools
}()

var mcpToolNames = func() map[string]bool {
	out := make(map[string]bool, len(mcpTools))
	for _, t := range mcpTools {
		out[t.Name] = true
	}
	return out
}()

func mcpDiscoverResult() map[string]any {
	info := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
	return map[string]any{
		"supportedVersions": append([]string(nil), mcpSupportedVersions...),
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        info,
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": info},
	}
}

// ---------------------------------------------------------------------------------------------
// tool implementations (the same forge operations as REST/GraphQL)
// ---------------------------------------------------------------------------------------------

func mcpRepoObject(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func mcpRequireString(args map[string]any, name string) (string, error) {
	v, ok := args[name].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("Missing required argument: %s", name)
	}
	return v, nil
}

func mcpFindRepo(forge *Forge, args map[string]any) (*Repo, error) {
	owner, err := mcpRequireString(args, "owner")
	if err != nil {
		return nil, err
	}
	repoName, err := mcpRequireString(args, "repo")
	if err != nil {
		return nil, err
	}
	if owner != ViewerLogin {
		return nil, fmt.Errorf("Not Found")
	}
	repo := forge.GetRepo(repoName)
	if repo == nil {
		return nil, fmt.Errorf("Not Found")
	}
	return repo, nil
}

func mcpCallTool(forge *Forge, name string, args map[string]any) (any, error) {
	switch name {
	case "create_repo":
		repoName, err := mcpRequireString(args, "name")
		if err != nil {
			return nil, err
		}
		description, _ := args["description"].(string)
		private, _ := args["private"].(bool)
		repo := forge.CreateRepo(repoName, private, description, "")
		// An invalid name and a taken name both surface the same message.
		if repo == nil {
			return nil, fmt.Errorf("Repository already exists")
		}
		return mcpRepoObject(repo), nil
	case "get_repo":
		repo, err := mcpFindRepo(forge, args)
		if err != nil {
			return nil, err
		}
		return mcpRepoObject(repo), nil
	case "create_issue":
		repo, err := mcpFindRepo(forge, args)
		if err != nil {
			return nil, err
		}
		title, err := mcpRequireString(args, "title")
		if err != nil {
			return nil, err
		}
		body, _ := args["body"].(string)
		created, errMsg := forge.CreateIssue(repo, title, body, []string{})
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return map[string]any{"id": created.ID, "number": created.Number, "title": created.Title, "body": created.Body, "state": created.State, "labels": []any{}}, nil
	case "list_issues":
		repo, err := mcpFindRepo(forge, args)
		if err != nil {
			return nil, err
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		state = strings.ToLower(state)
		list := make([]any, 0)
		// Newest first (creation sequence descending).
		for i := len(repo.Issues) - 1; i >= 0; i-- {
			issue := repo.Issues[i]
			if state != "all" && issue.State != state {
				continue
			}
			list = append(list, map[string]any{"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State})
		}
		return list, nil
	case "create_pull_request":
		repo, err := mcpFindRepo(forge, args)
		if err != nil {
			return nil, err
		}
		title, err := mcpRequireString(args, "title")
		if err != nil {
			return nil, err
		}
		head, err := mcpRequireString(args, "head")
		if err != nil {
			return nil, err
		}
		base, err := mcpRequireString(args, "base")
		if err != nil {
			return nil, err
		}
		body, _ := args["body"].(string)
		created, errMsg := forge.CreatePull(repo, title, body, head, base)
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return map[string]any{
			"id": created.ID, "number": created.Number, "title": created.Title, "body": created.Body, "state": created.State,
			"head": map[string]any{"ref": created.HeadRef, "sha": created.HeadSha},
			"base": map[string]any{"ref": created.BaseRef, "sha": created.BaseSha},
		}, nil
	case "merge_pull_request":
		repo, err := mcpFindRepo(forge, args)
		if err != nil {
			return nil, err
		}
		pullNumber, ok := asInt(args["pull_number"])
		if !ok {
			return nil, fmt.Errorf("Missing required argument: pull_number")
		}
		pull := forge.GetPull(repo, pullNumber)
		if pull == nil {
			return nil, fmt.Errorf("Not Found")
		}
		if pull.State != "open" {
			return nil, fmt.Errorf("Pull request is not open")
		}
		sha := forge.MergePull(repo, pull)
		if sha == "" {
			return nil, fmt.Errorf("Merge conflict")
		}
		return map[string]any{"merged": true, "sha": sha, "message": "Merge pull request #" + fmt.Sprint(pull.Number) + " from " + pull.HeadRef}, nil
	}
	return nil, fmt.Errorf("Unknown tool: %s", name)
}

// ---------------------------------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------------------------------

type mcpResult struct {
	status int
	body   []byte
}

func mcpRPCResult(id any, result any) mcpResult {
	return mcpResult{status: 200, body: marshalNoHTML(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})}
}

func mcpRPCError(id any, code int, message string, status int) mcpResult {
	return mcpResult{status: status, body: marshalNoHTML(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})}
}

func mcpHandleRequest(forge *Forge, request any) mcpResult {
	envelope, ok := request.(map[string]any)
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request", 200)
	}
	_, hasID := envelope["id"]
	method, isMethod := envelope["method"].(string)
	if !isMethod {
		if hasID {
			return mcpRPCError(envelope["id"], -32600, "Invalid Request", 200)
		}
		return mcpResult{status: 202, body: []byte{}}
	}
	if !hasID {
		// A notification (the initialize handshake, a cancellation, ...):
		// acknowledge, no body.
		return mcpResult{status: 202, body: []byte{}}
	}
	id := envelope["id"]
	switch method {
	case "server/discover":
		return mcpRPCResult(id, mcpDiscoverResult())
	case "initialize":
		params := asMap(envelope["params"])
		requested, _ := params["protocolVersion"].(string)
		protocolVersion := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if requested == v {
				protocolVersion = v
				break
			}
		}
		return mcpRPCResult(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
	case "tools/list":
		return mcpRPCResult(id, map[string]any{"tools": mcpTools})
	case "tools/call":
		args := asMap(envelope["params"])
		toolName, ok := args["name"].(string)
		if !ok {
			return mcpRPCError(id, -32602, "Invalid params: name is required", 200)
		}
		if !mcpToolNames[toolName] {
			return mcpRPCError(id, -32601, "Unknown tool: "+toolName, 200)
		}
		toolArgs := asMap(args["arguments"])
		result, err := mcpCallTool(forge, toolName, toolArgs)
		if err != nil {
			return mcpRPCResult(id, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		return mcpRPCResult(id, map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(marshalNoHTML(result))}},
			"structuredContent": result,
		})
	case "ping":
		return mcpRPCResult(id, map[string]any{})
	default:
		return mcpRPCError(id, -32601, "Method not found: "+method, 200)
	}
}

// HandleMcp handles one MCP request body (a single JSON-RPC message, or a
// batch array). Returns the HTTP status and the JSON body.
func HandleMcp(forge *Forge, body []byte) (int, []byte) {
	if len(body) == 0 {
		return 400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return 400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
	}
	if arr, ok := parsed.([]any); ok {
		if len(arr) == 0 {
			return 400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)
		}
		responses := []any{}
		for _, req := range arr {
			result := mcpHandleRequest(forge, req)
			if result.status == 202 {
				continue
			}
			var decoded any
			_ = json.Unmarshal(result.body, &decoded)
			responses = append(responses, decoded)
		}
		// A batch of only notifications gets no response at all.
		if len(responses) == 0 {
			return 202, nil
		}
		return 200, marshalNoHTML(responses)
	}
	result := mcpHandleRequest(forge, parsed)
	return result.status, result.body
}
