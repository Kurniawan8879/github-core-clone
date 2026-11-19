# GO-CONTRACT.md — interface contract for the Go forge port (github-core-clone-01)

Coordinator owns this file. Build agents: follow it EXACTLY. If a signature
here conflicts with what the Node code does, the Node code wins for BEHAVIOUR
but the signature here wins for the GO API — tell the coordinator in your
report if you had to deviate, with the reason.

## Repo layout (~/workspace/makefun/gh-go/, fresh git repo, branch main)

```
main.ts            # Node 22 entrypoint (copied from the gh-p5go probe — DO NOT rewrite;
                   # it spawns ./goserver/forge, waits for "[go] listening on <port>",
                   # registers with $MAKEFUN_ENV_URL, long-polls /done, kills the child)
LICENSE            # MIT
goserver/go.mod    # module forge, go 1.25, NO dependencies (stdlib only)
goserver/types.go  # shared types + constants (DO NOT rename/retype anything in it)
goserver/store.go  # Agent A: objects, packs, Store methods, diff
goserver/git.go    # Agent A: pkt-line, upload-pack v2, receive-pack v0, ServeGit
goserver/server.go # Agent B: HTTP server, router, auth, JSON helpers, NewHandler
goserver/rest.go   # Agent B: all REST v3 handlers + JSON rendering
goserver/graphql.go# Agent C: ServeGraphQL, hand-written executor (18 documents)
goserver/mcp.go    # Agent D: ServeMCP, JSON-RPC 2.0 Streamable HTTP, 6 tools
goserver/warmup.go # Agent D: WarmUpForge (throwaway server, fixed data, 10 s watchdog)
goserver/main.go   # Agent D: port argv, NewStore, listen, "[go] listening on <port>", SIGTERM
goserver/build.sh  # Agent D: static build + size check
goserver/forge     # the built static binary (committed; rebuilt by build.sh)
```

## Toolchain

- Go 1.25.4 at `~/workspace/tools/go/go/bin/go` (NOT in PATH; use the full path).
- Build: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .` from `goserver/`.
- Node 22 is available as `node` (for grade-local and for running the kit).
- The Node reference is `~/workspace/makefun/gh-p4ab/` @64dd411 (A+B, local 996.33).
  It is the PROTOCOL SPEC: translate it module by module, byte-for-byte
  behaviour. Read the kit rules first: `~/workspace/makefun/github-core-clone-01-kit/RULES.txt`
  (wire contract: register/done, auth, error shapes) + `competition.yaml`.

## Size budget (HARD — the entry is invalid if it breaks)

- ≤8 MiB in any one file, ≤16 MiB total (source + binary). The probe binary
  was 5,738,680 B. Watch the binary size at EVERY commit (`ls -l forge`);
  if it passes 7.5 MiB, STOP and report to the coordinator immediately.
- No dependencies: Go stdlib only (`net/http`, `crypto/sha1`,
  `compress/zlib`, `encoding/json`, `sort`, `sync`, ...).

## Determinism (scored: the suite's export must be byte-identical across runs)

- NEVER `time.Now()`, `rand`, or any wall clock in request handling. All
  timestamps come from `NowISO` / `TrialEpochSec()` (types.go). Fixed commit
  timestamps: `FixedEpochSec` (1700000000), identity `CommitIdent`.
- NEVER iterate a Go map when the order can reach output bytes. Sort keys
  first (repo lists, issue lists, pack objects, tree entries, state dumps).
- JSON encoding must match Node's `JSON.stringify` byte-for-byte for cached
  payloads: `json.Encoder` with `SetEscapeHTML(false)`, body + `"\n"`.
  (Go still escapes U+2028/U+2029 even with SetEscapeHTML(false); the suite
  data is ASCII — accept that edge case, do not hand-roll an encoder.)
- Request-id counter is a package-level atomic counter starting at 0,
  incremented once per request, formatted `req-<n>` (matches Node).

## Shared helpers (owned by Agent B in server.go; other agents STUB them)

```go
func writeJSON(w http.ResponseWriter, status int, v any)            // body = JSON(v)+"\n", content-type JSONType, x-github-request-id req-<seq>, content-length
func writeJSONBytes(w http.ResponseWriter, status int, body []byte) // pre-rendered bytes (jsonCache), same headers
func jsonError(w http.ResponseWriter, status int, msg string)       // {"message": msg}
func nextRequestID() string                                         // "req-<n>", atomic counter
func checkAuth(r *http.Request) bool                                // Authorization: Bearer <token>; token must equal $MAKEFUN_SHOP_TOKEN;
                                                                    // if that env is empty, any non-empty bearer passes; else 401 "Requires authentication"
