// graphql.go — Agent C: the GraphQL v4 API at POST /api/graphql.
//
// A hand-written executor for the documents gh and github-mcp-server send:
// a tokenizer, a recursive-descent parser, a validator against a fixed
// schema table, and resolvers over the one store REST and git share. No
// GraphQL library (none exists in the Go stdlib); translated 1:1 from
// gh-p4ab/graphql.ts (A+B @64dd411) + the HTTP envelope in gh-p4ab/app.ts
// (lines 1088-1107), so responses are byte-identical to Node's.
//
// The 18 documents (kit sdk/conformance/checks-graphql.ts GRAPHQL_DOCUMENTS):
//   viewerLogin, repositoryFields, issueFields, typeFields,
//   searchTypeEnumValues, openIssues, issueByNumber, createIssue, closeIssue,
//   pullRequests, pullRequestFiles, pullRequestId, createPullRequest,
//   mergePullRequest, addComment, invalidField, nodeRepository, issuesPage.
//
// Determinism: no wall clock, no randomness; map iteration that reaches
// output is sorted first (issues/pulls are sorted by createdSeq); all
// timestamps come from NowISO (unused here — this surface exposes none).

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- JSON values
//
// Responses are built as ordered objects (Node's JSON.stringify preserves
// insertion order; Go maps sort keys, which would break byte equality), then
// rendered with a writer that matches JSON.stringify exactly for the data
// this surface emits (ASCII): " and \ escaped, \b \f \n \r \t short forms,
// other control chars as \u00xx, non-ASCII passed through as UTF-8.

type gqlValue = any // nil, bool, string, int64, float64, *gqlObj, *gqlRaw, []gqlValue

// gqlObj is an ordered JSON object: keys in insertion order, set() replaces
// an existing key's value in place (Node's Object.assign semantics).
type gqlObj struct {
	keys []string
	vals map[string]gqlValue
}

func newGqlObj() *gqlObj { return &gqlObj{vals: map[string]gqlValue{}} }

func (o *gqlObj) set(k string, v gqlValue) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *gqlObj) get(k string) (gqlValue, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// gqlRaw is a typed object value: its public fields (in Node literal order,
// __typename first) plus the store pointers the resolvers need. The _repo /
// _issue / _pull internals never reach output bytes.
type gqlRaw struct {
	typename string
	fields   *gqlObj
	repo     *Repo
	issue    *Issue
	pull     *Pull
}

func newGqlRaw(typename string) *gqlRaw {
	r := &gqlRaw{typename: typename, fields: newGqlObj()}
	r.fields.set("__typename", typename)
	return r
}

func (r *gqlRaw) get(name string) (gqlValue, bool) { return r.fields.get(name) }

// jsJSONString renders s exactly like Node's JSON.stringify(s) for a string:
// short escapes for " \ \b \f \n \r \t, \u00xx for other control chars,
// everything else (including non-ASCII) as raw UTF-8.
func jsJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[byte(c)>>4])
				b.WriteByte(hex[byte(c)&0xf])
			} else {
				b.WriteRune(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsNumberString renders a float64 exactly like Node's Number::toString
// (which JSON.stringify uses): integers plain, otherwise shortest digits
// with JS's decimal/exponential placement rules.
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "null" // JSON.stringify(NaN) is null; never emitted here, but safe
	}
	if math.IsInf(f, 1) {
		return "null"
	}
	if math.IsInf(f, -1) {
		return "null"
	}
	if f == 0 {
		return "0" // JSON.stringify(-0) is "0"
	}
	neg := f < 0
	abs := math.Abs(f)
	// Shortest round-trip digits via Go, then place them per JS rules.
	// FormatFloat 'e' gives d.dddde±XX with the minimal digits.
	rep := strconv.FormatFloat(abs, 'e', -1, 64)
	// rep looks like "1.2345e+07" or "5e-05".
	ePos := strings.IndexByte(rep, 'e')
	mant := rep[:ePos]
	exp, _ := strconv.Atoi(rep[ePos+1:])
	digits := strings.Replace(mant, ".", "", 1)
	// digits is now the shortest digit string, no dot; value = 0.digits * 10^(exp+1)
	// JS: let n = len(digits), k = exp+1 (position of the decimal point).
	n := len(digits)
	k := exp + 1
	var out string
	switch {
	case k <= n && k >= 1:
		// digits[0:k] ["." digits[k:]]
		if k == n {
			out = digits
		} else {
			out = digits[:k] + "." + digits[k:]
		}
	case k > n && k <= 21:
		out = digits + strings.Repeat("0", k-n)
	case k <= 0 && k > -6:
		out = "0." + strings.Repeat("0", -k) + digits
	default:
		// Exponential: d[.ddd]e±X, exponent without '+' and without leading zeros.
		if n == 1 {
			out = digits + "e" + strconv.Itoa(exp)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + strconv.Itoa(exp)
		}
		// JS prints e+7 for positive exponents in exponential form.
		if exp >= 0 {
			out = strings.Replace(out, "e", "e+", 1)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

// gqlWriteJSON renders v the way Node's JSON.stringify does, into dst.
func gqlWriteJSON(dst *bytes.Buffer, v gqlValue) {
	switch t := v.(type) {
	case nil:
		dst.WriteString("null")
	case bool:
		if t {
			dst.WriteString("true")
		} else {
			dst.WriteString("false")
		}
	case string:
		dst.WriteString(jsJSONString(t))
	case int64:
		dst.WriteString(strconv.FormatInt(t, 10))
	case float64:
		dst.WriteString(jsNumberString(t))
	case *gqlObj:
		dst.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			dst.WriteString(jsJSONString(k))
			dst.WriteByte(':')
			gqlWriteJSON(dst, t.vals[k])
		}
		dst.WriteByte('}')
	case *gqlRaw:
		gqlWriteJSON(dst, t.fields)
	case []gqlValue:
		dst.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				dst.WriteByte(',')
			}
			gqlWriteJSON(dst, e)
		}
		dst.WriteByte(']')
	default:
		// Unreachable for executor values; never emit invalid JSON.
		dst.WriteString("null")
	}
}

