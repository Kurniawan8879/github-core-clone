// warmup.go — startup JIT warm-up for the forge's hot paths.
//
// Why: the first requests a fresh process serves pay lazy compilation and
// optimizing-compiler ramp-up (measured locally in Node: first ~100 GET-issue
// requests ~5x the steady-state latency, first push ~1.4x). Warming here
// moves that cost to before register, where the 20 s boot budget absorbs it.
// In Go the same idea warms the HTTP dispatch, JSON, git pack and MCP paths
// (map growth, zlib tables, handler closures) before the suite arrives.
//
// How: spin up a THROWAWAY handler on its own ephemeral port and drive the
// hot paths through real HTTP with fixed synthetic data, then close it. The
// throwaway has its own Store, so nothing warm-up touches can leak into the
// store the suite sees, and no time-dependent or random value is written
// anywhere: fixed strings, fixed commit timestamps, fixed counts. The
// request-id counter is package-level and advances by the same fixed amount
// on every run, so live and replay stay identical.
//
// Safety: best-effort only. Any failure (or a 10 s watchdog) logs one line
// and returns; main must still register. Warm-up runs after the real server
// listens and before register, so a slow warm-up can only eat boot budget,
// never suite time.
//
// 1:1 translation of gh-p4ab/warmup.ts (A+B @64dd411).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const warmupTimeout = 10 * time.Second

// Sequential GETs of one issue: enough to push the read path past the JIT threshold.
const warmupReadGets = 300

// Sequential issue creations: warms the write path.
const warmupWrites = 20

// Sequential receive-pack pushes: warms readPack/inflate/pushReport/advert rebuild.
const warmupPushes = 30

// warmupClient drives warm-up HTTP calls. Every attempted call counts,
// whether it succeeds or fails (Node counts before the fetch), and failures
// never stop the sequence.
type warmupClient struct {
	http  *http.Client
	count atomic.Int64
	auth  string
}

func newWarmupClient() *warmupClient {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	auth := "Bearer warmup"
	if token != "" {
		auth = "Bearer " + token
	}
	return &warmupClient{
		// No proxy: Node's fetch does not consult proxy env vars.
		http: &http.Client{Transport: &http.Transport{Proxy: nil}},
		auth: auth,
	}
}

func (c *warmupClient) call(method, url string, headers map[string]string, body []byte) {
	c.count.Add(1)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (c *warmupClient) get(base, path string, extra map[string]string) {
	headers := map[string]string{"Authorization": c.auth}
	for k, v := range extra {
		headers[k] = v
	}
	c.call(http.MethodGet, base+path, headers, nil)
}

func (c *warmupClient) postJSON(base, path string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	c.call(http.MethodPost, base+path, map[string]string{
		"Authorization": c.auth,
		"Content-Type":  "application/json",
	}, b)
}

func (c *warmupClient) postRaw(base, path, contentType string, body []byte) {
	c.call(http.MethodPost, base+path, map[string]string{
		"Authorization": c.auth,
		"Content-Type":  contentType,
	}, body)
}

// runWarmupPhase1 is the throwaway-server sequence from warmup.ts. It runs
// in its own goroutine under the 10 s watchdog; it must never block boot.
func runWarmupPhase1(c *warmupClient, base string) {
	// Dispatch + auth + JSON surfaces (no store mutation beyond the repo/issue setup below).
	for i := 0; i < 5; i++ {
		c.get(base, "/healthz", nil)
	}
	for i := 0; i < 3; i++ {
		c.get(base, "/api/v3", nil)
	}
	for i := 0; i < 3; i++ {
		c.get(base, "/api/v3/meta", nil)
	}
	for i := 0; i < 3; i++ {
		c.get(base, "/api/v3/user", nil)
	}
	c.postJSON(base, "/api/v3/user/repos", map[string]any{"name": "warmup", "description": "warmup"})
	c.postJSON(base, "/api/v3/repos/adaiversen/warmup/labels", map[string]any{"name": "bug", "color": "ededed"})
	for i := 0; i < warmupWrites; i++ {
		c.postJSON(base, "/api/v3/repos/adaiversen/warmup/issues", map[string]any{
			"title": fmt.Sprintf("warmup issue %d", i),
			"body":  "warmup body",
		})
	}
	for i := 0; i < 3; i++ {
		c.get(base, "/api/v3/repos/adaiversen/warmup/issues", nil)
	}
	for i := 0; i < 5; i++ {
		c.get(base, "/api/v3/repos/adaiversen/warmup", nil)
	}
	for i := 0; i < 2; i++ {
		c.get(base, "/api/v3/user/repos", nil)
	}
	// The read hot path, past the JIT threshold.
	for i := 0; i < warmupReadGets; i++ {
		c.get(base, "/api/v3/repos/adaiversen/warmup/issues/1", nil)
	}

	// Git advertisements, ls-refs and fetch (cached-response paths included).
	for i := 0; i < 3; i++ {
		c.get(base, "/adaiversen/warmup.git/info/refs?service=git-upload-pack", map[string]string{"Git-Protocol": "version=2"})
		c.get(base, "/adaiversen/warmup.git/info/refs?service=git-upload-pack", nil)
		c.get(base, "/adaiversen/warmup.git/info/refs?service=git-receive-pack", nil)
	}

	// Receive-pack pushes: one 1 KiB change per push, fixed content/timestamps.
	parent := ""
	for i := 0; i < warmupPushes; i++ {
		// Node: Buffer.from(`warmup ${i} `.repeat(113)).subarray(0, 1024) —
		// subarray does NOT pad, so pushes 0-9 carry a 1017-byte blob and
		// pushes 10-29 a 1024-byte blob. Replicate exactly.
		blob := []byte(strings.Repeat(fmt.Sprintf("warmup %d ", i), 113))
		if len(blob) > 1024 {
			blob = blob[:1024]
		}
		blobOID := Sha1Hex(ObjBlob, blob)
		tree := BuildTree([]TreeEntry{{Mode: "100644", Name: "bench-data.txt", OID: blobOID}})
		treeOID := Sha1Hex(ObjTree, tree)
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		commit := BuildCommit(treeOID, parents, fmt.Sprintf("warmup push %d\n", i), CommitIdent, CommitIdent, FixedEpochSec+int64(i))
		commitOID := Sha1Hex(ObjCommit, commit)
		pack := WritePack([]*GitObject{
			{Type: ObjBlob, Data: blob},
			{Type: ObjTree, Data: tree},
			{Type: ObjCommit, Data: commit},
		})
		c.get(base, "/adaiversen/warmup.git/info/refs?service=git-receive-pack", nil)
		oldOID := parent
		if oldOID == "" {
			oldOID = ZeroOID
		}
		body := PktLine(oldOID + " " + commitOID + " refs/heads/main\x00report-status side-band-64k\n")
		body = append(body, []byte("0000")...)
		body = append(body, pack...)
		c.postRaw(base, "/adaiversen/warmup.git/git-receive-pack", "application/x-git-receive-pack-request", body)
		parent = commitOID
	}

	// Upload-pack: ls-refs + a shallow v2 fetch of the warm-up tip.
	if parent != "" {
		lsRefsBody := bytes.Join([][]byte{
			PktLine("command=ls-refs\n"),
			PktLine("peel\n"),
			PktLine("unborn\n"),
			[]byte("0000"),
		}, nil)
		for i := 0; i < 3; i++ {
			c.postRaw(base, "/adaiversen/warmup.git/git-upload-pack", "application/x-git-upload-pack-request", lsRefsBody)
		}
		fetchBody := bytes.Join([][]byte{
			PktLine("command=fetch\n"),
			PktLine("want " + parent + "\n"),
			PktLine("deepen 1\n"),
			PktLine("done\n"),
			[]byte("0000"),
		}, nil)
		for i := 0; i < 3; i++ {
			c.postRaw(base, "/adaiversen/warmup.git/git-upload-pack", "application/x-git-upload-pack-request", fetchBody)
		}
	}

	// GraphQL + MCP: a few calls each so their dispatch is compiled too.
	for i := 0; i < 2; i++ {
		c.postJSON(base, "/api/graphql", map[string]any{"query": `{ __type(name: "Issue") { fields { name } } }`})
		c.postJSON(base, "/api/graphql", map[string]any{"query": `query { repository(owner: "adaiversen", name: "warmup") { name } }`})
	}
	c.postJSON(base, "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "warmup", "version": "1"},
		},
	})
	for i := 0; i < 2; i++ {
		c.postJSON(base, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	}
	c.postJSON(base, "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_repo",
			"arguments": map[string]any{"owner": "adaiversen", "repo": "warmup"},
		},
	})
}

