// server.go — Agent B: HTTP server, router, auth, JSON helpers, NewHandler.
//
// Translation of gh-p4ab/app.ts (createApp + sendJson/err/authorized/
// pageParams/paginate/linkHeader). The router uses MANUAL dispatch (no
// http.ServeMux): Go's mux path cleaning/redirects differ from Node's
// behaviour, and the suite's wire contract is defined by the Node code.
//
// Request-id rule (Node): one counter per process, incremented once per
// incoming request, first request = req-1. Every response carries it in
// X-GitHub-Request-Id. ServeHTTP assigns the id, wraps the writer, and
// pre-sets the header so every downstream writer (REST, git, GraphQL,
// MCP) carries it automatically.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// shopToken is the bearer this process expects ("" = any non-empty bearer),
// read once at startup like Node's `const SHOP_TOKEN`.
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

// reqSeq is the package-level request-id counter (starts at 0; the first
// request increments it to 1, so the first id is "req-1", like Node).
var reqSeq atomic.Int64

// restMu serialises REST handling. The Store type carries no lock and Node
// is single-threaded; the suite drives REST sequentially, so one mutex
// around handleRest keeps map access safe without touching git/GraphQL/MCP
// throughput (the clone workload is concurrent git, not REST).
var restMu sync.Mutex

// nextRequestID returns "req-<n>" from the atomic counter.
func nextRequestID() string {
	return "req-" + strconv.FormatInt(reqSeq.Add(1), 10)
}

// idWriter carries the request id assigned in ServeHTTP. Header() is the
// underlying header map, so the id pre-set there rides on every response
// written through it, including git/GraphQL/MCP responses.
type idWriter struct {
	http.ResponseWriter
	id string
}

// Flush forwards to the underlying writer when it supports flushing
// (needed by SSE responses); without it the wrapper would hide Flusher.
func (w *idWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// writerReqID returns the request id for a response writer: the id baked
// into an idWriter, or a fresh counter value for a bare writer (tests).
func writerReqID(w http.ResponseWriter) string {
	if iw, ok := w.(*idWriter); ok {
		return iw.id
	}
	return nextRequestID()
}

// writeJSONExtra renders v as JSON + "\n" (Node: JSON.stringify(v) + "\n",
// SetEscapeHTML(false) to match byte-for-byte), with content-type
// application/json; charset=utf-8, X-GitHub-Request-Id and content-length.
// extra headers are set when non-empty (Link, X-OAuth-Scopes).
func writeJSONExtra(w http.ResponseWriter, status int, v any, extra map[string]string) {
	var buf bytes.Buffer
	if v != nil {
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v) // appends "\n", like sendJson
	}
	h := w.Header()
	h.Set("Content-Type", JSONType)
	if h.Get("X-GitHub-Request-Id") == "" {
		h.Set("X-GitHub-Request-Id", writerReqID(w))
	}
	for k, val := range extra {
		if val != "" {
			h.Set(k, val)
		}
	}
	if buf.Len() > 0 {
		h.Set("Content-Length", strconv.Itoa(buf.Len()))
	}
	w.WriteHeader(status)
	if buf.Len() > 0 {
		_, _ = w.Write(buf.Bytes())
	}
}

// writeJSON renders v as JSON + "\n" with the standard JSON headers.
func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONExtra(w, status, v, nil)
}

// writeJSONBytes writes pre-rendered bytes (jsonCache) as-is — no trailing
// newline added — with the same headers. This is how GET repo / POST issue /
// GET issue answer: the Node cache holds JSON.stringify output verbatim.
func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", JSONType)
	if h.Get("X-GitHub-Request-Id") == "" {
		h.Set("X-GitHub-Request-Id", writerReqID(w))
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// jsonError writes {"message": msg} (with trailing "\n", via writeJSON).
func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorView{Message: msg})
}