func pageParams(r *http.Request) (page, perPage int)                // page default 1 min 1; per_page default 30 clamp 1..100
func paginate[T any](list []T, page, perPage int) []T               // slice
func linkHeader(r *http.Request, page, perPage, total int) string   // absolute next+last Link, only when more pages follow; "" otherwise
```

STUB RULE: if your files call a function owned by another agent, create
`goserver/zz_stub.go` in your worktree with a MINIMAL implementation (just
enough to compile + smoke-test your surface). Test with it, then DELETE
`zz_stub.go` before your final commit — it must NOT be in your final tree.
Your final commit contains ONLY your owned files (plus the shared binary
rebuild if you are Agent D). The coordinator merges all four branches and
verifies `go build ./...` passes.

## Agent A — store.go + git.go (spec: gh-p4ab/store.ts + git.ts)

```go
// store.go — git objects, packs, Store methods, diff. Translate store.ts 1:1.
func Sha1Hex(typ ObjectType, data []byte) string
func SortTreeEntries(entries []TreeEntry) []TreeEntry          // git tree sort: dirs as if ending in '/'
func BuildTree(entries []TreeEntry) []byte
func ParseTree(data []byte) []TreeEntry
func ParseCommit(data []byte) ParsedCommit
func BuildCommit(tree string, parents []string, message, author, committer string, timestamp int64) []byte
//   author/committer default to CommitIdent when empty; timestamp defaults to FixedEpochSec when 0.
func TreeOIDFromFiles(objects map[string]*GitObject, files map[string]string) string
func FlattenTree(objects map[string]*GitObject, treeOID string) map[string]string
func IsAncestor(objects map[string]*GitObject, ancestor, tip string) bool
func CollectObjects(objects map[string]*GitObject, tips []string, exclude map[string]bool) map[string]*GitObject
func WalkCommits(objects map[string]*GitObject, tip string) []string
func WritePack(objs []*GitObject) []byte                       // pack v2, plain objects only, NO deltas; caller passes objs sorted by OID
func ReadPack(buf []byte) ([]*PackObject, error)               // verifies magic/version/trailer checksum; rejects delta entries
type PackObject struct { Type ObjectType; Data []byte; OID string }
func LineDiff(oldText, newText string) (additions, deletions int)
func DiffTrees(objects map[string]*GitObject, baseTree, headTree string) []PullFile
func NewStore() *Store
func (s *Store) NextSeq() int
func (s *Store) CreateRepo(name string, isPrivate bool, description string) *Repo
func (s *Store) GetRepo(name string) *Repo
func (s *Store) DeleteRepo(name string) bool
func (s *Store) InvalidateAdverts(repo *Repo)                 // clears advV0Upload/advV0Receive/advV2/fetchCache/lsRefsCache
func (s *Store) RefTip(repo *Repo, ref string) (string, bool)
func (s *Store) ResolveRef(repo *Repo, ref string) (string, bool) // HEAD -> refs/heads/<default>; 40-hex sha; refs/...; branch then tag
func (s *Store) FilesAt(repo *Repo, commitOID string) map[string]string
func (s *Store) CreateIssue(repo *Repo, title, body string) *Issue
func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef string) *Pull // nil when a branch is missing or an open PR covers same head+base
func (s *Store) MergePull(repo *Repo, pull *Pull) bool
func (s *Store) PullConflicted(repo *Repo, pull *Pull) bool

