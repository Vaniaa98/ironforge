package main

// MCP endpoint (POST /mcp) — worker 3. JSON-RPC 2.0 Streamable HTTP.
//
// Two protocol eras, both served:
//   modern 2026-07-28: server/discover answers supportedVersions; no
//           handshake, no session.
//   legacy <=2025-11-25: initialize (+ notifications/initialized) echoes the
//           offered revision; no session id is issued (optional per spec).
//
// tools/list is built ONCE from fixed structs (byte-identical across calls).
// All tools work on the same Store as REST/GraphQL. No wall clock, no
// randomness; every timestamp is epochStr().

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tool definitions (fixed; marshalled once).
// ---------------------------------------------------------------------------

type mcpProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpSchema struct {
	Type       string             `json:"type"`
	Properties map[string]mcpProp `json:"properties"`
	Required   []string           `json:"required"`
}

type mcpToolDef struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	InputSchema mcpSchema `json:"inputSchema"`
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"name":        {Type: "string", Description: "Repository name"},
			"description": {Type: "string", Description: "Repository description"},
			"private":     {Type: "boolean", Description: "Whether the repository is private"},
		}, Required: []string{"name"}},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Issue title"},
			"body":  {Type: "string", Description: "Issue body"},
		}, Required: []string{"owner", "repo", "title"}},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"state": {Type: "string", Description: "Issue state filter: open, closed, or all"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Pull request title"},
			"body":  {Type: "string", Description: "Pull request body"},
			"head":  {Type: "string", Description: "Head branch name"},
			"base":  {Type: "string", Description: "Base branch name"},
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request in a repository.",
		InputSchema: mcpSchema{Type: "object", Properties: map[string]mcpProp{
			"owner":       {Type: "string", Description: "Repository owner login"},
			"repo":        {Type: "string", Description: "Repository name"},
			"pull_number": {Type: "integer", Description: "Pull request number"},
		}, Required: []string{"owner", "repo", "pull_number"}},
	},
}

// mcpToolsResultJSON is the pre-marshalled {"tools": [...]} result, built
// once so two tools/list answers carry byte-identical tools.
var mcpToolsResultJSON []byte

func init() {
	b, err := json.Marshal(map[string]any{"tools": mcpTools})
	if err != nil {
		panic(err)
	}
	mcpToolsResultJSON = b
}

