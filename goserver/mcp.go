// mcp.go — Model Context Protocol over Streamable HTTP at /mcp, speaking the
// modern era 2026-07-28 (server/discover, no handshake, no session) with the
// legacy initialize + notifications/initialized handshake as a fallback.
// JSON-RPC 2.0, plain JSON answers (no SSE needed), one process, the same
// in-memory store REST and GraphQL already share.
//
// 1:1 translation of gh-p4ab/mcp.ts (A+B @64dd411). Field order in every
// response object matches Node's JSON.stringify insertion order (structs,
// not maps, so encoding/json emits the same bytes).
//
// ServeMCP owns the whole /mcp surface: it reads the request body itself
// (the router must NOT consume it first), checks the JSON parse before auth
// (Node precedence: bad JSON + no auth answers 400 Parse error, not 401),
// then dispatches. Notifications answer 202 with an empty body.
//
// Concurrency note (deliberate deviation from Node): Node is single-threaded;
// Go serves requests concurrently, and the Store has no lock, so ServeMCP
// serializes dispatch through mcpMu. Sequential behaviour and bytes are
// unchanged; this only keeps two in-flight tools/call requests from racing
// on the same maps (the suite explicitly checks two in-flight calls keep
// their own JSON-RPC ids).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const mcpServerName = "gh-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// mcpMu serializes MCP dispatch (see package note above).
var mcpMu sync.Mutex

// ---------------------------------------------------------------- JSON helpers

// marshalNoEscape renders v exactly like Node's JSON.stringify: no HTML
// escaping, no trailing newline.
func marshalNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// ---------------------------------------------------------------- tool definitions (built once, reused verbatim)

type mcpProp struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// mcpToolProps holds every property any of the six tools uses. Field order
// is the one global order consistent with all six Node definitions:
// create_repo: name, description, private; get_repo: owner, repo;
// create_issue: owner, repo, title, body; list_issues: owner, repo, state;
// create_pull_request: owner, repo, title, head, base;
// merge_pull_request: owner, repo, pull_number.
type mcpToolProps struct {
	Name        *mcpProp `json:"name,omitempty"`
	Description *mcpProp `json:"description,omitempty"`
	Private     *mcpProp `json:"private,omitempty"`
	Owner       *mcpProp `json:"owner,omitempty"`
	Repo        *mcpProp `json:"repo,omitempty"`
	Title       *mcpProp `json:"title,omitempty"`
	Body        *mcpProp `json:"body,omitempty"`
	State       *mcpProp `json:"state,omitempty"`
	Head        *mcpProp `json:"head,omitempty"`
	Base        *mcpProp `json:"base,omitempty"`
	PullNumber  *mcpProp `json:"pull_number,omitempty"`
}

