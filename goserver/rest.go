// rest.go — Agent B: every REST v3 handler + the JSON renderers.
//
// Translation of gh-p4ab/app.ts (repoObject/issueObject/pullObject, the
// handleRest/handleRepo/handleContents/handleIssues/handlePulls handlers).
// Field names, field order, null-vs-omitted, ids and sequences match Node
// exactly. Repo/Issue jsonCache is reused exactly like Node: rendered once,
// served as raw bytes (no trailing newline), invalidated on mutation.
// Pulls are rendered fresh on every read (Node never caches pull JSON;
// the Pull jsonCache field is only cleared on merge).

package main

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- JSON views
// Field order is the JSON key order (encoding/json marshals struct fields
// in declaration order, matching Node's object-literal order).

type userView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoView struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userView `json:"owner"`
	Private       bool     `json:"private"`
	DefaultBranch string   `json:"default_branch"`
	Description   string   `json:"description"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HTMLURL       string   `json:"html_url"`
	CloneURL      string   `json:"clone_url"`
	HasIssues     bool     `json:"has_issues"`
}

type labelView struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueView struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelView `json:"labels"`
	User      userView    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	HTMLURL   string      `json:"html_url"`
}

type refView struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type pullView struct {
	ID             int      `json:"id"`
	Number         int      `json:"number"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	State          string   `json:"state"`
	Head           refView  `json:"head"`
	Base           refView  `json:"base"`
	Merged         bool     `json:"merged"`
	MergedAt       *string  `json:"merged_at"`
	MergeCommitSHA *string  `json:"merge_commit_sha"`
	Mergeable      bool     `json:"mergeable"`
	User           userView `json:"user"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	HTMLURL        string   `json:"html_url"`
}

type commentView struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userView `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type commitSHAView struct {
	SHA string `json:"sha"`
}

type branchView struct {
	Name      string        `json:"name"`
	Commit    commitSHAView `json:"commit"`
	Protected bool          `json:"protected"`
}

type gitRefView struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type identView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitInnerView struct {
	Message   string    `json:"message"`
	Author    identView `json:"author"`
	Committer identView `json:"committer"`
}

type commitView struct {
	SHA       string          `json:"sha"`
	Commit    commitInnerView `json:"commit"`
	Author    userView        `json:"author"`
	Committer userView        `json:"committer"`
	Parents   []commitSHAView `json:"parents"`
}

type contentFileView struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Content  string `json:"content"`
}

type dirEntryView struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size *int   `json:"size,omitempty"`
}

type contentPutInner struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type contentPutView struct {
	Content contentPutInner `json:"content"`
	Commit  commitSHAView   `json:"commit"`
}

type commitOnlyView struct {
	Commit commitSHAView `json:"commit"`
}

type mergeView struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type pullFileView struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

type errorView struct {
	Message string `json:"message"`
}

type rootView struct {
	CurrentUserURL string `json:"current_user_url"`
	RateLimitURL   string `json:"rate_limit_url"`
}

type metaView struct {
	InstalledVersion                 string `json:"installed_version"`
	VerifiablePasswordAuthentication bool   `json:"verifiable_password_authentication"`
}

type userFullView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

type okView struct {
	OK bool `json:"ok"`
}

type rpcErrorView struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type parseErrorView struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Error   rpcErrorView `json:"error"`
}

// ---------------------------------------------------------------- side state
// types.go's Comment carries no issue number (Node smuggles it as an extra
// property) and Go maps have no insertion order (Node's label Map does),
// so REST keeps two tiny side tables. Keys are (store, id) / (store,
// repo createdSeq) so a second store (e.g. warmup) can never collide and
// no cleanup is ever needed: ids and createdSeqs only move forward.

type labelOrderKey struct {
	store *Store
	seq   int // repo.CreatedSeq
}

var auxMu sync.Mutex
var commentIssue = map[*Store]map[int]int{}   // comment id -> issue number
var labelOrder = map[labelOrderKey][]string{} // repo -> label names, creation order

// ---------------------------------------------------------------- renderers

func viewerUser() userView {
	return userView{Login: ViewerLogin, ID: ViewerID, Type: "User"}
}