// marshalJSONNoNL renders v exactly like JSON.stringify (no trailing
// newline): the cached repo/issue payloads. Encoder.Encode appends "\n",
// so trim exactly one.
func marshalJSONNoNL(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// checkAuth mirrors Node's authorized(): "Bearer <token>" must equal
// $MAKEFUN_SHOP_TOKEN; when that env is empty any non-empty bearer passes.
func checkAuth(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := h[len("Bearer "):]
	if shopToken != "" {
		return token == shopToken
	}
	return token != ""
}

// jsParseInt mimics JavaScript parseInt(s, 10): leading whitespace, an
// optional sign, then digits until the first non-digit. ok=false when no
// digits were found. Over-long digit runs saturate (Node returns a large
// float); callers that need safe-integer semantics check the bound.
func jsParseInt(s string) (int64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var n int64
	const sat = int64(1) << 62
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		d := int64(s[i] - '0')
		if n > (sat-d)/10 {
			n = sat
		} else {
			n = n*10 + d
		}
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// pageParams mirrors Node: page default 1 min 1 (0/NaN fall back to 1);
// per_page default 30 clamp 1..100 (0/NaN fall back to 30).
func pageParams(r *http.Request) (page, perPage int) {
	q := r.URL.Query()
	if v, ok := jsParseInt(q.Get("page")); ok && v != 0 {
		page = int(v)
	} else {
		page = 1
	}
	if page < 1 {
		page = 1
	}
	if v, ok := jsParseInt(q.Get("per_page")); ok && v != 0 {
		perPage = int(v)
	} else {
		perPage = 30
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// paginate slices a list into one page. Overflow-safe: a page past the end
// returns an empty (non-nil) slice, never a panic.
func paginate[T any](list []T, page, perPage int) []T {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 1
	}
	total := len(list)
	if total == 0 {
		return []T{}
	}
	pages := (total + perPage - 1) / perPage
	if page > pages {
		return []T{}
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > total {
		end = total
	}
	return list[start:end]
}

// linkHeader returns the absolute next+last Link value, only when more
// pages follow; "" otherwise (Node returns {} in that case, i.e. no header).
func linkHeader(r *http.Request, page, perPage, total int) string {
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page >= totalPages {
		return ""
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	base := "http://" + host + requestPath(r)
	next := base + "?page=" + strconv.Itoa(page+1) + "&per_page=" + strconv.Itoa(perPage)
	last := base + "?page=" + strconv.Itoa(totalPages) + "&per_page=" + strconv.Itoa(perPage)
	return "<" + next + ">; rel=\"next\", <" + last + ">; rel=\"last\""
}

// parseQuery parses a raw query string like URLSearchParams: split on '&',
// first value wins, '+' decodes to space, %XX decoded (kept raw on error).
func parseQuery(raw string) map[string]string {
	m := map[string]string{}
	if raw == "" {
		return m
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return m
}

// splitSegments splits a path on "/" ignoring empty segments
// (Node: split('/').filter(x => x !== ”)).
func splitSegments(s string) []string {
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// removeDotSegments resolves "." and ".." segments (RFC 3986), matching
// what Node's `new URL(rawUrl, ...)` fallback does for odd request targets.
func removeDotSegments(p string) string {
	segs := strings.Split(p, "/")
	out := []string{}
	for _, s := range segs {
		switch s {
		case ".":
			// drop
		case "..":
			if len(out) > 1 || (len(out) == 1 && out[0] != "") {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	res := strings.Join(out, "/")
	if !strings.HasPrefix(res, "/") {
		res = "/" + res
	}
	return res
}

// requestPath returns the request path exactly as Node's router sees it:
// the raw (still %-encoded) path, dot segments resolved, one trailing
// slash stripped (when len > 1). Go's net/http does not clean the path
// before the handler runs, so EscapedPath is the raw request-target path.
func requestPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if p == "" {
		p = "/"
	}
	if strings.Contains(p, "/./") || strings.Contains(p, "/../") ||
		strings.HasSuffix(p, "/.") || strings.HasSuffix(p, "/..") {
		p = removeDotSegments(p)
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	if p == "" {
		p = "/"
	}
	return p
}

// restCtx is the per-request context for the REST surface (Node's Ctx).
type restCtx struct {
	w        http.ResponseWriter
	r        *http.Request
	store    *Store
	method   string
	path     string
	query    map[string]string
	segments []string
	json     map[string]any // nil when the body was not JSON-parsed (GET) or unparsable
	jsonBad  bool
	seq      string
}

// needAuth writes 401 {"message":"Requires authentication"} unless the
// request carries a valid bearer; true when authorised.
func needAuth(c *restCtx) bool {
	if checkAuth(c.r) {
		return true
	}
	jsonError(c.w, 401, "Requires authentication")
	return false
}

// forgeHandler is the top-level handler: manual dispatch, no ServeMux.
type forgeHandler struct {
	store *Store
}

// NewHandler builds the HTTP handler for a store: /healthz, /api/v3/*,
// /api/graphql -> ServeGraphQL, /mcp -> ServeMCP, /{owner}/{repo}.git/*
// -> ServeGit. Anything else is a JSON 404.
func NewHandler(s *Store) http.Handler {
	return &forgeHandler{store: s}
}

func (h *forgeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	seq := nextRequestID()
	iw := &idWriter{ResponseWriter: w, id: seq}
	iw.Header().Set("X-GitHub-Request-Id", seq)

	body, _ := io.ReadAll(r.Body)
	// Handlers downstream (GraphQL/MCP/git) re-read the body themselves.
	r.Body = io.NopCloser(bytes.NewReader(body))

	path := requestPath(r)
	method := r.Method
	query := parseQuery(r.URL.RawQuery)

	// Only the JSON surfaces get a JSON parse: git smart HTTP posts
	// binary packs. Empty body => json={} (non-nil); a non-object JSON
	// value also yields json={}; unparsable => jsonBad.
	isJSONPath := strings.HasPrefix(path, "/api/") || path == "/mcp"
	needsJSON := isJSONPath && (method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE")
	var jsonObj map[string]any
	jsonBad := false
	if needsJSON {
		if len(body) == 0 {
			jsonObj = map[string]any{}
		} else {
			var parsed any
			if err := json.Unmarshal(body, &parsed); err != nil {
				jsonBad = true
			} else if m, ok := parsed.(map[string]any); ok {
				jsonObj = m
			} else {
				jsonObj = map[string]any{}
			}
		}
	}

	ctx := &restCtx{
		w: iw, r: r, store: h.store,
		method: method, path: path, query: query,
		json: jsonObj, jsonBad: jsonBad, seq: seq,
	}

	// GET /healthz -> 200 {"ok":true}, no auth. (Checked before jsonBad,
	// like Node; a GET can never be jsonBad anyway.)
	if method == "GET" && path == "/healthz" {
		writeJSON(iw, 200, okView{OK: true})
		return
	}

	// Malformed JSON on a JSON surface: 400 before routing/auth. MCP
	// answers the JSON-RPC parse error; everything else gets the plain
	// "Problems parsing JSON" message.
	if jsonBad {
		if path == "/mcp" {
			writeJSON(iw, 400, parseErrorView{JSONRPC: "2.0", ID: nil, Error: rpcErrorView{Code: -32700, Message: "Parse error"}})
			return
		}
		jsonError(iw, 400, "Problems parsing JSON")
		return
	}

	// REST v3.
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if path == "/api/v3" {
			ctx.segments = []string{}
		} else {
			ctx.segments = splitSegments(path[len("/api/v3/"):])
		}
		restMu.Lock()
		handleRest(ctx)
		restMu.Unlock()
		return
	}

	// GraphQL: POST only, auth first.
	if path == "/api/graphql" && method == "POST" {
		if !needAuth(ctx) {
			return
		}
		ServeGraphQL(h.store, iw, r)
		return
	}

	// MCP: POST only, auth first.
	if path == "/mcp" && method == "POST" {
		if !needAuth(ctx) {
			return
		}
		ServeMCP(h.store, iw, r)
		return
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	// Node splits the raw path and requires >= 2 segments with the second
	// ending in ".git". Auth first, then the repo lookup (a repo under
	// another owner is 404, never 403; a missing repo is the same JSON
	// 404 gitError returns, so answering it here is byte-identical).
	gitSegs := splitSegments(path)
	if len(gitSegs) >= 2 && strings.HasSuffix(gitSegs[1], ".git") {
		if !needAuth(ctx) {
			return
		}
		owner := decodeSeg(gitSegs[0])
		name := decodeSeg(gitSegs[1][:len(gitSegs[1])-4])
		var repo *Repo
		if owner == ViewerLogin {
			repo = h.store.GetRepo(name)
		}
		if repo == nil {
			jsonError(iw, 404, "Not Found")
			return
		}
		// Node's handleGit returns null (=> JSON 404) for anything that
		// is not info/refs, git-upload-pack or git-receive-pack, and for
		// a bare /{owner}/{repo}.git with no sub-path.
		if len(gitSegs) < 3 {
			jsonError(iw, 404, "Not Found")
			return
		}
		switch gitSegs[2] {
		case "git-upload-pack", "git-receive-pack":
			ServeGit(h.store, repo, iw, r)
		case "info":
			if len(gitSegs) >= 4 && gitSegs[3] == "refs" {
				ServeGit(h.store, repo, iw, r)
			} else {
				jsonError(iw, 404, "Not Found")
			}
		default:
			jsonError(iw, 404, "Not Found")
		}
		return
	}

	// Any other path is an unknown route: JSON 404, never HTML.
	jsonError(iw, 404, "Not Found")
}