func gqlRenderJSON(v gqlValue) []byte {
	var b bytes.Buffer
	gqlWriteJSON(&b, v)
	b.WriteByte('\n')
	return b.Bytes()
}

// gqlParseInt mirrors JS parseInt(s, 10): an optional sign, then the leading
// digit run; no digits at all is NaN.
func gqlParseInt(s string) float64 {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s[start:i], 64)
	if neg {
		return -f
	}
	return f
}

// gqlParseIntToInt is gqlParseInt narrowed to a Go int for map lookups; false
// when there are no leading digits (NaN in JS, which never matches a key).
func gqlParseIntToInt(s string) (int, bool) {
	f := gqlParseInt(s)
	if math.IsNaN(f) {
		return 0, false
	}
	return int(f), true
}

// jsParseFloat mirrors JS parseFloat: the longest valid float prefix of s
// (sign, digits, optional dot + digits, optional exponent); no valid prefix
// is NaN. The tokenizer only feeds digit/dot strings (plus a leading '-').
func jsParseFloat(s string) float64 {
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	startDigits := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i == startDigits {
		return math.NaN()
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '-' || s[j] == '+') {
			j++
		}
		expStart := j
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > expStart {
			i = j
		}
	}
	f, _ := strconv.ParseFloat(s[:i], 64)
	return f
}

// jsString mirrors JS String(v) for the values resolveValue can produce.
// (null/undefined are replaced by ” by the caller's ?? ” before this.)
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return ""
	}
}

// ---------------------------------------------------------------- tokenizer

type gqlTokKind int

const (
	tokName gqlTokKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokSpread
	tokEOF
)

type gqlTok struct {
	kind  gqlTokKind
	value string
}

type gqlThrow struct{ msg string }

func throwGQL(msg string) { panic(gqlThrow{msg}) }

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func tokenize(src string) []gqlTok {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.' {
			toks = append(toks, gqlTok{kind: tokSpread, value: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}():[]!=$@&|", c) >= 0 {
			toks = append(toks, gqlTok{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					case '"':
						out.WriteByte('"')
					case '\\':
						out.WriteByte('\\')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			toks = append(toks, gqlTok{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			// DEVIATION (documented, see report): Node's scan only consumes
			// [0-9.] after the first char, so a leading '-' consumes nothing
			// and the tokenizer loops forever, wedging the whole forge —
			// verified against the Node forge 2026-11-19 (first: -1 hangs it
			// and it never answers again). The suite's 18 documents contain
			// no negative literal, so no observable response changes; consume
			// the leading '-' so a negative literal tokenizes sanely instead.
			if c == '-' {
				j++
			}
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			text := src[i:j]
			kind := tokInt
			if strings.Contains(text, ".") {
				kind = tokFloat
			}
			toks = append(toks, gqlTok{kind: kind, value: text})
			i = j
			continue
		}
		if isNameStart(c) {
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlTok{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		throwGQL("unexpected character " + jsJSONString(string(c)) + " in GraphQL document")
	}
	toks = append(toks, gqlTok{kind: tokEOF, value: ""})
	return toks
}

// ---------------------------------------------------------------- parser

type gqlASTValue struct {
	kind    string // variable|string|int|float|boolean|null|enum|list|object
	str     string
	num     float64
	boolean bool
	items   []*gqlASTValue
	fields  map[string]*gqlASTValue
}

type gqlSelection struct {
	kind          string // field|fragment
	alias         string
	name          string
	args          map[string]*gqlASTValue
	children      []*gqlSelection
	typeCondition string // "" = none
	hasCondition  bool
}

type gqlOperation struct {
	kind       string // query|mutation
	name       string // "" = unnamed
	selections []*gqlSelection
}

type gqlParser struct {
	pos  int
	toks []gqlTok
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expectPunct(value string) {
	tok := p.next()
	if tok.kind != tokPunct || tok.value != value {
		throwGQL("expected '" + value + "' in GraphQL document, got '" + tok.value + "'")
	}
}

func (p *gqlParser) expectName(what string) string {
	tok := p.next()
	if tok.kind != tokName {
		throwGQL("expected " + what + " in GraphQL document, got '" + tok.value + "'")
	}
	return tok.value
}

func (p *gqlParser) parseDocument() []*gqlOperation {
	var ops []*gqlOperation
	for p.peek().kind != tokEOF {
		ops = append(ops, p.parseOperation())
	}
	return ops
}

func (p *gqlParser) parseOperation() *gqlOperation {
	kind := "query"
	first := p.peek()
	if first.kind == tokName && (first.value == "query" || first.value == "mutation") {
		kind = p.next().value
	}
	name := ""
	if p.peek().kind == tokName {
		name = p.next().value
	}
	// Variable definitions: ($name: Type = default, ...) — names and defaults only.
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			p.expectPunct("$")
			p.expectName("a variable name")
			p.expectPunct(":")
			p.parseType()
			if p.peek().kind == tokPunct && p.peek().value == "=" {
				p.next()
				p.parseValue()
			}
		}
		p.expectPunct(")")
	}
	// Directives on the operation (none of the suite's documents use any).
	p.skipDirectives()
	selections := p.parseSelectionSet()
	return &gqlOperation{kind: kind, name: name, selections: selections}
}

func (p *gqlParser) parseType() {
	tok := p.peek()
	if tok.kind == tokPunct && tok.value == "[" {
		p.next()
		p.parseType()
		p.expectPunct("]")
	} else {
		p.expectName("a type name")
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
	}
}

func (p *gqlParser) skipDirectives() {
	for p.peek().kind == tokPunct && p.peek().value == "@" {
		p.next()
		p.expectName("a directive name")
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			p.parseArguments()
		}
	}
}

func (p *gqlParser) parseSelectionSet() []*gqlSelection {
	p.expectPunct("{")
	var out []*gqlSelection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		if p.peek().kind == tokEOF {
			throwGQL("unterminated selection set")
		}
		out = append(out, p.parseSelection())
	}
	p.expectPunct("}")
	return out
}

func (p *gqlParser) parseSelection() *gqlSelection {
	if p.peek().kind == tokSpread {
		p.next()
		typeCondition := ""
		hasCondition := false
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			typeCondition = p.expectName("a type condition")
			hasCondition = true
		} else if p.peek().kind == tokName {
			throwGQL("named fragments are not supported")
		}
		p.skipDirectives()
		return &gqlSelection{kind: "fragment", typeCondition: typeCondition, hasCondition: hasCondition, children: p.parseSelectionSet()}
	}
	first := p.expectName("a field name")
	alias := first
	name := first
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.next()
		name = p.expectName("a field name")
	}
	var args map[string]*gqlASTValue
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		args = p.parseArguments()
	} else {
		args = map[string]*gqlASTValue{}
	}
	p.skipDirectives()
	var children []*gqlSelection
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		children = p.parseSelectionSet()
	}
	return &gqlSelection{kind: "field", alias: alias, name: name, args: args, children: children}
}