func mcpFindTool(name string) *mcpToolDef {
	norm := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", ""))
	for i := range mcpTools {
		t := &mcpTools[i]
		tnorm := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(t.Name, "-", ""), "_", ""))
		if tnorm == norm || strings.HasSuffix(tnorm, norm) || strings.HasSuffix(norm, tnorm) {
			return t
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON-RPC envelope.
// ---------------------------------------------------------------------------

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any            `json:"result,omitempty"`
	Error   *mcpRPCError   `json:"error,omitempty"`
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpContentBlock `json:"content"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

func mcpWriteJSON(w http.ResponseWriter, s *Store, status int, v any) {
	writeJSON(w, s, status, v)
}

func mcpWriteRaw(w http.ResponseWriter, s *Store, status int, body []byte) {
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", s.NextReqID())
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

func mcpResult(id json.RawMessage, result any) mcpRPCResponse {
	return mcpRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpError(id json.RawMessage, code int, message string) mcpRPCResponse {
	return mcpRPCResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: code, Message: message}}
}

// mcpToolOK builds a successful tools/call result: text JSON + structured.
func mcpToolOK(payload any) mcpToolResult {
	text, _ := json.Marshal(payload)
	return mcpToolResult{
		Content:           []mcpContentBlock{{Type: "text", Text: string(text)}},
		StructuredContent: payload,
	}
}

func mcpToolErr(message string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContentBlock{{Type: "text", Text: message}},
		IsError: true,
	}
}

// ---------------------------------------------------------------------------
// Argument helpers.
// ---------------------------------------------------------------------------

func mcpStrArg(args map[string]any, name string) string {
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func mcpBoolArg(args map[string]any, name string) bool {
	if v, ok := args[name]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// mcpIntArg accepts a JSON number (float64) or a digit string.
func mcpIntArg(args map[string]any, name string) (int, bool) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	}
	return 0, false
}

// mcpMissingRequired lists required properties absent (or null/empty-string)
// from args. Mirrors the suite's own missing-argument judgement.
func mcpMissingRequired(tool *mcpToolDef, args map[string]any) []string {
	var missing []string
	for _, req := range tool.InputSchema.Required {
		v, ok := args[req]
		if !ok || v == nil {
			missing = append(missing, req)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			missing = append(missing, req)
		}
	}
	return missing
}

// ---------------------------------------------------------------------------
// Payload shapes (REST-parity: same objects REST serves).
// ---------------------------------------------------------------------------

func mcpRepoPayload(r *Repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"node_id":        gqlGlobalID("Repository", itoa(r.ID)),
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner, "id": ViewerID},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
		"html_url":       "https://example.invalid/" + r.Owner + "/" + r.Name,
		"created_at":     epochStr(),
		"updated_at":     epochStr(),
	}
}

func mcpIssuePayload(r *Repo, iss *Issue) map[string]any {
	return map[string]any{
		"id":         iss.Number,
		"node_id":    gqlGlobalID("Issue", itoa(r.ID), itoa(iss.Number)),
		"number":     iss.Number,
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID},
		"created_at": epochStr(),
		"updated_at": epochStr(),
	}
}

func mcpPullPayload(r *Repo, p *Pull) map[string]any {
	return map[string]any{
		"id":          p.Number,
		"node_id":     gqlGlobalID("PullRequest", itoa(r.ID), itoa(p.Number)),
		"number":      p.Number,
		"pull_number": p.Number,
		"title":       p.Title,
		"body":        p.Body,
		"state":       p.State,
		"head":        p.Head,
		"base":        p.Base,
		"merged":      p.Merged,
		"mergeable":   gqlMergeable(p),
		"created_at":  epochStr(),
		"updated_at":  epochStr(),
	}
}

// ---------------------------------------------------------------------------
// Tool implementations (each returns a tool result).
// ---------------------------------------------------------------------------

func mcpCallCreateRepo(s *Store, args map[string]any) mcpToolResult {
	name := mcpStrArg(args, "name")
	repo, apiErr := s.CreateRepo(name, mcpBoolArg(args, "private"), mcpStrArg(args, "description"))
	if apiErr != nil {
		return mcpToolErr(apiErr.Message)
	}
	return mcpToolOK(mcpRepoPayload(repo))
}

func mcpCallGetRepo(s *Store, args map[string]any) mcpToolResult {
	repo := s.GetRepo(mcpStrArg(args, "owner"), mcpStrArg(args, "repo"))
	if repo == nil {
		return mcpToolErr("Repository not found")
	}
	return mcpToolOK(mcpRepoPayload(repo))
}

func mcpCallCreateIssue(s *Store, args map[string]any) mcpToolResult {
	repo := s.GetRepo(mcpStrArg(args, "owner"), mcpStrArg(args, "repo"))
	if repo == nil {
		return mcpToolErr("Repository not found")
	}
	iss := gqlAddIssueToRepo(s, repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"))
	return mcpToolOK(mcpIssuePayload(repo, iss))
}

func mcpCallListIssues(s *Store, args map[string]any) mcpToolResult {
	repo := s.GetRepo(mcpStrArg(args, "owner"), mcpStrArg(args, "repo"))
	if repo == nil {
		return mcpToolErr("Repository not found")
	}
	state := strings.ToLower(mcpStrArg(args, "state"))
	if state == "" {
		state = "open"
	}
	s.mu.RLock()
	var list []any
	for _, iss := range repo.Issues {
		if state != "all" && iss.State != state {
			continue
		}
		list = append(list, mcpIssuePayload(repo, iss))
	}
	s.mu.RUnlock()
	if list == nil {
		list = []any{}
	}
	return mcpToolOK(list)
}

func mcpCallCreatePullRequest(s *Store, args map[string]any) mcpToolResult {
	repo := s.GetRepo(mcpStrArg(args, "owner"), mcpStrArg(args, "repo"))
	if repo == nil {
		return mcpToolErr("Repository not found")
	}
	pr, apiErr := createPullInRepo(s, repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"), mcpStrArg(args, "head"), mcpStrArg(args, "base"))
	if apiErr != nil {
		return mcpToolErr(apiErr.Message)
	}
	return mcpToolOK(mcpPullPayload(repo, pr))
}

func mcpCallMergePullRequest(s *Store, args map[string]any) mcpToolResult {
	repo := s.GetRepo(mcpStrArg(args, "owner"), mcpStrArg(args, "repo"))
	if repo == nil {
		return mcpToolErr("Repository not found")
	}
	number, ok := mcpIntArg(args, "pull_number")
	if !ok {
		return mcpToolErr("pull_number is required")
	}
	s.mu.Lock()
	var pr *Pull
	for _, cand := range repo.Pulls {
		if cand.Number == number {
			pr = cand
			break
		}
	}
	if pr == nil {
		s.mu.Unlock()
		return mcpToolErr("Pull request not found")
	}
	if pr.Merged {
		s.mu.Unlock()
		return mcpToolErr("Pull request already merged")
	}
	_, apiErr := mergePullSquash(repo, pr)
	s.mu.Unlock()
	if apiErr != nil {
		return mcpToolErr(apiErr.Message)
	}
	return mcpToolOK(mcpPullPayload(repo, pr))
}

// mcpDispatchTool runs one tools/call. Returns (result, ok) on success;
// on failure returns (zero, code, message, false) for a JSON-RPC error.
func mcpDispatchTool(s *Store, name string, args map[string]any) (mcpToolResult, int, string, bool) {
	tool := mcpFindTool(name)
	if tool == nil {
		return mcpToolResult{}, -32602, "Unknown tool: " + name, false
	}
	if args == nil {
		args = map[string]any{}
	}
	if missing := mcpMissingRequired(tool, args); len(missing) > 0 {
		return mcpToolResult{}, -32602, "Missing required argument(s): " + strings.Join(missing, ", "), false
	}
	var result mcpToolResult
	switch tool.Name {
	case "create_repo":
		result = mcpCallCreateRepo(s, args)
	case "get_repo":
		result = mcpCallGetRepo(s, args)
	case "create_issue":
		result = mcpCallCreateIssue(s, args)
	case "list_issues":
		result = mcpCallListIssues(s, args)
	case "create_pull_request":
		result = mcpCallCreatePullRequest(s, args)
	case "merge_pull_request":
		result = mcpCallMergePullRequest(s, args)
	default:
		return mcpToolResult{}, -32602, "Unknown tool: " + name, false
	}
	return result, 0, "", true
}

// ---------------------------------------------------------------------------
// Protocol methods.
// ---------------------------------------------------------------------------

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpServerInfo = map[string]any{"name": "forge", "version": "1.0.0"}

func mcpDiscoverResult() map[string]any {
	return map[string]any{
		"supportedVersions": mcpSupportedVersions,
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        mcpServerInfo,
		"instructions":      "GitHub-compatible git forge: repositories, issues, pull requests, and git over MCP.",
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": mcpServerInfo,
		},
	}
}

func mcpInitializeResult(protocolVersion string) map[string]any {
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}
	accepted := false
	for _, v := range mcpSupportedVersions[1:] { // legacy revisions only
		if v == protocolVersion {
			accepted = true
			break
		}
	}
	if !accepted {
		protocolVersion = "2025-11-25"
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      mcpServerInfo,
	}
}

// ---------------------------------------------------------------------------
// HTTP wiring.
// ---------------------------------------------------------------------------

type mcpRawRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func registerMCP(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			// Legacy session close (client.close()): nothing held.
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
			// handled below
		default:
			methodNotAllowed(w, s)
			return
		}
		if !s.requireAuth(w, r) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			mcpWriteJSON(w, s, 400, mcpError(nil, -32700, "Parse error"))
			return
		}
		var raw mcpRawRequest
		if err := json.Unmarshal(body, &raw); err != nil {
			// Body is not JSON: HTTP 400 with JSON-RPC -32700.
			mcpWriteJSON(w, s, 400, mcpError(nil, -32700, "Parse error"))
			return
		}
		if raw.Method == "" {
			mcpWriteJSON(w, s, 400, mcpError(raw.ID, -32600, "Invalid Request"))
			return
		}
		// Notifications carry no id: acknowledge with 202 and no body.
		if raw.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var params map[string]any
		if len(raw.Params) > 0 {
			_ = json.Unmarshal(raw.Params, &params)
		}
		if params == nil {
			params = map[string]any{}
		}
		switch raw.Method {
		case "server/discover":
			mcpWriteJSON(w, s, 200, mcpResult(raw.ID, mcpDiscoverResult()))
		case "initialize":
			pv, _ := params["protocolVersion"].(string)
			mcpWriteJSON(w, s, 200, mcpResult(raw.ID, mcpInitializeResult(pv)))
		case "ping":
			mcpWriteJSON(w, s, 200, mcpResult(raw.ID, map[string]any{}))
		case "tools/list":
			// Pre-marshalled tools result: byte-identical across calls.
			resp := append([]byte(nil), []byte(`{"jsonrpc":"2.0","id":`)...)
			resp = append(resp, raw.ID...)
			resp = append(resp, []byte(`,"result":`)...)
			resp = append(resp, mcpToolsResultJSON...)
			resp = append(resp, '}')
			mcpWriteRaw(w, s, 200, resp)
		case "tools/call":
			name, _ := params["name"].(string)
			var args map[string]any
			if a, ok := params["arguments"].(map[string]any); ok {
				args = a
			}
			result, code, msg, ok := mcpDispatchTool(s, name, args)
			if !ok {
				mcpWriteJSON(w, s, 200, mcpError(raw.ID, code, msg))
				return
			}
			mcpWriteJSON(w, s, 200, mcpResult(raw.ID, result))
		default:
			if strings.HasPrefix(raw.Method, "notifications/") {
				mcpWriteJSON(w, s, 200, mcpResult(raw.ID, map[string]any{}))
				return
			}
			mcpWriteJSON(w, s, 200, mcpError(raw.ID, -32601, "Method not found: "+raw.Method))
		}
	})
}