func repoObject(repo *Repo) repoView {
	return repoView{
		ID: repo.ID, Name: repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         viewerUser(),
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

func repoJSON(repo *Repo) []byte {
	if repo.JSONCache == nil {
		repo.JSONCache = marshalJSONNoNL(repoObject(repo))
		repo.JSONCacheLen = len(repo.JSONCache)
	}
	return repo.JSONCache
}

func labelObject(label *Label) labelView {
	return labelView{Name: label.Name, Color: label.Color, Description: ""}
}

func issueObject(repo *Repo, issue *Issue) issueView {
	labels := make([]labelView, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if l, ok := repo.Labels[name]; ok {
			labels = append(labels, labelObject(l))
		} else {
			labels = append(labels, labelView{Name: name, Color: "ededed", Description: ""})
		}
	}
	return issueView{
		ID: issue.ID, Number: issue.Number,
		Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels:    labels,
		User:      viewerUser(),
		CreatedAt: NowISO,
		UpdatedAt: NowISO,
		HTMLURL:   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func issueJSON(repo *Repo, issue *Issue) []byte {
	if issue.JSONCache == nil {
		issue.JSONCache = marshalJSONNoNL(issueObject(repo, issue))
		issue.JSONCacheLen = len(issue.JSONCache)
	}
	return issue.JSONCache
}

func pullHeadSHA(repo *Repo, pull *Pull) string {
	if sha, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		return sha
	}
	return pull.HeadSHA
}

func pullBaseSHA(repo *Repo, pull *Pull) string {
	if sha, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		return sha
	}
	return pull.BaseSHA
}

func pullObject(s *Store, repo *Repo, pull *Pull) pullView {
	return pullView{
		ID: pull.ID, Number: pull.Number,
		Title: pull.Title, Body: pull.Body, State: pull.State,
		Head:           refView{Ref: pull.HeadRef, SHA: pullHeadSHA(repo, pull), Label: repo.Owner + ":" + pull.HeadRef},
		Base:           refView{Ref: pull.BaseRef, SHA: pullBaseSHA(repo, pull), Label: repo.Owner + ":" + pull.BaseRef},
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSHA: pull.MergeCommitSHA,
		Mergeable:      !repoPullConflicted(s, repo, pull),
		User:           viewerUser(),
		CreatedAt:      NowISO,
		UpdatedAt:      NowISO,
		HTMLURL:        "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
	}
}

// repoPullConflicted mirrors Node's app.ts helper (and Store.pullConflicted):
// conflicted when a path the PR changes differs between its recorded base
// and the current tip of its base branch.
func repoPullConflicted(s *Store, repo *Repo, pull *Pull) bool {
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok || baseTip == pull.BaseSHA {
		return false
	}
	baseFiles := s.FilesAt(repo, pull.BaseSHA)
	tipFiles := s.FilesAt(repo, baseTip)
	for _, f := range pull.Files {
		if baseFiles[f.Filename] != tipFiles[f.Filename] {
			return true
		}
	}
	return false
}

var identRe = regexp.MustCompile(`^(.*?) <(.*?)> (\d+) ([+-]\d{4})$`)

// identParts splits "Name <email> <epoch> +0000" like Node; a non-matching
// ident yields the whole string as the name and the fixed date.
func identParts(ident string) identView {
	m := identRe.FindStringSubmatch(ident)
	if m == nil {
		return identView{Name: ident, Email: "", Date: FixedISO}
	}
	sec, _ := strconv.ParseInt(m[3], 10, 64)
	return identView{
		Name:  m[1],
		Email: m[2],
		Date:  time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// ---------------------------------------------------------------- helpers

func bodyString(c *restCtx, key string) string {
	if c.json == nil {
		return ""
	}
	if v, ok := c.json[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func bodyBool(c *restCtx, key string, fallback bool) bool {
	if c.json == nil {
		return fallback
	}
	if v, ok := c.json[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

// openRepo resolves owner + repo; 404 when the owner is not the viewer or
// the repo is missing (never 403).
func openRepo(c *restCtx, owner, name string) *Repo {
	if owner != ViewerLogin {
		jsonError(c.w, 404, "Not Found")
		return nil
	}
	repo := c.store.GetRepo(name)
	if repo == nil {
		jsonError(c.w, 404, "Not Found")
		return nil
	}
	return repo
}

// decodeSeg is Node's decode(): decodeURIComponent, raw segment on error
// (and the identity when there is no '%').
func decodeSeg(seg string) string {
	if !strings.Contains(seg, "%") {
		return seg
	}
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

// sendEmpty writes a status with no body and no content-type, only the
// request id (Node's sendEmpty; used for 204).
func sendEmpty(c *restCtx, status int) {
	if c.w.Header().Get("X-GitHub-Request-Id") == "" {
		c.w.Header().Set("X-GitHub-Request-Id", writerReqID(c.w))
	}
	c.w.WriteHeader(status)
}

// linkExtra builds the extra-header map for a paginated list (Link only
// when linkHeader returns a non-empty value).
func linkExtra(c *restCtx, page, perPage, total int) map[string]string {
	if l := linkHeader(c.r, page, perPage, total); l != "" {
		return map[string]string{"link": l}
	}
	return nil
}

// parseNumber parses an issue/PR number the way Node does:
// parseInt(seg, 10), then Number.isSafeInteger. Non-numeric, empty and
// out-of-safe-range values fail (=> 404).
func parseNumber(seg string) (int, bool) {
	n, ok := jsParseInt(seg)
	if !ok || n > 9007199254740991 || n < -9007199254740991 {
		return 0, false
	}
	return int(n), true
}

// commitFiles creates one commit on branch from a flat file map; returns
// the commit oid. (Node's commitFiles in app.ts.)
func commitFiles(s *Store, repo *Repo, branch string, files map[string]string, message string) string {
	treeOID := TreeOIDFromFiles(repo.Objects, files)
	var parents []string
	if p, ok := repo.Refs["refs/heads/"+branch]; ok {
		parents = []string{p}
	}
	commit := BuildCommit(treeOID, parents, message, "", "", FixedEpochSec)
	oid := Sha1Hex(ObjCommit, commit)
	repo.Objects[oid] = &GitObject{Type: ObjCommit, Data: commit}
	repo.Refs["refs/heads/"+branch] = oid
	return oid
}

// decodeBase64Lenient decodes like Node's Buffer.from(s, 'base64'):
// characters outside the alphabet are skipped, decoding stops at the
// first '=' padding, and missing padding is tolerated. Valid suite input
// decodes identically under strict decoding.
func decodeBase64Lenient(s string) ([]byte, error) {
	var clean []byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '+' || ch == '/' {
			clean = append(clean, ch)
		} else if ch == '=' {
			break // Node stops at padding
		}
		// any other character is skipped, like Node
	}
	for len(clean)%4 != 0 {
		clean = append(clean, '=')
	}
	return base64.StdEncoding.DecodeString(string(clean))
}

// ---------------------------------------------------------------- REST router

func handleRest(c *restCtx) {
	store := c.store
	s := c.segments // after /api/v3
	method := c.method

	// GET /api/v3 — the API root (open, 200 + scopes).
	if len(s) == 0 {
		if method == "GET" {
			writeJSONExtra(c.w, 200, rootView{CurrentUserURL: "/api/v3/user", RateLimitURL: "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET /api/v3/meta
	if len(s) == 1 && s[0] == "meta" {
		if method != "GET" {
			// Node returns here without writing a response (the request
			// hangs); answer a JSON 404 instead of hanging the client.
			jsonError(c.w, 404, "Not Found")
			return
		}
		if !needAuth(c) {
			return
		}
		writeJSON(c.w, 200, metaView{InstalledVersion: "1.0.0", VerifiablePasswordAuthentication: true})
		return
	}

	// GET /api/v3/user
	if len(s) == 1 && s[0] == "user" {
		if method != "GET" {
			jsonError(c.w, 404, "Not Found")
			return
		}
		if !needAuth(c) {
			return
		}
		writeJSON(c.w, 200, userFullView{Login: ViewerLogin, ID: ViewerID, Name: ViewerName, Type: "User"})
		return
	}

	// /api/v3/user/repos
	if len(s) == 2 && s[0] == "user" && s[1] == "repos" {
		if !needAuth(c) {
			return
		}
		if method == "GET" {
			all := make([]*Repo, 0, len(store.Repos))
			for _, r := range store.Repos {
				all = append(all, r)
			}
			sort.Slice(all, func(i, j int) bool { return all[i].CreatedSeq < all[j].CreatedSeq })
			page, perPage := pageParams(c.r)
			paged := paginate(all, page, perPage)
			views := make([]repoView, 0, len(paged))
			for _, r := range paged {
				views = append(views, repoObject(r))
			}
			writeJSONExtra(c.w, 200, views, linkExtra(c, page, perPage, len(all)))
			return
		}
		if method == "POST" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			name := bodyString(c, "name")
			if name == "" || store.GetRepo(name) != nil {
				if name == "" {
					jsonError(c.w, 422, "Repository name is required")
				} else {
					jsonError(c.w, 422, "Repository '"+name+"' already exists")
				}
				return
			}
			repo := store.CreateRepo(name, bodyBool(c, "private", false), bodyString(c, "description"))
			writeJSON(c.w, 201, repoObject(repo))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// /api/v3/repos/{owner}/{repo}[...]
	if len(s) >= 3 && s[0] == "repos" {
		handleRepo(c, s)
		return
	}

	jsonError(c.w, 404, "Not Found")
}

func handleRepo(c *restCtx, s []string) {
	method := c.method
	owner := decodeSeg(s[1])
	name := decodeSeg(s[2])
	rest := s[3:]

	if !needAuth(c) {
		return
	}
	repo := openRepo(c, owner, name)
	if repo == nil {
		return
	}

	// GET/PATCH/DELETE /repos/{owner}/{repo}
	if len(rest) == 0 {
		if method == "GET" {
			body := repoJSON(repo)
			writeJSONBytes(c.w, 200, body)
			return
		}
		if method == "PATCH" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			if desc, ok := c.json["description"]; ok {
				if d, ok := desc.(string); ok {
					repo.Description = d
				}
			}
			repo.JSONCache = nil
			repo.JSONCacheLen = 0
			writeJSON(c.w, 200, repoObject(repo))
			return
		}
		if method == "DELETE" {
			c.store.DeleteRepo(name)
			sendEmpty(c, 204)
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET /repos/{owner}/{repo}/branches[/{branch}]
	if rest[0] == "branches" {
		if method != "GET" {
			jsonError(c.w, 404, "Not Found")
			return
		}
		branches := make([]string, 0, len(repo.Refs))
		for r := range repo.Refs {
			if strings.HasPrefix(r, "refs/heads/") {
				branches = append(branches, r[len("refs/heads/"):])
			}
		}
		sort.Strings(branches)
		if len(rest) == 1 {
			views := make([]branchView, 0, len(branches))
			for _, b := range branches {
				views = append(views, branchView{Name: b, Commit: commitSHAView{SHA: repo.Refs["refs/heads/"+b]}, Protected: false})
			}
			writeJSON(c.w, 200, views)
			return
		}
		branch := decodeSeg(strings.Join(rest[1:], "/"))
		tip, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			jsonError(c.w, 404, "Branch not found")
			return
		}
		writeJSON(c.w, 200, branchView{Name: branch, Commit: commitSHAView{SHA: tip}, Protected: false})
		return
	}

	// GET /repos/{owner}/{repo}/git/refs/heads/{branch} | POST /git/refs
	if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
		if method == "GET" && len(rest) >= 3 {
			ref := decodeSeg(strings.Join(rest[2:], "/"))
			full := ref
			if !strings.HasPrefix(ref, "refs/") {
				full = "refs/" + ref
			}
			tip, ok := repo.Refs[full]
			if !ok {
				jsonError(c.w, 404, "Not Found")
				return
			}
			v := gitRefView{Ref: full}
			v.Object.SHA = tip
			v.Object.Type = "commit"
			writeJSON(c.w, 200, v)
			return
		}
		if method == "POST" && len(rest) == 2 {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			ref := bodyString(c, "ref")
			sha := bodyString(c, "sha")
			if !strings.HasPrefix(ref, "refs/") || !sha40Re.MatchString(sha) {
				jsonError(c.w, 422, "Invalid ref or sha")
				return
			}
			if _, ok := repo.Objects[sha]; !ok {
				jsonError(c.w, 422, "Invalid ref or sha")
				return
			}
			if _, exists := repo.Refs[ref]; exists {
				jsonError(c.w, 422, "Reference already exists")
				return
			}
			repo.Refs[ref] = sha
			c.store.InvalidateAdverts(repo)
			v := gitRefView{Ref: ref}
			v.Object.SHA = sha
			v.Object.Type = "commit"
			writeJSON(c.w, 201, v)
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET /repos/{owner}/{repo}/commits[?sha=<ref>]
	if rest[0] == "commits" && len(rest) == 1 {
		if method != "GET" {
			jsonError(c.w, 404, "Not Found")
			return
		}
		ref := repo.DefaultBranch
		if v, ok := c.query["sha"]; ok {
			ref = v
		}
		tip, ok := c.store.ResolveRef(repo, ref)
		if !ok {
			jsonError(c.w, 404, "Not Found")
			return
		}
		walked := make([]commitView, 0)
		seen := map[string]bool{}
		stack := []string{tip}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			obj, ok := repo.Objects[oid]
			if !ok || obj.Type != ObjCommit {
				continue
			}
			pc := ParseCommit(obj.Data)
			author := identParts(pc.Author)
			committer := identParts(pc.Committer)
			parents := make([]commitSHAView, 0, len(pc.Parents))
			for _, p := range pc.Parents {
				parents = append(parents, commitSHAView{SHA: p})
			}
			walked = append(walked, commitView{
				SHA: oid,
				Commit: commitInnerView{
					Message:   pc.Message,
					Author:    author,
					Committer: committer,
				},
				Author:    viewerUser(),
				Committer: viewerUser(),
				Parents:   parents,
			})
			stack = append(stack, pc.Parents...)
		}
		page, perPage := pageParams(c.r)
		writeJSONExtra(c.w, 200, paginate(walked, page, perPage), linkExtra(c, page, perPage, len(walked)))
		return
	}

	// /repos/{owner}/{repo}/contents/{path...}
	if rest[0] == "contents" {
		handleContents(c, repo, rest[1:])
		return
	}

	// /repos/{owner}/{repo}/labels
	if rest[0] == "labels" {
		if method == "GET" && len(rest) == 1 {
			writeJSON(c.w, 200, labelList(c.store, repo))
			return
		}
		if method == "POST" && len(rest) == 1 {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			labelName := bodyString(c, "name")
			if labelName == "" {
				jsonError(c.w, 422, "Label name is required")
				return
			}
			if _, exists := repo.Labels[labelName]; exists {
				jsonError(c.w, 422, "Label '"+labelName+"' already exists")
				return
			}
			label := &Label{Name: labelName, Color: bodyString(c, "color")}
			if label.Color == "" {
				label.Color = "ededed"
			}
			repo.Labels[labelName] = label
			auxMu.Lock()
			key := labelOrderKey{store: c.store, seq: repo.CreatedSeq}
			labelOrder[key] = append(labelOrder[key], labelName)
			auxMu.Unlock()
			writeJSON(c.w, 201, labelObject(label))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// /repos/{owner}/{repo}/issues[/{n}[/comments]]
	if rest[0] == "issues" {
		handleIssues(c, repo, rest[1:])
		return
	}

	// /repos/{owner}/{repo}/pulls[/{n}[/files|merge]]
	if rest[0] == "pulls" {
		handlePulls(c, repo, rest[1:])
		return
	}

	jsonError(c.w, 404, "Not Found")
}

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// labelList returns labels in creation order (Node's Map insertion order).
// Any label missing from the order table (never via REST) falls back to a
// sorted append so the output stays deterministic.
func labelList(store *Store, repo *Repo) []labelView {
	auxMu.Lock()
	order := labelOrder[labelOrderKey{store: store, seq: repo.CreatedSeq}]
	auxMu.Unlock()
	views := make([]labelView, 0, len(repo.Labels))
	seen := map[string]bool{}
	for _, name := range order {
		if l, ok := repo.Labels[name]; ok {
			views = append(views, labelObject(l))
			seen[name] = true
		}
	}
	if len(seen) != len(repo.Labels) {
		rest := make([]string, 0, len(repo.Labels))
		for name := range repo.Labels {
			if !seen[name] {
				rest = append(rest, name)
			}
		}
		sort.Strings(rest)
		for _, name := range rest {
			views = append(views, labelObject(repo.Labels[name]))
		}
	}
	return views
}

func handleContents(c *restCtx, repo *Repo, pathParts []string) {
	method := c.method
	path := ""
	{
		parts := make([]string, 0, len(pathParts))
		for _, p := range pathParts {
			parts = append(parts, decodeSeg(p))
		}
		path = strings.Join(parts, "/")
	}
	// Node: branch = body.branch || (query.ref ?? '') || default;
	// ref = query.ref ?? branch. For GET the body is empty, so ref is
	// effectively query.ref ?? default.
	branch := bodyString(c, "branch")
	qref, qrefOK := c.query["ref"]
	if branch == "" {
		if qrefOK {
			branch = qref
		}
		if branch == "" {
			branch = repo.DefaultBranch
		}
	}
	ref := branch
	if qrefOK {
		ref = qref
	}
	tip, tipOK := c.store.ResolveRef(repo, ref)

	if method == "GET" {
		if !tipOK {
			jsonError(c.w, 404, "Not Found")
			return
		}
		files := c.store.FilesAt(repo, tip)
		if path == "" {
			listing, ok := dirListing(repo, tip, "")
			if !ok {
				jsonError(c.w, 404, "Not Found")
				return
			}
			writeJSON(c.w, 200, listing)
			return
		}
		if blobOID, ok := files[path]; ok {
			var data []byte
			if blob, ok := repo.Objects[blobOID]; ok {
				data = blob.Data
			} else {
				data = []byte{}
			}
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			writeJSON(c.w, 200, contentFileView{
				Type: "file", Encoding: "base64", Size: len(data),
				Name: name, Path: path, SHA: blobOID,
				Content: base64.StdEncoding.EncodeToString(data),
			})
			return
		}
		listing, ok := dirListing(repo, tip, path)
		if !ok {
			jsonError(c.w, 404, "Not Found")
			return
		}
		writeJSON(c.w, 200, listing)
		return
	}

	if method == "PUT" || method == "DELETE" {
		if c.json == nil {
			jsonError(c.w, 400, "Problems parsing JSON")
			return
		}
		message := bodyString(c, "message")
		if message == "" {
			if method == "PUT" {
				message = "Update " + path
			} else {
				message = "Delete " + path
			}
		}
		givenSha := bodyString(c, "sha")
		branchName := bodyString(c, "branch")
		if branchName == "" {
			branchName = repo.DefaultBranch
		}
		// A named branch that does not exist yet starts at the default
		// tip (or empty).
		branchTip, hasBranchTip := repo.Refs["refs/heads/"+branchName]
		if !hasBranchTip && branchName != repo.DefaultBranch {
			if dt, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
				repo.Refs["refs/heads/"+branchName] = dt
				branchTip, hasBranchTip = dt, true
			}
		}
		files := map[string]string{}
		if hasBranchTip {
			files = c.store.FilesAt(repo, branchTip)
		}
		current, hasCurrent := files[path]

		if method == "PUT" {
			contentB64 := bodyString(c, "content")
			if contentB64 == "" {
				jsonError(c.w, 422, "Content is required")
				return
			}
			if hasCurrent && givenSha != "" && givenSha != current {
				jsonError(c.w, 409, "The sha does not match the current blob")
				return
			}
			if hasCurrent && givenSha == "" {
				jsonError(c.w, 409, "The sha was not supplied for an existing file")
				return
			}
			data, err := decodeBase64Lenient(contentB64)
			if err != nil {
				jsonError(c.w, 422, "Content is required")
				return
			}
			blobOID := Sha1Hex(ObjBlob, data)
			repo.Objects[blobOID] = &GitObject{Type: ObjBlob, Data: data}
			files[path] = blobOID
			isNew := !hasCurrent
			commitOID := commitFiles(c.store, repo, branchName, files, message)
			c.store.InvalidateAdverts(repo)
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			status := 200
			if isNew {
				status = 201
			}
			writeJSON(c.w, status, contentPutView{
				Content: contentPutInner{
					Name: name, Path: path, SHA: blobOID, Size: len(data),
					Type: "file", Encoding: "base64", Content: contentB64,
				},
				Commit: commitSHAView{SHA: commitOID},
			})
			return
		}

		// DELETE
		if !hasCurrent {
			jsonError(c.w, 404, "Not Found")
			return
		}
		if givenSha != "" && givenSha != current {
			jsonError(c.w, 409, "The sha does not match the current blob")
			return
		}
		delete(files, path)
		commitOID := commitFiles(c.store, repo, branchName, files, message)
		c.store.InvalidateAdverts(repo)
		writeJSON(c.w, 200, commitOnlyView{Commit: commitSHAView{SHA: commitOID}})
		return
	}

	jsonError(c.w, 404, "Not Found")
}

// dirListing returns the directory listing for dir inside a commit's tree,
// or ok=false when it is not a directory.
func dirListing(repo *Repo, commitOID, dir string) ([]dirEntryView, bool) {
	obj, ok := repo.Objects[commitOID]
	if !ok || obj.Type != ObjCommit {
		return nil, false
	}
	treeOID := ParseCommit(obj.Data).Tree
	if dir != "" {
		parts := strings.Split(dir, "/")
		for _, part := range parts {
			treeObj, ok := repo.Objects[treeOID]
			if !ok || treeObj.Type != ObjTree {
				return nil, false
			}
			found := false
			for _, e := range ParseTree(treeObj.Data) {
				if e.Name == part && e.Mode == "40000" {
					treeOID = e.OID
					found = true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
	}
	treeObj, ok := repo.Objects[treeOID]
	if !ok || treeObj.Type != ObjTree {
		return nil, false
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	out := make([]dirEntryView, 0)
	for _, e := range ParseTree(treeObj.Data) {
		isDir := e.Mode == "40000"
		ent := dirEntryView{Type: "file", Name: e.Name, Path: prefix + e.Name, SHA: e.OID}
		if isDir {
			ent.Type = "dir"
		} else {
			sz := 0
			if blob, ok := repo.Objects[e.OID]; ok {
				sz = len(blob.Data)
			}
			ent.Size = &sz
		}
		out = append(out, ent)
	}
	return out, true
}

func handleIssues(c *restCtx, repo *Repo, rest []string) {
	method := c.method

	// POST / GET /issues
	if len(rest) == 0 {
		if method == "POST" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			title := bodyString(c, "title")
			if title == "" {
				jsonError(c.w, 422, "Title is required")
				return
			}
			var labels []string
			if raw, ok := c.json["labels"]; ok {
				if arr, ok := raw.([]any); ok {
					labels = []string{}
					for _, l := range arr {
						if s, ok := l.(string); ok {
							labels = append(labels, s)
						}
					}
				}
			}
			for _, name := range labels {
				if _, ok := repo.Labels[name]; !ok {
					jsonError(c.w, 422, "Label '"+name+"' does not exist")
					return
				}
			}
			repo.IssueSeq++
			issue := &Issue{
				ID:         c.store.NextSeq(),
				Number:     repo.IssueSeq,
				Title:      title,
				Body:       bodyString(c, "body"),
				State:      "open",
				Labels:     labels,
				CreatedSeq: c.store.NextSeq(),
			}
			repo.Issues[issue.Number] = issue
			writeJSONBytes(c.w, 201, issueJSON(repo, issue))
			return
		}
		if method == "GET" {
			state := "open"
			if v, ok := c.query["state"]; ok {
				state = v
			}
			direction := "desc"
			if v, ok := c.query["direction"]; ok {
				direction = v
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for _, iss := range repo.Issues {
				if state == "all" || iss.State == state {
					list = append(list, iss)
				}
			}
			if direction == "asc" {
				sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq < list[j].CreatedSeq })
			} else {
				sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
			}
			page, perPage := pageParams(c.r)
			paged := paginate(list, page, perPage)
			views := make([]issueView, 0, len(paged))
			for _, iss := range paged {
				views = append(views, issueObject(repo, iss))
			}
			writeJSONExtra(c.w, 200, views, linkExtra(c, page, perPage, len(list)))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	number, ok := parseNumber(rest[0])
	if !ok {
		jsonError(c.w, 404, "Not Found")
		return
	}
	issue, ok := repo.Issues[number]
	if !ok {
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET / PATCH /issues/{n}
	if len(rest) == 1 {
		if method == "GET" {
			writeJSONBytes(c.w, 200, issueJSON(repo, issue))
			return
		}
		if method == "PATCH" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			if v, ok := c.json["title"]; ok {
				if s, ok := v.(string); ok {
					issue.Title = s
				}
			}
			if v, ok := c.json["body"]; ok {
				if s, ok := v.(string); ok {
					issue.Body = s
				}
			}
			if v, ok := c.json["state"]; ok {
				if s, ok := v.(string); ok && (s == "open" || s == "closed") {
					issue.State = s
				}
			}
			issue.JSONCache = nil
			issue.JSONCacheLen = 0
			writeJSON(c.w, 200, issueObject(repo, issue))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// /issues/{n}/comments
	if len(rest) == 2 && rest[1] == "comments" {
		if method == "GET" {
			auxMu.Lock()
			issueOf := commentIssue[c.store]
			auxMu.Unlock()
			all := make([]*Comment, 0)
			for _, cm := range repo.Comments {
				if issueOf != nil && issueOf[cm.ID] == number {
					all = append(all, cm)
				}
			}
			sort.Slice(all, func(i, j int) bool { return all[i].CreatedSeq < all[j].CreatedSeq })
			page, perPage := pageParams(c.r)
			paged := paginate(all, page, perPage)
			views := make([]commentView, 0, len(paged))
			for _, cm := range paged {
				views = append(views, commentView{
					ID: cm.ID, Body: cm.Body, User: viewerUser(),
					CreatedAt: NowISO, UpdatedAt: NowISO,
				})
			}
			writeJSONExtra(c.w, 200, views, linkExtra(c, page, perPage, len(all)))
			return
		}
		if method == "POST" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			commentBody := bodyString(c, "body")
			if commentBody == "" {
				jsonError(c.w, 422, "Body is required")
				return
			}
			repo.CommentSeq++
			comment := &Comment{ID: c.store.NextSeq(), Body: commentBody, CreatedSeq: c.store.NextSeq()}
			repo.Comments[comment.ID] = comment
			auxMu.Lock()
			m := commentIssue[c.store]
			if m == nil {
				m = map[int]int{}
				commentIssue[c.store] = m
			}
			m[comment.ID] = number
			auxMu.Unlock()
			writeJSON(c.w, 201, commentView{
				ID: comment.ID, Body: comment.Body, User: viewerUser(),
				CreatedAt: NowISO, UpdatedAt: NowISO,
			})
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	jsonError(c.w, 404, "Not Found")
}

func handlePulls(c *restCtx, repo *Repo, rest []string) {
	method := c.method

	// POST / GET /pulls
	if len(rest) == 0 {
		if method == "POST" {
			if c.json == nil {
				jsonError(c.w, 400, "Problems parsing JSON")
				return
			}
			title := bodyString(c, "title")
			headRef := bodyString(c, "head")
			baseRef := bodyString(c, "base")
			if baseRef == "" {
				baseRef = repo.DefaultBranch
			}
			if title == "" || headRef == "" {
				jsonError(c.w, 422, "Title and head are required")
				return
			}
			headTip, headOK := repo.Refs["refs/heads/"+headRef]
			baseTip, baseOK := repo.Refs["refs/heads/"+baseRef]
			if !headOK || !baseOK {
				jsonError(c.w, 422, "Head or base branch does not exist")
				return
			}
			for _, p := range repo.Pulls {
				if p.State == "open" && p.HeadRef == headRef && p.BaseRef == baseRef {
					jsonError(c.w, 422, "A pull request already exists for this head and base")
					return
				}
			}
			baseTree := ""
			if obj, ok := repo.Objects[baseTip]; ok && obj.Type == ObjCommit {
				baseTree = ParseCommit(obj.Data).Tree
			}
			headTree := ""
			if obj, ok := repo.Objects[headTip]; ok && obj.Type == ObjCommit {
				headTree = ParseCommit(obj.Data).Tree
			}
			repo.PullSeq++
			pull := &Pull{
				ID:         c.store.NextSeq(),
				Number:     repo.PullSeq,
				Title:      title,
				Body:       bodyString(c, "body"),
				State:      "open",
				HeadRef:    headRef,
				BaseRef:    baseRef,
				HeadSHA:    headTip,
				BaseSHA:    baseTip,
				Merged:     false,
				Files:      DiffTrees(repo.Objects, baseTree, headTree),
				CreatedSeq: c.store.NextSeq(),
			}
			repo.Pulls[pull.Number] = pull
			writeJSON(c.w, 201, pullObject(c.store, repo, pull))
			return
		}
		if method == "GET" {
			state := "open"
			if v, ok := c.query["state"]; ok {
				state = v
			}
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				if state == "all" || p.State == state {
					list = append(list, p)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].CreatedSeq > list[j].CreatedSeq })
			page, perPage := pageParams(c.r)
			paged := paginate(list, page, perPage)
			views := make([]pullView, 0, len(paged))
			for _, p := range paged {
				views = append(views, pullObject(c.store, repo, p))
			}
			writeJSONExtra(c.w, 200, views, linkExtra(c, page, perPage, len(list)))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	number, ok := parseNumber(rest[0])
	if !ok {
		jsonError(c.w, 404, "Not Found")
		return
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET /pulls/{n}
	if len(rest) == 1 {
		if method == "GET" {
			writeJSON(c.w, 200, pullObject(c.store, repo, pull))
			return
		}
		jsonError(c.w, 404, "Not Found")
		return
	}

	// GET /pulls/{n}/files
	if len(rest) == 2 && rest[1] == "files" {
		if method != "GET" {
			jsonError(c.w, 404, "Not Found")
			return
		}
		views := make([]pullFileView, 0, len(pull.Files))
		for _, f := range pull.Files {
			views = append(views, pullFileView{
				Filename: f.Filename, Status: f.Status,
				Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes,
			})
		}
		writeJSON(c.w, 200, views)
		return
	}

	// PUT /pulls/{n}/merge
	if len(rest) == 2 && rest[1] == "merge" {
		if method != "PUT" {
			jsonError(c.w, 404, "Not Found")
			return
		}
		if pull.State != "open" || pull.Merged {
			jsonError(c.w, 405, "Pull Request is not mergeable")
			return
		}
		if repoPullConflicted(c.store, repo, pull) {
			jsonError(c.w, 405, "Merge conflict")
			return
		}
		headTip, headOK := repo.Refs["refs/heads/"+pull.HeadRef]
		baseTip, baseOK := repo.Refs["refs/heads/"+pull.BaseRef]
		if !headOK || !baseOK {
			jsonError(c.w, 405, "Pull Request is not mergeable")
			return
		}
		headTree := ""
		if obj, ok := repo.Objects[headTip]; ok && obj.Type == ObjCommit {
			headTree = ParseCommit(obj.Data).Tree
		}
		commit := BuildCommit(headTree, []string{baseTip},
			"Merge pull request #"+strconv.Itoa(pull.Number)+" from "+pull.HeadRef, "", "", FixedEpochSec)
		sha := Sha1Hex(ObjCommit, commit)
		repo.Objects[sha] = &GitObject{Type: ObjCommit, Data: commit}
		repo.Refs["refs/heads/"+pull.BaseRef] = sha
		pull.State = "closed"
		pull.Merged = true
		mergedAt := NowISO
		pull.MergedAt = &mergedAt
		pull.MergeCommitSHA = &sha
		pull.JSONCache = nil
		c.store.InvalidateAdverts(repo)
		writeJSON(c.w, 200, mergeView{SHA: sha, Merged: true, Message: "Pull Request successfully merged"})
		return
	}

	jsonError(c.w, 404, "Not Found")
}