func (p *gqlParser) parseArguments() map[string]*gqlASTValue {
	p.expectPunct("(")
	args := map[string]*gqlASTValue{}
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		name := p.expectName("an argument name")
		p.expectPunct(":")
		args[name] = p.parseValue()
	}
	p.expectPunct(")")
	return args
}

func (p *gqlParser) parseValue() *gqlASTValue {
	tok := p.next()
	if tok.kind == tokPunct && tok.value == "$" {
		return &gqlASTValue{kind: "variable", str: p.expectName("a variable name")}
	}
	if tok.kind == tokString {
		return &gqlASTValue{kind: "string", str: tok.value}
	}
	if tok.kind == tokInt {
		return &gqlASTValue{kind: "int", num: gqlParseInt(tok.value)}
	}
	if tok.kind == tokFloat {
		return &gqlASTValue{kind: "float", num: jsParseFloat(tok.value)}
	}
	if tok.kind == tokName {
		if tok.value == "true" {
			return &gqlASTValue{kind: "boolean", boolean: true}
		}
		if tok.value == "false" {
			return &gqlASTValue{kind: "boolean", boolean: false}
		}
		if tok.value == "null" {
			return &gqlASTValue{kind: "null"}
		}
		return &gqlASTValue{kind: "enum", str: tok.value}
	}
	if tok.kind == tokPunct && tok.value == "[" {
		var items []*gqlASTValue
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			items = append(items, p.parseValue())
		}
		p.expectPunct("]")
		return &gqlASTValue{kind: "list", items: items}
	}
	if tok.kind == tokPunct && tok.value == "{" {
		fields := map[string]*gqlASTValue{}
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name := p.expectName("an object field name")
			p.expectPunct(":")
			fields[name] = p.parseValue()
		}
		p.expectPunct("}")
		return &gqlASTValue{kind: "object", fields: fields}
	}
	throwGQL("unexpected value token '" + tok.value + "'")
	return nil
}

// ---------------------------------------------------------------- schema table (validation only)