// runWarmupPhase2 warms the REAL server's own request callback. Only
// non-mutating requests: they read an empty store and write no state, so the
// suite sees a pristine forge (the request-id counter advances by the same
// fixed amount every run). No watchdog here, matching Node.
func runWarmupPhase2(realPort int) {
	c := newWarmupClient()
	base := "http://127.0.0.1:" + strconv.Itoa(realPort)
	for i := 0; i < 50; i++ {
		c.get(base, "/healthz", nil)
	}
	for i := 0; i < 20; i++ {
		c.get(base, "/api/v3", nil)
	}
	for i := 0; i < 20; i++ {
		c.get(base, "/api/v3/meta", nil)
	}
	for i := 0; i < 20; i++ {
		c.get(base, "/api/v3/user", nil)
	}
	for i := 0; i < 30; i++ {
		c.get(base, "/api/v3/user/repos", nil)
	}
	for i := 0; i < 30; i++ {
		c.get(base, "/api/v3/repos/adaiversen/__warmup_missing__", nil)
	}
	for i := 0; i < 20; i++ {
		c.get(base, "/__warmup_missing__", nil)
	}
	for i := 0; i < 20; i++ {
		c.get(base, "/adaiversen/__warmup_missing__.git/info/refs?service=git-upload-pack", nil)
	}
	for i := 0; i < 10; i++ {
		c.postRaw(base, "/api/v3/user/repos", "application/json", []byte("{bad json"))
	}
	fmt.Fprintf(os.Stderr, "[warmup] real-server phase: %d requests\n", c.count.Load())
}

// WarmUpForge runs the throwaway warm-up (phase 1, 10 s watchdog) and then
// the real-server warm-up (phase 2). Best-effort: any failure logs one line
// and returns; it must never fail boot.
func WarmUpForge(port int) {
	started := time.Now()

	// Phase 1: throwaway handler on its own ephemeral port, own Store.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warmup] skipped: %v\n", err)
	} else {
		srv := &http.Server{Handler: NewHandler(NewStore())}
		go func() { _ = srv.Serve(ln) }()
		base := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		c := newWarmupClient()
		done := make(chan struct{})
		go func() {
			defer close(done)
			runWarmupPhase1(c, base)
		}()
		select {
		case <-done:
		case <-time.After(warmupTimeout):
		}
		fmt.Fprintf(os.Stderr, "[warmup] %d requests in %d ms\n", c.count.Load(), time.Since(started).Milliseconds())
		_ = srv.Close()
		<-done
	}

	// Phase 2: warm the real server's own request callback.
	if port > 0 {
		runWarmupPhase2(port)
	}
}