// git.go — smart HTTP. Translate git.ts 1:1 (pkt-line framing, v0/v2 adverts,
// fetch + ls-refs response caching keyed exactly like Node's, bounded 16).
// ServeGit is called by the router AFTER auth + repo lookup; it handles the
// whole /{owner}/{repo}.git/* surface (info/refs, upload-pack, receive-pack).
func ServeGit(s *Store, repo *Repo, w http.ResponseWriter, r *http.Request)
func PktLine(text string) []byte
```

Pack order rule: `WritePack` callers sort objects by OID ascending before
calling. `ReadPack` must use the zlib stream's consumed length per entry
(Node: `inflateSync(..., {info:true}).engine.bytesWritten`); in Go use
`zlib.NewReader` + `io.ReadAll` per entry and track the reader's consumed
bytes via a counting reader — do NOT guess lengths.

## Agent B — server.go + rest.go (spec: gh-p4ab/app.ts, esp. lines 1-300 rendering + 1000-1165 router)

```go
func NewHandler(s *Store) http.Handler  // router: /healthz, /api/v3/*, /api/graphql -> ServeGraphQL, /mcp -> ServeMCP, /{owner}/{repo}.git/* -> ServeGit
func writeJSON(w http.ResponseWriter, status int, v any)
func writeJSONBytes(w http.ResponseWriter, status int, body []byte)
func jsonError(w http.ResponseWriter, status int, msg string)
func nextRequestID() string
func checkAuth(r *http.Request) bool
func pageParams(r *http.Request) (page, perPage int)
func paginate[T any](list []T, page, perPage int) []T
func linkHeader(r *http.Request, page, perPage, total int) string
// rest.go: every REST v3 handler + the JSON renderers (repoJSON, issueJSON,
// pullJSON, commentJSON, labelJSON, contents handlers, ...). Translate
// app.ts exactly: field names, field order, null vs omitted, created_at /
// updated_at = NowISO, ids/numbers/sequences identical to Node. Reuse the
// Repo/Issue/Pull jsonCache fields exactly like Node (render once, serve
// bytes; invalidate on mutation).
```

Routing notes: `GET /healthz` → 200 `{"ok":true}` (no auth). `GET /api/v3`
→ 200 `{current_user_url, rate_limit_url}` + `x-oauth-scopes: repo, read:org`.
Every REST reply is `application/json` (never HTML) and carries a non-empty
`X-GitHub-Request-Id`. Errors carry `{"message": "..."}`. A request without
a valid Bearer is 401. A repo under another owner is 404, never 403.
`/api/v3` sub-paths split on `/` ignoring empty segments (Node: `split('/').filter(x => x !== '')`).

## Agent C — graphql.go (spec: gh-p4ab/graphql.ts, 855 lines)

```go
func ServeGraphQL(s *Store, w http.ResponseWriter, r *http.Request) // POST /api/graphql only
```

Translate the hand-written executor for the suite's 18 documents 1:1:
same query/mutation shapes as `gh` and `github-mcp-server` send, same
introspection answers for Issue/PullRequest/Repository/SearchType, same
error shapes. No GraphQL library exists in stdlib — hand-roll the parser +
executor exactly like Node does. Read graphql.ts in full before writing a
line; list the 18 documents in your report.

## Agent D — mcp.go + warmup.go + main.go + build.sh (spec: gh-p4ab/mcp.ts, warmup.ts, main.ts)

```go
func ServeMCP(s *Store, w http.ResponseWriter, r *http.Request) // POST /mcp only; JSON-RPC 2.0 Streamable HTTP, era 2026-07-28 + legacy initialize fallback; 6 tools incl. create_repo/get_repo/create_issue/list_issues/create_pull_request/merge_pull_request
func WarmUpForge(port int)                                      // replicate warmup.ts: throwaway handler on its own ephemeral port, fixed synthetic data, 300 GETs + 20 writes + 30 pushes (read the full file for the exact sequence), 10 s watchdog, best-effort, never fails boot
func main()                                                      // argv[1] port (or $PORT); NewStore(); NewHandler(store); listen; print "[go] listening on <port>" to stdout; WarmUpForge(port) unless MAKEFUN_WARMUP_OFF=1; serve until SIGTERM then exit 0
```

build.sh (run from goserver/):
```sh
GO=~/workspace/tools/go/go/bin/go
CGO_ENABLED=0 $GO build -trimpath -ldflags="-s -w" -o forge .
ls -l forge  # MUST stay < 8 MiB; abort + report if it passes 7.5 MiB
```
Also: `go vet ./...` must pass. The binary is COMMITTED (it is vendored
source per the rules). Rebuild it whenever any .go file changes.

## Commit conventions (transcript gate — follow exactly)

- First commit in your branch, BEFORE any code: `makefun anchor: drand round 34021932`
  (empty commit or LICENSE only — mirrors the Node builds).
- Then small, focused commits (`go: store objects + packs`, `go: git smart HTTP`, ...).
- NEVER commit credentials, tokens, or anything under `~/.makefun/`, `~/.config/`.
- NEVER rewrite history. NEVER touch `gh-entry`, `gh-p4ab`, `gh-p4a/b/c`,
  `gh-p5a/b/go`, `gh-submit`, `gh-submit-ab`, or any submitted branch.
- Keep the tree clean at every commit; `go build` + `go vet` pass on your
  files + types.go + your zz_stub.go (the stub is deleted before the final commit).

## Smoke tests (each agent, before reporting done)

- Agent A: pack round-trip (write → read → identical objects), tree/commit
  build/parse round-trip, a push/fetch cycle against your own ServeGit via
  `httptest` or a real `net.Listen` server (create repo in the store, push a
  pack, fetch it back, compare oids). Determinism: run the same sequence
  twice, byte-identical outputs.
- Agent B: drive every REST route you implement with `curl` against a
  standalone run (main.ts --standalone or a test main): create/list/get/
  update repo, issue, comment, label, PR, merge, contents GET/PUT/DELETE.
  Compare response bodies byte-for-byte against the Node forge running the
  same sequence (run gh-p4ab with `node main.ts --standalone` on another
  port). Any byte difference is a bug — fix it or report it.
- Agent C: run the 18 documents from graphql.ts against your executor and
  diff responses byte-for-byte against the Node forge.
- Agent D: MCP initialize + all 6 tools via curl against a standalone run;
  warmup runs without error and finishes inside the 10 s watchdog; the
  binary starts, prints the listening line, answers /healthz, dies cleanly
  on SIGTERM (exit 0). Full `build.sh` run + size check.

## Deadlines (hard)

- Code complete + smoke-tested: ~Fri 2026-11-21 00:00 WIB (11 h from spawn).
- First full grade-local: ~Fri 06:00 WIB. If the port is not gradeable by
  then, the coordinator aborts and keeps 01a1efbb — say so early if you are
  slipping; do not silently burn the window.
- Final grade + kit check: ~Fri 12:00 WIB. Entries close Fri 13:30 WIB.
- Report to the coordinator: your branch name + final commit sha, what you
  built, smoke-test results (with byte-diff evidence vs Node where required),
  binary size if you touched it, problems found, and the exact files you own.

## Bundle note (parent-side, recorded 2026-11-19 13:01 WIB — no code change)

The Node A+B resubmit 01a1efbb was REJECTED at intake: kit check
`transcripts_run_record` FAILS because the bundle's run record marks the
system prompt "in-session" but no Muse session carries the harness's own
system prompt. This rule applies to entries received from 18 Nov 12:00 UTC.
Fix (parent's job, separate from this port): re-bundle with
`--system-prompt <file>` using the current kit. The Go entry's bundle MUST
do the same, and the kit check must pass `transcripts_run_record` before the
entry is reported submit-ready. Build agents: no action — just keep your
work credential-free and your session ids recordable (the coordinator
records every build-agent session id for the bundle).