var gqlSchema = map[string][]string{
	"Query":                            {"viewer", "repository", "node", "__type", "__schema"},
	"User":                             {"login", "id"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url", "createdAt", "updatedAt"},
	"Issue":                            {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "url", "author", "labels", "comments"},
	"IssueConnection":                  {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"},
	"PullRequest":                      {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt", "url", "author", "headRef", "baseRef"},
	"PullRequestConnection":            {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changes"},
	"PullRequestChangedFileConnection": {"nodes", "pageInfo", "totalCount"},
	"Ref":                              {"name", "target"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "clientMutationId"},
	"CommentEdge":                      {"node", "cursor"},
	"Comment":                          {"id", "body", "author", "createdAt", "updatedAt"},
	"__Type":                           {"name", "fields", "enumValues", "kind", "description"},
	"__Field":                          {"name", "description", "type"},
	"__EnumValue":                      {"name", "description"},
	"Node":                             {"id"},
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "url", "author", "labels", "comments"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt", "url", "author", "headRef", "baseRef"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url", "createdAt", "updatedAt"},
	"SearchType":  {},
}

var gqlTypeEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT", "MILESTONE", "PACKAGE", "WIKI", "PROJECT"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

// ---------------------------------------------------------------- global ids

func gqlB64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

var gqlB64Cleaner = regexp.MustCompile(`[^A-Za-z0-9+/=]`)

// gqlUnb64 mirrors Node's Buffer.from(text, 'base64').toString('utf8'):
// lenient (ignores characters outside the alphabet, tolerates missing
// padding), and null when the decoded text holds no ':'.
func gqlUnb64(text string) (string, bool) {
	cleaned := gqlB64Cleaner.ReplaceAllString(text, "")
	if m := len(cleaned) % 4; m != 0 {
		cleaned += strings.Repeat("=", 4-m)
	}
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", false
	}
	s := string(decoded)
	if !strings.Contains(s, ":") {
		return "", false
	}
	return s, true
}

func repoGlobalID(repo *Repo) string {
	return gqlB64("Repository:" + strconv.Itoa(repo.ID))
}

func issueGlobalID(repo *Repo, issue *Issue) string {
	return gqlB64("Issue:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(issue.Number))
}

func pullGlobalID(repo *Repo, pull *Pull) string {
	return gqlB64("PullRequest:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(pull.Number))
}

func commentGlobalID(repo *Repo, issueNumber, commentID int) string {
	return gqlB64("IssueComment:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(issueNumber) + ":" + strconv.Itoa(commentID))
}

func cursorFor(repo *Repo, number int) string {
	return gqlB64("IssueCursor:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(number))
}

func cursorNumber(cursor string) (int, bool) {
	decoded, ok := gqlUnb64(cursor)
	if !ok {
		return 0, false
	}
	parts := strings.Split(decoded, ":")
	if len(parts) != 3 || parts[0] != "IssueCursor" {
		return 0, false
	}
	if !isAllDigits(parts[1]) || !isAllDigits(parts[2]) {
		return 0, false
	}
	n, _ := strconv.Atoi(parts[2])
	return n, true
}

func isAllDigits(s string) bool {
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

// ---------------------------------------------------------------- raw values (what resolvers return)

func userRaw() *gqlRaw {
	r := newGqlRaw("User")
	r.fields.set("login", ViewerLogin)
	r.fields.set("id", gqlB64("User:"+strconv.Itoa(ViewerID)))
	return r
}

func refRaw(name string) *gqlRaw {
	r := newGqlRaw("Ref")
	r.fields.set("name", name)
	return r
}

func repoRaw(repo *Repo) *gqlRaw {
	_, hasTip := repo.Refs["refs/heads/"+repo.DefaultBranch]
	r := newGqlRaw("Repository")
	r.fields.set("id", repoGlobalID(repo))
	r.fields.set("databaseId", int64(repo.ID))
	r.fields.set("name", repo.Name)
	r.fields.set("nameWithOwner", repo.Owner+"/"+repo.Name)
	r.fields.set("owner", userRaw())
	if !hasTip {
		r.fields.set("defaultBranchRef", nil)
	} else {
		r.fields.set("defaultBranchRef", refRaw(repo.DefaultBranch))
	}
	r.fields.set("viewerPermission", "ADMIN")
	r.fields.set("hasIssuesEnabled", true)
	r.fields.set("description", repo.Description)
	r.fields.set("url", "https://github.com/"+repo.Owner+"/"+repo.Name)
	r.repo = repo
	return r
}

func issueRaw(repo *Repo, issue *Issue) *gqlRaw {
	state := "CLOSED"
	if issue.State == "open" {
		state = "OPEN"
	}
	r := newGqlRaw("Issue")
	r.fields.set("id", issueGlobalID(repo, issue))
	r.fields.set("number", int64(issue.Number))
	r.fields.set("title", issue.Title)
	r.fields.set("state", state)
	r.fields.set("body", issue.Body)
	r.fields.set("author", userRaw())
	r.repo = repo
	r.issue = issue
	return r
}

func pullState(pull *Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func pullRaw(repo *Repo, pull *Pull) *gqlRaw {
	r := newGqlRaw("PullRequest")
	r.fields.set("id", pullGlobalID(repo, pull))
	r.fields.set("number", int64(pull.Number))
	r.fields.set("title", pull.Title)
	r.fields.set("body", pull.Body)
	r.fields.set("state", pullState(pull))
	r.fields.set("merged", pull.Merged)
	r.fields.set("headRefName", pull.HeadRef)
	r.fields.set("baseRefName", pull.BaseRef)
	r.repo = repo
	r.pull = pull
	return r
}

func commentRaw(repo *Repo, issueNumber int, comment *Comment) *gqlRaw {
	r := newGqlRaw("Comment")
	r.fields.set("id", commentGlobalID(repo, issueNumber, comment.ID))
	r.fields.set("body", comment.Body)
	r.fields.set("author", userRaw())
	return r
}

// ---------------------------------------------------------------- execution

type gqlError struct{ message string }

type gqlCtx struct {
	store     *Store
	variables map[string]any
	errors    []gqlError
}

func resolveValue(value *gqlASTValue, variables map[string]any) any {
	switch value.kind {
	case "variable":
		if v, ok := variables[value.str]; ok {
			return v
		}
		return nil
	case "string":
		return value.str
	case "int", "float":
		return value.num
	case "boolean":
		return value.boolean
	case "null":
		return nil
	case "enum":
		return value.str
	case "list":
		out := make([]any, len(value.items))
		for i, item := range value.items {
			out[i] = resolveValue(item, variables)
		}
		return out
	case "object":
		out := map[string]any{}
		for k, v := range value.fields {
			out[k] = resolveValue(v, variables)
		}
		return out
	}
	return nil
}

func resolveArgs(args map[string]*gqlASTValue, variables map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = resolveValue(v, variables)
	}
	return out
}

// asStringListContains reports whether a resolved list value contains the
// string s (Node: Array.prototype.includes with SameValueZero).
func asStringListContains(v any, s string) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range list {
		if str, ok := e.(string); ok && str == s {
			return true
		}
	}
	return false
}

func asNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asObject(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// jsSlice mirrors Array.prototype.slice(0, end) for a list of length n:
// end is ToIntegerOrInfinity (NaN -> 0, negatives count from the end).
func jsSliceLen(n int, end float64) int {
	var e int
	switch {
	case math.IsNaN(end):
		e = 0
	case math.IsInf(end, 1):
		e = n
	case math.IsInf(end, -1):
		e = 0
	case end < 0:
		e = n + int(end)
		if e < 0 {
			e = 0
		}
	default:
		e = int(end)
	}
	if e > n {
		e = n
	}
	if e < 0 {
		e = 0
	}
	return e
}

func repoOf(raw *gqlRaw) *Repo {
	if raw == nil {
		return nil
	}
	return raw.repo
}

var gqlRepoIDRe = regexp.MustCompile(`^Repository:(\d+)$`)

func findRepoByGlobalID(ctx *gqlCtx, id string) *Repo {
	decoded, ok := gqlUnb64(id)
	if !ok {
		return nil
	}
	m := gqlRepoIDRe.FindStringSubmatch(decoded)
	if m == nil {
		return nil
	}
	numeric, _ := strconv.Atoi(m[1])
	for _, repo := range ctx.store.Repos {
		if repo.ID == numeric {
			return repo
		}
	}
	return nil
}

type decodedGlobalID struct {
	typ    string
	repoID int
	rest   []string
}

func decodeGlobalID(id string) *decodedGlobalID {
	decoded, ok := gqlUnb64(id)
	if !ok {
		return nil
	}
	parts := strings.Split(decoded, ":")
	if len(parts) < 2 {
		return nil
	}
	repoID := gqlParseInt(parts[1])
	if math.IsNaN(repoID) || math.IsInf(repoID, 0) || math.Abs(repoID) > 9007199254740991 || repoID != math.Trunc(repoID) {
		return nil
	}
	return &decodedGlobalID{typ: parts[0], repoID: int(repoID), rest: parts[2:]}
}

func findRepoByID(ctx *gqlCtx, numeric int) *Repo {
	for _, repo := range ctx.store.Repos {
		if repo.ID == numeric {
			return repo
		}
	}
	return nil
}

func issuesConnection(ctx *gqlCtx, repo *Repo, args map[string]any) *gqlRaw {
	list := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		list = append(list, issue)
	}
	// Node spreads the Map in insertion order (= creation order); sort by
	// createdSeq to get the same order from a Go map.
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedSeq < list[j].CreatedSeq })
	if states, ok := args["states"]; ok {
		if arr, isArr := states.([]any); isArr && len(arr) > 0 {
			filtered := list[:0]
			for _, issue := range list {
				want := "CLOSED"
				if issue.State == "open" {
					want = "OPEN"
				}
				if asStringListContains(states, want) {
					filtered = append(filtered, issue)
				}
			}
			list = filtered
		}
	}
	dir := -1.0
	if orderBy := asObject(args["orderBy"]); orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok && d == "ASC" {
			dir = 1
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if dir == 1 {
			return list[i].CreatedSeq < list[j].CreatedSeq
		}
		return list[i].CreatedSeq > list[j].CreatedSeq
	})
	if after, ok := args["after"].(string); ok && after != "" {
		if n, ok := cursorNumber(after); ok {
			filtered := list[:0]
			for _, issue := range list {
				if dir == 1 {
					if issue.Number > n {
						filtered = append(filtered, issue)
					}
				} else if issue.Number < n {
					filtered = append(filtered, issue)
				}
			}
			list = filtered
		}
	}
	first := float64(len(list))
	if f, ok := asNumber(args["first"]); ok {
		first = f
	}
	end := jsSliceLen(len(list), first)
	page := list[:end]
	hasNextPage := float64(len(list)) > first
	var endCursor gqlValue
	if len(page) > 0 {
		endCursor = cursorFor(repo, page[len(page)-1].Number)
	}
	nodes := make([]gqlValue, len(page))
	for i, issue := range page {
		nodes[i] = issueRaw(repo, issue)
	}
	pageInfo := newGqlRaw("PageInfo")
	pageInfo.fields.set("hasNextPage", hasNextPage)
	pageInfo.fields.set("endCursor", endCursor)
	pageInfo.fields.set("hasPreviousPage", false)
	pageInfo.fields.set("startCursor", nil)
	r := newGqlRaw("IssueConnection")
	r.fields.set("nodes", nodes)
	r.fields.set("pageInfo", pageInfo)
	r.fields.set("totalCount", int64(len(list)))
	return r
}

func pullsConnection(repo *Repo, args map[string]any) *gqlRaw {
	list := make([]*Pull, 0, len(repo.Pulls))
	for _, pull := range repo.Pulls {
		if pull.State == "open" {
			list = append(list, pull)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
	first := float64(len(list))
	if f, ok := asNumber(args["first"]); ok {
		first = f
	}
	end := jsSliceLen(len(list), first)
	page := list[:end]
	nodes := make([]gqlValue, len(page))
	for i, pull := range page {
		nodes[i] = pullRaw(repo, pull)
	}
	pageInfo := newGqlRaw("PageInfo")
	pageInfo.fields.set("hasNextPage", float64(len(list)) > first)
	pageInfo.fields.set("endCursor", nil)
	pageInfo.fields.set("hasPreviousPage", false)
	pageInfo.fields.set("startCursor", nil)
	r := newGqlRaw("PullRequestConnection")
	r.fields.set("nodes", nodes)
	r.fields.set("pageInfo", pageInfo)
	r.fields.set("totalCount", int64(len(list)))
	return r
}

func filesConnection(pull *Pull, args map[string]any) *gqlRaw {
	first := float64(len(pull.Files))
	if f, ok := asNumber(args["first"]); ok {
		first = f
	}
	end := jsSliceLen(len(pull.Files), first)
	page := pull.Files[:end]
	nodes := make([]gqlValue, len(page))
	for i, f := range page {
		n := newGqlRaw("PullRequestChangedFile")
		n.fields.set("path", f.Filename)
		n.fields.set("additions", int64(f.Additions))
		n.fields.set("deletions", int64(f.Deletions))
		n.fields.set("changes", int64(f.Changes))
		nodes[i] = n
	}
	pageInfo := newGqlRaw("PageInfo")
	pageInfo.fields.set("hasNextPage", float64(len(pull.Files)) > first)
	pageInfo.fields.set("endCursor", nil)
	pageInfo.fields.set("hasPreviousPage", false)
	pageInfo.fields.set("startCursor", nil)
	r := newGqlRaw("PullRequestChangedFileConnection")
	r.fields.set("nodes", nodes)
	r.fields.set("pageInfo", pageInfo)
	r.fields.set("totalCount", int64(len(pull.Files)))
	return r
}

func typeRaw(name string) *gqlRaw {
	fields, hasFields := gqlTypeFields[name]
	enums, hasEnums := gqlTypeEnumValues[name]
	if !hasFields && !hasEnums {
		return nil
	}
	r := newGqlRaw("__Type")
	r.fields.set("name", name)
	fieldNodes := make([]gqlValue, 0, len(fields))
	for _, f := range fields {
		n := newGqlRaw("__Field")
		n.fields.set("name", f)
		fieldNodes = append(fieldNodes, n)
	}
	r.fields.set("fields", fieldNodes)
	enumNodes := make([]gqlValue, 0, len(enums))
	for _, e := range enums {
		n := newGqlRaw("__EnumValue")
		n.fields.set("name", e)
		enumNodes = append(enumNodes, n)
	}
	r.fields.set("enumValues", enumNodes)
	if hasEnums {
		r.fields.set("kind", "ENUM")
	} else {
		r.fields.set("kind", "OBJECT")
	}
	return r
}

func mergeableOf(ctx *gqlCtx, repo *Repo, pull *Pull) string {
	if pull.Merged || pull.State != "open" {
		return "UNKNOWN"
	}
	if ctx.store.PullConflicted(repo, pull) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// readField reads one field of a raw value. Returns nil for a missing
// nullable field. Mutations run here (they are fields of the Mutation type).
func readField(ctx *gqlCtx, parentType string, raw *gqlRaw, name string, args map[string]any) gqlValue {
	if raw == nil {
		return nil
	}
	if name == "__typename" {
		return raw.typename
	}

	if parentType == "Query" {
		if name == "viewer" {
			return userRaw()
		}
		if name == "repository" {
			owner, _ := args["owner"].(string)
			repoName, isStr := args["name"].(string)
			if owner != ViewerLogin || !isStr {
				return nil
			}
			repo := ctx.store.GetRepo(repoName)
			if repo == nil {
				return nil
			}
			return repoRaw(repo)
		}
		if name == "node" {
			id, isStr := args["id"].(string)
			if !isStr {
				return nil
			}
			decoded := decodeGlobalID(id)
			if decoded == nil {
				return nil
			}
			repo := findRepoByID(ctx, decoded.repoID)
			if repo == nil {
				return nil
			}
			switch decoded.typ {
			case "Repository":
				return repoRaw(repo)
			case "Issue":
				if len(decoded.rest) == 0 {
					return nil
				}
				n, ok := gqlParseIntToInt(decoded.rest[0])
				if !ok {
					return nil
				}
				issue := repo.Issues[n]
				if issue == nil {
					return nil
				}
				return issueRaw(repo, issue)
			case "PullRequest":
				if len(decoded.rest) == 0 {
					return nil
				}
				n, ok := gqlParseIntToInt(decoded.rest[0])
				if !ok {
					return nil
				}
				pull := repo.Pulls[n]
				if pull == nil {
					return nil
				}
				return pullRaw(repo, pull)
			}
			return nil
		}
		if name == "__type" {
			typeName, isStr := args["name"].(string)
			if !isStr {
				return nil
			}
			tr := typeRaw(typeName)
			if tr == nil {
				return nil
			}
			return tr
		}
	}

	if parentType == "Repository" {
		repo := repoOf(raw)
		if repo == nil {
			return nil
		}
		if name == "issue" {
			number, ok := asNumber(args["number"])
			if !ok {
				return nil
			}
			if number != math.Trunc(number) {
				return nil
			}
			issue := repo.Issues[int(number)]
			if issue == nil {
				return nil
			}
			return issueRaw(repo, issue)
		}
		if name == "issues" {
			return issuesConnection(ctx, repo, args)
		}
		if name == "pullRequests" {
			return pullsConnection(repo, args)
		}
		if name == "pullRequest" {
			number, ok := asNumber(args["number"])
			if !ok {
				return nil
			}
			if number != math.Trunc(number) {
				return nil
			}
			pull := repo.Pulls[int(number)]
			if pull == nil {
				return nil
			}
			return pullRaw(repo, pull)
		}
		if name == "owner" {
			return userRaw()
		}
		if name == "defaultBranchRef" {
			if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
				return nil
			}
			return refRaw(repo.DefaultBranch)
		}
	}

	if parentType == "IssueConnection" || parentType == "PullRequestConnection" || parentType == "PullRequestChangedFileConnection" {
		if name == "nodes" {
			if v, ok := raw.get("nodes"); ok {
				return v
			}
			return []gqlValue{}
		}
		if name == "pageInfo" {
			if v, ok := raw.get("pageInfo"); ok {
				return v
			}
			return nil
		}
		if name == "totalCount" {
			if v, ok := raw.get("totalCount"); ok {
				return v
			}
			return int64(0)
		}
	}

	if parentType == "PullRequest" {
		repo := repoOf(raw)
		pull := raw.pull
		if repo == nil || pull == nil {
			return nil
		}
		if name == "mergeable" {
			return mergeableOf(ctx, repo, pull)
		}
		if name == "files" {
			return filesConnection(pull, args)
		}
	}

	if parentType == "Mutation" {
		// Node: input === undefined is the only "missing" case; a null input
		// proceeds and throws a TypeError on first property access, naming
		// the property each mutation reads first.
		inputVal, present := args["input"]
		if !present {
			ctx.errors = append(ctx.errors, gqlError{message: "Field \"" + name + "\" requires an input argument"})
			return nil
		}
		if inputVal == nil {
			prop := map[string]string{
				"createIssue":       "repositoryId",
				"closeIssue":        "issueId",
				"createPullRequest": "repositoryId",
				"mergePullRequest":  "pullRequestId",
				"addComment":        "subjectId",
			}[name]
			if prop != "" {
				throwGQL("Cannot read properties of null (reading '" + prop + "')")
			}
		}
		input := asObject(inputVal)
		if input == nil {
			input = map[string]any{}
		}
		optStr := func(key string) string {
			if v, ok := input[key]; ok && v != nil {
				return jsString(v)
			}
			return ""
		}
		if name == "createIssue" {
			repo := findRepoByGlobalID(ctx, optStr("repositoryId"))
			if repo == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "createIssue: unknown repositoryId"})
				return nil
			}
			issue := ctx.store.CreateIssue(repo, optStr("title"), optStr("body"))
			payload := newGqlRaw("CreateIssuePayload")
			payload.fields.set("issue", issueRaw(repo, issue))
			return payload
		}
		if name == "closeIssue" {
			var decoded *decodedGlobalID
			if s, ok := input["issueId"].(string); ok {
				decoded = decodeGlobalID(s)
			}
			if decoded == nil || decoded.typ != "Issue" {
				ctx.errors = append(ctx.errors, gqlError{message: "closeIssue: unknown issueId"})
				return nil
			}
			repo := findRepoByID(ctx, decoded.repoID)
			var issue *Issue
			if repo != nil && len(decoded.rest) > 0 {
				if n, ok := gqlParseIntToInt(decoded.rest[0]); ok {
					issue = repo.Issues[n]
				}
			}
			if repo == nil || issue == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "closeIssue: unknown issueId"})
				return nil
			}
			issue.State = "closed"
			issue.JSONCache = nil
			issue.JSONCacheLen = 0
			payload := newGqlRaw("CloseIssuePayload")
			payload.fields.set("issue", issueRaw(repo, issue))
			return payload
		}
		if name == "createPullRequest" {
			repo := findRepoByGlobalID(ctx, optStr("repositoryId"))
			if repo == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "createPullRequest: unknown repositoryId"})
				return nil
			}
			pull := ctx.store.CreatePull(repo, optStr("title"), "", optStr("headRefName"), optStr("baseRefName"))
			if pull == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "createPullRequest: head or base branch does not exist, or a pull request already exists"})
				return nil
			}
			payload := newGqlRaw("CreatePullRequestPayload")
			payload.fields.set("pullRequest", pullRaw(repo, pull))
			return payload
		}
		if name == "mergePullRequest" {
			var decoded *decodedGlobalID
			if s, ok := input["pullRequestId"].(string); ok {
				decoded = decodeGlobalID(s)
			}
			if decoded == nil || decoded.typ != "PullRequest" {
				ctx.errors = append(ctx.errors, gqlError{message: "mergePullRequest: unknown pullRequestId"})
				return nil
			}
			repo := findRepoByID(ctx, decoded.repoID)
			var pull *Pull
			if repo != nil && len(decoded.rest) > 0 {
				if n, ok := gqlParseIntToInt(decoded.rest[0]); ok {
					pull = repo.Pulls[n]
				}
			}
			if repo == nil || pull == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "mergePullRequest: unknown pullRequestId"})
				return nil
			}
			if !ctx.store.MergePull(repo, pull) {
				ctx.errors = append(ctx.errors, gqlError{message: "mergePullRequest: pull request is not mergeable"})
				return nil
			}
			payload := newGqlRaw("MergePullRequestPayload")
			payload.fields.set("pullRequest", pullRaw(repo, pull))
			return payload
		}
		if name == "addComment" {
			var decoded *decodedGlobalID
			if s, ok := input["subjectId"].(string); ok {
				decoded = decodeGlobalID(s)
			}
			if decoded == nil || decoded.typ != "Issue" {
				ctx.errors = append(ctx.errors, gqlError{message: "addComment: unknown subjectId"})
				return nil
			}
			repo := findRepoByID(ctx, decoded.repoID)
			issueNumber := 0
			var issue *Issue
			if repo != nil && len(decoded.rest) > 0 {
				if n, ok := gqlParseIntToInt(decoded.rest[0]); ok {
					issueNumber = n
					issue = repo.Issues[n]
				}
			}
			if repo == nil || issue == nil {
				ctx.errors = append(ctx.errors, gqlError{message: "addComment: unknown subjectId"})
				return nil
			}
			repo.CommentSeq++
			comment := &Comment{ID: ctx.store.NextSeq(), Body: optStr("body"), Issue: issueNumber, CreatedSeq: ctx.store.NextSeq()}
			repo.Comments[comment.ID] = comment
			payload := newGqlRaw("AddCommentPayload")
			edge := newGqlRaw("CommentEdge")
			edge.fields.set("node", commentRaw(repo, issueNumber, comment))
			edge.fields.set("cursor", nil)
			payload.fields.set("commentEdge", edge)
			return payload
		}
	}

	if parentType == "__Type" {
		if name == "fields" {
			if v, ok := raw.get("fields"); ok {
				return v
			}
			return []gqlValue{}
		}
		if name == "enumValues" {
			if v, ok := raw.get("enumValues"); ok {
				return v
			}
			return []gqlValue{}
		}
	}

	// A plain scalar field stored on the raw value.
	if v, ok := raw.get(name); ok {
		return v
	}
	return nil
}