type mcpInputSchema struct {
	Type       string       `json:"type"`
	Properties mcpToolProps `json:"properties"`
	Required   []string     `json:"required"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

func mcpPropDef(typ, desc string, enum ...string) *mcpProp {
	p := &mcpProp{Type: typ, Description: desc}
	if len(enum) > 0 {
		p.Enum = enum
	}
	return p
}

func mcpTool(name, desc string, props mcpToolProps, required []string) mcpToolDef {
	return mcpToolDef{
		Name:        name,
		Description: desc,
		InputSchema: mcpInputSchema{Type: "object", Properties: props, Required: required},
	}
}

var mcpTools = []mcpToolDef{
	mcpTool("create_repo", "Create a repository for the authenticated user.", mcpToolProps{
		Name:        mcpPropDef("string", "The repository name."),
		Description: mcpPropDef("string", "A short description of the repository."),
		Private:     mcpPropDef("boolean", "Whether the repository is private."),
	}, []string{"name"}),
	mcpTool("get_repo", "Get a repository by owner and name.", mcpToolProps{
		Owner: mcpPropDef("string", "The repository owner login."),
		Repo:  mcpPropDef("string", "The repository name."),
	}, []string{"owner", "repo"}),
	mcpTool("create_issue", "Create an issue in a repository.", mcpToolProps{
		Owner: mcpPropDef("string", "The repository owner login."),
		Repo:  mcpPropDef("string", "The repository name."),
		Title: mcpPropDef("string", "The issue title."),
		Body:  mcpPropDef("string", "The issue body."),
	}, []string{"owner", "repo", "title"}),
	mcpTool("list_issues", "List the issues of a repository.", mcpToolProps{
		Owner: mcpPropDef("string", "The repository owner login."),
		Repo:  mcpPropDef("string", "The repository name."),
		State: mcpPropDef("string", "open, closed, or all.", "open", "closed", "all"),
	}, []string{"owner", "repo"}),
	mcpTool("create_pull_request", "Open a pull request from a head branch into a base branch.", mcpToolProps{
		Owner: mcpPropDef("string", "The repository owner login."),
		Repo:  mcpPropDef("string", "The repository name."),
		Title: mcpPropDef("string", "The pull request title."),
		Head:  mcpPropDef("string", "The head branch name."),
		Base:  mcpPropDef("string", "The base branch name."),
	}, []string{"owner", "repo", "title", "head", "base"}),
	mcpTool("merge_pull_request", "Merge a pull request by number (squash).", mcpToolProps{
		Owner:      mcpPropDef("string", "The repository owner login."),
		Repo:       mcpPropDef("string", "The repository name."),
		PullNumber: mcpPropDef("integer", "The pull request number."),
	}, []string{"owner", "repo", "pull_number"}),
}

// ---------------------------------------------------------------- REST-shaped objects (same fields REST returns)

type mcpOwnerObj struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

func mcpViewer() mcpOwnerObj {
	return mcpOwnerObj{Login: ViewerLogin, ID: ViewerID, Type: "User"}
}

type mcpRepoObj struct {
	ID            int         `json:"id"`
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	Owner         mcpOwnerObj `json:"owner"`
	Private       bool        `json:"private"`
	DefaultBranch string      `json:"default_branch"`
	Description   string      `json:"description"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
	HTMLURL       string      `json:"html_url"`
	CloneURL      string      `json:"clone_url"`
	HasIssues     bool        `json:"has_issues"`
}

func mcpRepoObject(repo *Repo) mcpRepoObj {
	return mcpRepoObj{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         mcpOwnerObj{Login: repo.Owner, ID: ViewerID, Type: "User"},
		Private:       repo.IsPrivate,
		DefaultBranch: repo.DefaultBranch,
		Description:   repo.Description,
		CreatedAt:     NowISO,
		UpdatedAt:     NowISO,
		HTMLURL:       "https://github.com/" + repo.Owner + "/" + repo.Name,
		CloneURL:      "https://github.com/" + repo.Owner + "/" + repo.Name + ".git",
		HasIssues:     true,
	}
}