// executeSelections executes one operation's selections against a root raw
// value.
func executeSelections(ctx *gqlCtx, selections []*gqlSelection, parentType string, parent *gqlRaw) *gqlObj {
	out := newGqlObj()
	for _, selection := range selections {
		if selection.kind == "fragment" {
			if selection.hasCondition {
				if parent == nil || selection.typeCondition != parent.typename {
					continue
				}
			}
			merged := executeSelections(ctx, selection.children, parentType, parent)
			for _, k := range merged.keys {
				out.set(k, merged.vals[k])
			}
			continue
		}
		if allowed, ok := gqlSchema[parentType]; ok {
			found := false
			for _, f := range allowed {
				if f == selection.name {
					found = true
					break
				}
			}
			if !found && selection.name != "__typename" {
				ctx.errors = append(ctx.errors, gqlError{message: "Cannot query field \"" + selection.name + "\" on type \"" + parentType + "\"."})
				out.set(selection.alias, nil)
				continue
			}
		}
		args := resolveArgs(selection.args, ctx.variables)
		value := readField(ctx, parentType, parent, selection.name, args)
		out.set(selection.alias, executeValue(ctx, selection.children, value))
	}
	return out
}

// executeValue recurses into a resolved value: scalars pass through,
// objects/lists re-execute.
func executeValue(ctx *gqlCtx, children []*gqlSelection, value gqlValue) gqlValue {
	if value == nil {
		return nil
	}
	switch t := value.(type) {
	case []gqlValue:
		out := make([]gqlValue, len(t))
		for i, entry := range t {
			out[i] = executeValue(ctx, children, entry)
		}
		return out
	case *gqlRaw:
		if t == nil {
			return nil
		}
		if len(children) == 0 {
			return value
		}
		return executeSelections(ctx, children, t.typename, t)
	default:
		return value
	}
}

type gqlResult struct {
	data   *gqlObj
	errors []gqlError
}

// executeOperation executes one parsed operation; returns the {data, errors}
// envelope. A throw anywhere (Node's try/catch) yields data null.
func executeOperation(store *Store, operation *gqlOperation, variables map[string]any) (result gqlResult) {
	ctx := &gqlCtx{store: store, variables: variables}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	root := newGqlRaw(rootType)
	defer func() {
		if r := recover(); r != nil {
			msg := "execution failed"
			if t, ok := r.(gqlThrow); ok {
				msg = t.msg
			}
			ctx.errors = append(ctx.errors, gqlError{message: msg})
			result = gqlResult{data: nil, errors: ctx.errors}
		}
	}()
	data := executeSelections(ctx, operation.selections, root.typename, root)
	return gqlResult{data: data, errors: ctx.errors}
}

// parseDocument parses one document string into operations; throws
// (gqlThrow) on a syntax error.
func parseDocument(src string) []*gqlOperation {
	return (&gqlParser{toks: tokenize(src)}).parseDocument()
}

// executeRequest runs one {query, variables, operationName} request. A
// tokenizer/parser throw becomes the {data: null, errors} envelope Node
// returns for a syntax error.
func executeRequest(store *Store, body map[string]any) (result gqlResult) {
	defer func() {
		if r := recover(); r != nil {
			if t, ok := r.(gqlThrow); ok {
				result = gqlResult{data: nil, errors: []gqlError{{message: t.msg}}}
				return
			}
			panic(r)
		}
	}()
	query, _ := body["query"].(string)
	if _, isStr := body["query"].(string); !isStr || strings.TrimSpace(query) == "" {
		return gqlResult{data: nil, errors: []gqlError{{message: "Must provide query string."}}}
	}
	operations := parseDocument(query)
	if len(operations) == 0 {
		return gqlResult{data: nil, errors: []gqlError{{message: "Must provide query string."}}}
	}
	operation := operations[0]
	if name, ok := body["operationName"].(string); ok {
		var named *gqlOperation
		for _, op := range operations {
			if op.name == name {
				named = op
				break
			}
		}
		if named == nil {
			return gqlResult{data: nil, errors: []gqlError{{message: "Unknown operation named \"" + name + "\"."}}}
		}
		operation = named
	}
	variables := map[string]any{}
	if v, ok := body["variables"].(map[string]any); ok && v != nil {
		variables = v
	}
	return executeOperation(store, operation, variables)
}