type mcpLabelObj struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type mcpIssueObj struct {
	ID        int           `json:"id"`
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	State     string        `json:"state"`
	Labels    []mcpLabelObj `json:"labels"`
	User      mcpOwnerObj   `json:"user"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	HTMLURL   string        `json:"html_url"`
}

func mcpIssueObject(repo *Repo, issue *Issue) mcpIssueObj {
	labels := make([]mcpLabelObj, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label, ok := repo.Labels[name]; ok {
			labels = append(labels, mcpLabelObj{Name: label.Name, Color: label.Color, Description: ""})
		} else {
			labels = append(labels, mcpLabelObj{Name: name, Color: "ededed", Description: ""})
		}
	}
	return mcpIssueObj{
		ID:        issue.ID,
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      mcpViewer(),
		CreatedAt: NowISO,
		UpdatedAt: NowISO,
		HTMLURL:   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

type mcpRefObj struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type mcpPullObj struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Head           mcpRefObj   `json:"head"`
	Base           mcpRefObj   `json:"base"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSHA *string     `json:"merge_commit_sha"`
	Mergeable      bool        `json:"mergeable"`
	User           mcpOwnerObj `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	HTMLURL        string      `json:"html_url"`
}

func mcpPullObject(s *Store, repo *Repo, pull *Pull) mcpPullObj {
	headSHA, ok := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok {
		headSHA = pull.HeadSHA
	}
	baseSHA, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		baseSHA = pull.BaseSHA
	}
	return mcpPullObj{
		ID:             pull.ID,
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Head:           mcpRefObj{Ref: pull.HeadRef, SHA: headSHA, Label: repo.Owner + ":" + pull.HeadRef},
		Base:           mcpRefObj{Ref: pull.BaseRef, SHA: baseSHA, Label: repo.Owner + ":" + pull.BaseRef},
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSHA: pull.MergeCommitSHA,
		Mergeable:      !pull.Merged && pull.State == "open" && !s.PullConflicted(repo, pull),
		User:           mcpViewer(),
		CreatedAt:      NowISO,
		UpdatedAt:      NowISO,
		HTMLURL:        "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
	}
}

// ---------------------------------------------------------------- tool results

type mcpTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpTextBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

func mcpOKResult(value any) mcpToolResult {
	return mcpToolResult{
		Content:           []mcpTextBlock{{Type: "text", Text: mcpTextOf(value)}},
		StructuredContent: value,
	}
}

func mcpErrorResult(message string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpTextBlock{{Type: "text", Text: message}},
		IsError: true,
	}
}

// mcpTextOf mirrors Node's textOf: a bare string stays itself, anything else
// is JSON.stringify'd (no trailing newline).
func mcpTextOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return marshalNoEscape(value)
}

// ---------------------------------------------------------------- arguments

func mcpArgString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func mcpAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func mcpArgNumber(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return int(i), true
		}
		if f, err := strconv.ParseFloat(string(n), 64); err == nil && f == math.Trunc(f) {
			return int(f), true
		}
		return 0, false
	case float64:
		if n == math.Trunc(n) {
			return int(n), true
		}
		return 0, false
	case string:
		if !mcpAllDigits(n) {
			return 0, false
		}
		i, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

func mcpOpenRepo(s *Store, args map[string]any) *Repo {
	owner, ok := mcpArgString(args, "owner")
	if !ok {
		return nil
	}
	name, ok := mcpArgString(args, "repo")
	if !ok {
		return nil
	}
	if owner != ViewerLogin {
		return nil
	}
	return s.GetRepo(name)
}

// ---------------------------------------------------------------- the six tools

func mcpCallCreateRepo(s *Store, args map[string]any) mcpToolResult {
	name, ok := mcpArgString(args, "name")
	if !ok {
		return mcpErrorResult("create_repo: missing required argument: name")
	}
	if s.GetRepo(name) != nil {
		return mcpErrorResult("create_repo: repository " + name + " already exists")
	}
	description := ""
	if d, ok := args["description"].(string); ok {
		description = d
	}
	isPrivate := args["private"] == true
	repo := s.CreateRepo(name, isPrivate, description)
	return mcpOKResult(mcpRepoObject(repo))
}

func mcpCallGetRepo(s *Store, args map[string]any) mcpToolResult {
	repo := mcpOpenRepo(s, args)
	if repo == nil {
		return mcpErrorResult("get_repo: repository not found")
	}
	return mcpOKResult(mcpRepoObject(repo))
}

func mcpCallCreateIssue(s *Store, args map[string]any) mcpToolResult {
	repo := mcpOpenRepo(s, args)
	if repo == nil {
		return mcpErrorResult("create_issue: repository not found")
	}
	title, ok := mcpArgString(args, "title")
	if !ok {
		return mcpErrorResult("create_issue: missing required argument: title")
	}
	body := ""
	if b, ok := args["body"].(string); ok {
		body = b
	}
	issue := s.CreateIssue(repo, title, body)
	return mcpOKResult(mcpIssueObject(repo, issue))
}

type mcpIssuesList struct {
	Issues []mcpIssueObj `json:"issues"`
}

func mcpCallListIssues(s *Store, args map[string]any) mcpToolResult {
	repo := mcpOpenRepo(s, args)
	if repo == nil {
		return mcpErrorResult("list_issues: repository not found")
	}
	state := "open"
	if v, ok := args["state"].(string); ok {
		state = v
	}
	list := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		if state == "all" || issue.State == state {
			list = append(list, issue)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
	rendered := make([]mcpIssueObj, 0, len(list))
	for _, issue := range list {
		rendered = append(rendered, mcpIssueObject(repo, issue))
	}
	return mcpOKResult(mcpIssuesList{Issues: rendered})
}

func mcpCallCreatePullRequest(s *Store, args map[string]any) mcpToolResult {
	repo := mcpOpenRepo(s, args)
	if repo == nil {
		return mcpErrorResult("create_pull_request: repository not found")
	}
	title, ok := mcpArgString(args, "title")
	if !ok {
		return mcpErrorResult("create_pull_request: missing required argument: title")
	}
	head, ok := mcpArgString(args, "head")
	if !ok {
		return mcpErrorResult("create_pull_request: missing required argument: head")
	}
	base, ok := mcpArgString(args, "base")
	if !ok {
		return mcpErrorResult("create_pull_request: missing required argument: base")
	}
	pull := s.CreatePull(repo, title, "", head, base)
	if pull == nil {
		return mcpErrorResult("create_pull_request: head or base branch does not exist, or a pull request already exists")
	}
	return mcpOKResult(mcpPullObject(s, repo, pull))
}

func mcpCallMergePullRequest(s *Store, args map[string]any) mcpToolResult {
	repo := mcpOpenRepo(s, args)
	if repo == nil {
		return mcpErrorResult("merge_pull_request: repository not found")
	}
	number, ok := mcpArgNumber(args, "pull_number")
	if !ok {
		return mcpErrorResult("merge_pull_request: missing required argument: pull_number")
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		return mcpErrorResult(fmt.Sprintf("merge_pull_request: pull request #%d not found", number))
	}
	if !s.MergePull(repo, pull) {
		return mcpErrorResult(fmt.Sprintf("merge_pull_request: pull request #%d is not mergeable", number))
	}
	return mcpOKResult(mcpPullObject(s, repo, pull))
}

func mcpCallTool(s *Store, name string, args map[string]any) (mcpToolResult, bool) {
	switch name {
	case "create_repo":
		return mcpCallCreateRepo(s, args), true
	case "get_repo":
		return mcpCallGetRepo(s, args), true
	case "create_issue":
		return mcpCallCreateIssue(s, args), true
	case "list_issues":
		return mcpCallListIssues(s, args), true
	case "create_pull_request":
		return mcpCallCreatePullRequest(s, args), true
	case "merge_pull_request":
		return mcpCallMergePullRequest(s, args), true
	default:
		return mcpToolResult{}, false
	}
}

// ---------------------------------------------------------------- JSON-RPC plumbing

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools map[string]any `json:"tools"`
}

type mcpDiscoverMeta struct {
	ServerInfo mcpServerInfo `json:"io.modelcontextprotocol/serverInfo"`
}

type mcpDiscoverResult struct {
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      mcpCapabilities `json:"capabilities"`
	ServerInfo        mcpServerInfo   `json:"serverInfo"`
	Instructions      string          `json:"instructions"`
	Meta              mcpDiscoverMeta `json:"_meta"`
}

type mcpInitResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
}

type mcpToolsListResult struct {
	Tools []mcpToolDef `json:"tools"`
}

type mcpRPCResult struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result"`
}

type mcpRPCErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpRPCError struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Error   mcpRPCErrorObj `json:"error"`
}

func mcpRPCResultValue(id any, result any) mcpRPCResult {
	return mcpRPCResult{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpRPCErrorValue(id any, code int, message string) mcpRPCError {
	return mcpRPCError{JSONRPC: "2.0", ID: id, Error: mcpRPCErrorObj{Code: code, Message: message}}
}

func mcpServerInfoValue() mcpServerInfo {
	return mcpServerInfo{Name: mcpServerName, Version: mcpServerVersion}
}

func mcpCapabilitiesValue() mcpCapabilities {
	return mcpCapabilities{Tools: map[string]any{}}
}

func mcpDiscoverResultValue() mcpDiscoverResult {
	info := mcpServerInfoValue()
	return mcpDiscoverResult{
		SupportedVersions: append([]string(nil), mcpSupportedVersions...),
		Capabilities:      mcpCapabilitiesValue(),
		ServerInfo:        info,
		Instructions:      "A GitHub-compatible git forge: create repositories, issues and pull requests over MCP tools.",
		Meta:              mcpDiscoverMeta{ServerInfo: info},
	}
}

// mcpExtractID mirrors Node: a string or number id is kept, anything else
// (missing, null, bool, object, array) becomes null. Numbers keep their
// integer form so they re-encode exactly (id 1 answers 1, not 1.0).
func mcpExtractID(body map[string]any) any {
	raw, ok := body["id"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return v
	case json.Number:
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return f
		}
		return nil
	case float64:
		return v
	default:
		return nil
	}
}

// mcpRecord returns body.params (or any named field) as a record, or an
// empty record when it is missing or not an object — exactly like Node's
// `isRecord(body.params) ? body.params : {}`.
func mcpRecord(body map[string]any, key string) map[string]any {
	if v, ok := body[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// handleMCPRequest answers one parsed POST /mcp body. It returns the HTTP
// status, the value to JSON-encode, and empty=true for a notification (202
// with an empty body, no JSON). A body that is not a JSON-RPC object is an
// invalid request.
func handleMCPRequest(s *Store, body any) (status int, value any, empty bool) {
	rec, ok := body.(map[string]any)
	if !ok {
		return 200, mcpRPCErrorValue(nil, -32600, "Invalid Request"), false
	}
	methodRaw, ok := rec["method"]
	if !ok {
		return 200, mcpRPCErrorValue(mcpExtractID(rec), -32600, "Invalid Request"), false
	}
	method, ok := methodRaw.(string)
	if !ok {
		return 200, mcpRPCErrorValue(mcpExtractID(rec), -32600, "Invalid Request"), false
	}
	id := mcpExtractID(rec)

	switch {
	case method == "server/discover":
		return 200, mcpRPCResultValue(id, mcpDiscoverResultValue()), false
	case method == "initialize":
		params := mcpRecord(rec, "params")
		offered, _ := params["protocolVersion"].(string)
		negotiated := mcpSupportedVersions[0]
		for _, v := range mcpSupportedVersions {
			if offered != "" && offered == v {
				negotiated = v
				break
			}
		}
		return 200, mcpRPCResultValue(id, mcpInitResult{
			ProtocolVersion: negotiated,
			Capabilities:    mcpCapabilitiesValue(),
			ServerInfo:      mcpServerInfoValue(),
		}), false
	case method == "notifications/initialized" || strings.HasPrefix(method, "notifications/"):
		return 202, nil, true
	case method == "ping":
		return 200, mcpRPCResultValue(id, map[string]any{}), false
	case method == "tools/list":
		return 200, mcpRPCResultValue(id, mcpToolsListResult{Tools: mcpTools}), false
	case method == "tools/call":
		params := mcpRecord(rec, "params")
		name, _ := params["name"].(string)
		args := mcpRecord(params, "arguments")
		if name == "" {
			return 200, mcpRPCErrorValue(id, -32602, "tools/call: missing tool name"), false
		}
		result, known := mcpCallTool(s, name, args)
		if !known {
			return 200, mcpRPCErrorValue(id, -32602, `tools/call: unknown tool "`+name+`"`), false
		}
		return 200, mcpRPCResultValue(id, result), false
	default:
		return 200, mcpRPCErrorValue(id, -32601, "Method not found: "+method), false
	}
}

// ServeMCP serves POST /mcp only; JSON-RPC 2.0 Streamable HTTP. It reads the
// request body itself — the router must not consume it first. JSON parse
// errors answer 400 {-32700} before auth is checked (Node precedence); a
// missing/invalid bearer answers 401 {"message":"Requires authentication"};
// any other method answers 404 {"message":"Not Found"} (Node fallthrough).
func ServeMCP(s *Store, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusNotFound, "Not Found")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, mcpRPCErrorValue(nil, -32700, "Parse error"))
		return
	}
	var bodyParsed any
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&bodyParsed); err != nil {
			writeJSON(w, http.StatusBadRequest, mcpRPCErrorValue(nil, -32700, "Parse error"))
			return
		}
		// Trailing data after the first JSON value is a parse error too
		// (Node's JSON.parse rejects it).
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, mcpRPCErrorValue(nil, -32700, "Parse error"))
			return
		}
	}
	if !checkAuth(r) {
		jsonError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	mcpMu.Lock()
	status, value, empty := handleMCPRequest(s, bodyParsed)
	mcpMu.Unlock()
	if empty {
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, value)
}