// gqlEnvelope renders one operation's {data, errors} envelope: errors are
// included only when non-empty (Node's envelope in app.ts).
func gqlEnvelope(result gqlResult) *gqlObj {
	out := newGqlObj()
	if result.data == nil {
		out.set("data", nil)
	} else {
		out.set("data", result.data)
	}
	if len(result.errors) > 0 {
		errs := make([]gqlValue, len(result.errors))
		for i, e := range result.errors {
			entry := newGqlObj()
			entry.set("message", e.message)
			errs[i] = entry
		}
		out.set("errors", errs)
	}
	return out
}

// ---------------------------------------------------------------- HTTP handler

// ServeGraphQL serves POST /api/graphql only (Agent B's router maps the
// path here). The body is parsed here, mirroring app.ts: malformed JSON is
// a 400 before auth is checked; a JSON array body is GitHub's operation
// batching, answered by an array of envelopes in order.
func ServeGraphQL(s *Store, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusNotFound, "Not Found")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	var bodyParsed any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &bodyParsed); err != nil {
			jsonError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	if !checkAuth(r) {
		jsonError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if arr, ok := bodyParsed.([]any); ok {
		results := make([]gqlValue, len(arr))
		for i, entry := range arr {
			if m, ok := entry.(map[string]any); ok {
				results[i] = gqlEnvelope(executeRequest(s, m))
			} else {
				results[i] = gqlEnvelope(gqlResult{data: nil, errors: []gqlError{{message: "Must provide query string."}}})
			}
		}
		writeJSONBytes(w, http.StatusOK, gqlRenderJSON(results))
		return
	}
	single := map[string]any{}
	if m, ok := bodyParsed.(map[string]any); ok {
		single = m
	}
	writeJSONBytes(w, http.StatusOK, gqlRenderJSON(gqlEnvelope(executeRequest(s, single))))
}
