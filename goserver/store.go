// store.go — git objects, packs, the Store, and diff. Agent A.
//
// 1:1 translation of gh-p4ab/store.ts (Node A+B @64dd411, local 996.33),
// which is the protocol spec. Determinism: no wall clock, no unseeded
// randomness; every map iteration that can reach output bytes is sorted
// first (repo lists, pack objects, tree entries).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- git objects

var typeNum = map[ObjectType]int{
	ObjCommit: 1,
	ObjTree:   2,
	ObjBlob:   3,
	ObjTag:    4,
}

var numType = map[int]ObjectType{
	1: ObjCommit,
	2: ObjTree,
	3: ObjBlob,
	4: ObjTag,
}

// Sha1Hex is the git object id: SHA-1 over "<type> <size>\0" + bytes.
func Sha1Hex(typ ObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// SortTreeEntries applies git's tree sort: names compared byte-wise,
// directories as if ending in '/'. Returns a new slice.
func SortTreeEntries(entries []TreeEntry) []TreeEntry {
	out := make([]TreeEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		an := out[i].Name
		if out[i].Mode == "40000" {
			an += "/"
		}
		bn := out[j].Name
		if out[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	return out
}

// BuildTree serializes tree entries to raw tree bytes.
func BuildTree(entries []TreeEntry) []byte {
	var buf bytes.Buffer
	for _, e := range SortTreeEntries(entries) {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ParseTree parses raw tree bytes back into entries.
func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], 0x20)
		if sp == -1 {
			break
		}
		sp += i
		nul := bytes.IndexByte(data[sp:], 0x00)
		if nul == -1 {
			break
		}
		nul += sp
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[i:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		i = nul + 21
	}
	return entries
}

// ParseCommit parses a commit object's headers and message.
func ParseCommit(data []byte) ParsedCommit {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split != -1 {
		head = text[:split]
		message = text[split+2:]
	}
	var out ParsedCommit
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.Committer = line[10:]
		}
	}
	out.Message = message
	return out
}

// BuildCommit builds a commit object. Identity defaults to the conformance
// identity, timestamp to FixedEpochSec when 0.
func BuildCommit(tree string, parents []string, message, author, committer string, timestamp int64) []byte {
	if timestamp == 0 {
		timestamp = FixedEpochSec
	}
	if author == "" {
		author = CommitIdent
	}
	if committer == "" {
		committer = CommitIdent
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", author, timestamp)
	fmt.Fprintf(&b, "committer %s %d +0000\n", committer, timestamp)
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// TreeOIDFromFiles builds a tree for a flat path -> blob-oid map, storing
// every intermediate tree in objects. Returns the root tree oid.
func TreeOIDFromFiles(objects map[string]*GitObject, files map[string]string) string {
	var buildDir func(prefix string) string
	buildDir = func(prefix string) string {
		var entries []TreeEntry
		subdirs := map[string]bool{}
		var subdirNames []string
		for path, oid := range files {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest := path[len(prefix):]
			if rest == "" {
				continue
			}
			if slash := strings.Index(rest, "/"); slash == -1 {
				entries = append(entries, TreeEntry{Mode: "100644", Name: rest, OID: oid})
			} else {
				dir := rest[:slash]
				if !subdirs[dir] {
					subdirs[dir] = true
					subdirNames = append(subdirNames, dir)
				}
			}
		}
		sort.Strings(subdirNames)
		for _, dir := range subdirNames {
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: buildDir(prefix + dir + "/")})
		}
		buf := BuildTree(entries)
		oid := Sha1Hex(ObjTree, buf)
		objects[oid] = &GitObject{Type: ObjTree, Data: buf}
		return oid
	}
	return buildDir("")
}

// FlattenTree flattens a tree into path -> blob-oid, recursing into
// subdirectories.
func FlattenTree(objects map[string]*GitObject, treeOID string) map[string]string {
	out := map[string]string{}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		obj := objects[treeOID]
		if obj == nil || obj.Type != ObjTree {
			return
		}
		for _, e := range ParseTree(obj.Data) {
			if e.Mode == "40000" {
				walk(e.OID, prefix+e.Name+"/")
			} else {
				out[prefix+e.Name] = e.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

// IsAncestor is true when ancestor is reachable from tip by parent links.
func IsAncestor(objects map[string]*GitObject, ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := objects[cur]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, p := range ParseCommit(obj.Data).Parents {
			stack = append(stack, p)
		}
	}
	return false
}

var tagObjectRe = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})`)

// CollectObjects returns every object reachable from tips, not crossing into
// exclude (objects the other side already holds), keyed by oid.
func CollectObjects(objects map[string]*GitObject, tips []string, exclude map[string]bool) map[string]*GitObject {
	out := map[string]*GitObject{}
	seen := map[string]bool{}
	for oid := range exclude {
		seen[oid] = true
	}
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = obj
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		case ObjTag:
			if m := tagObjectRe.FindSubmatch(obj.Data); m != nil {
				stack = append(stack, string(m[1]))
			}
		}
	}
	return out
}

// WalkCommits returns all commit oids reachable from tip, newest-first by
// walk order (parents after child).
func WalkCommits(objects map[string]*GitObject, tip string) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		out = append(out, oid)
		for _, p := range ParseCommit(obj.Data).Parents {
			stack = append(stack, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- pack files

func packEntryHeader(typeNum int, size int) []byte {
	var out []byte
	b := byte((typeNum << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, b)
	return out
}

// WritePack serializes objects into a pack v2 (plain objects only, no
// deltas). Callers pass objs sorted by OID ascending (deterministic replay).
func WritePack(objs []*GitObject) []byte {
	var parts bytes.Buffer
	head := make([]byte, 12)
	copy(head[0:4], "PACK")
	binary.BigEndian.PutUint32(head[4:8], 2)
	binary.BigEndian.PutUint32(head[8:12], uint32(len(objs)))
	parts.Write(head)
	for _, obj := range objs {
		parts.Write(packEntryHeader(typeNum[obj.Type], len(obj.Data)))
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(obj.Data)
		zw.Close()
		parts.Write(zb.Bytes())
	}
	body := parts.Bytes()
	trailer := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, trailer[:]...)
	return out
}

// PackObject is one object read back from a pack, with its oid.
type PackObject struct {
	Type ObjectType
	Data []byte
	OID  string
}

// countingReader tracks how many bytes the zlib stream consumed. It
// implements io.ByteReader so compress/zlib (via flate) reads it directly
// without a bufio layer that would read ahead past the stream end.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	var b [1]byte
	n, err := c.r.Read(b[:])
	c.n += int64(n)
	return b[0], err
}

// ReadPack parses a pack v2. Each entry's zlib stream length comes from the
// counting reader — one inflate per entry, no guessing. Errors on a bad
// magic, a bad version, a bad trailer checksum, or a delta entry (this forge
// never sends deltas and the suite's client never writes them).
func ReadPack(buf []byte) ([]*PackObject, error) {
	if len(buf) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("not a pack file")
	}
	if binary.BigEndian.Uint32(buf[4:8]) != 2 {
		return nil, errors.New("unsupported pack version")
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	trailer := buf[len(buf)-20:]
	expect := sha1.Sum(buf[:len(buf)-20])
	if !bytes.Equal(expect[:], trailer) {
		return nil, errors.New("pack checksum mismatch")
	}
	out := make([]*PackObject, 0, count)
	off := 12
	end := len(buf) - 20
	for n := uint32(0); n < count; n++ {
		if off >= end {
			return nil, errors.New("pack truncated")
		}
		b := buf[off]
		off++
		typeNum := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= end {
				return nil, errors.New("pack truncated")
			}
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		typ, ok := numType[int(typeNum)]
		if !ok {
			return nil, fmt.Errorf("unsupported pack object type %d", typeNum)
		}
		if typeNum == 6 || typeNum == 7 {
			return nil, errors.New("delta objects are not accepted here")
		}
		cr := &countingReader{r: bytes.NewReader(buf[off:end])}
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return nil, errors.New("pack entry does not inflate")
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, errors.New("pack entry does not inflate")
		}
		consumed := int(cr.n)
		if consumed <= 0 || len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		off += consumed
		out = append(out, &PackObject{Type: typ, Data: data, OID: Sha1Hex(typ, data)})
	}
	return out, nil
}

// ---------------------------------------------------------------- the store

// Concurrency: the router serialises dispatch with one global mutex
// (mirroring Node's single-threaded execution), so no locking is done in
// this file. No goroutines touch the store, and no shared mutable state
// lives outside the Store itself.
func fetchCachePut(repo *Repo, key string, body []byte) {
	if len(repo.FetchCache) >= FetchCacheMax {
		// Evict one entry. Node evicts the oldest (insertion order); Go
		// maps have no order, so the victim is arbitrary — unobservable,
		// because a rebuilt fetch response is byte-identical to a cached
		// one (the build is a pure function of request + repo state).
		for k := range repo.FetchCache {
			delete(repo.FetchCache, k)
			break
		}
	}
	repo.FetchCache[key] = body
}

// LineDiff counts added/deleted lines between two texts (LCS, one row).
func LineDiff(oldText, newText string) (additions, deletions int) {
	var a, b []string
	if oldText != "" {
		a = strings.Split(oldText, "\n")
	}
	if newText != "" {
		b = strings.Split(newText, "\n")
	}
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev = cur
	}
	common := prev[len(b)]
	return len(b) - common, len(a) - common
}

// DiffTrees is the file-level diff between two trees: one PullFile per
// changed path.
func DiffTrees(objects map[string]*GitObject, baseTree, headTree string) []PullFile {
	var base, head map[string]string
	if baseTree == "" {
		base = map[string]string{}
	} else {
		base = FlattenTree(objects, baseTree)
	}
	if headTree == "" {
		head = map[string]string{}
	} else {
		head = FlattenTree(objects, headTree)
	}
	namesSet := map[string]bool{}
	for name := range base {
		namesSet[name] = true
	}
	for name := range head {
		namesSet[name] = true
	}
	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	var files []PullFile
	blobText := func(oid string) string {
		blob := objects[oid]
		if blob == nil {
			return ""
		}
		return string(blob.Data)
	}
	lineCount := func(text string) int {
		if text == "" {
			return 0
		}
		return len(strings.Split(text, "\n"))
	}
	for _, name := range names {
		b, bOK := base[name]
		h, hOK := head[name]
		if bOK && hOK && b == h {
			continue
		}
		switch {
		case !bOK && hOK:
			lines := lineCount(blobText(h))
			files = append(files, PullFile{Filename: name, Status: "added", Additions: lines, Deletions: 0, Changes: lines})
		case hOK == false && bOK:
			lines := lineCount(blobText(b))
			files = append(files, PullFile{Filename: name, Status: "removed", Additions: 0, Deletions: lines, Changes: lines})
		default:
			add, del := LineDiff(blobText(b), blobText(h))
			files = append(files, PullFile{Filename: name, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return files
}

// NewStore creates an empty store.
func NewStore() *Store {
	return &Store{Repos: map[string]*Repo{}}
}

// NextSeq returns the next global sequence number.
func (s *Store) NextSeq() int {
	s.globalSeq++
	return s.globalSeq
}

// CreateRepo creates a repo owned by the viewer login.
func (s *Store) CreateRepo(name string, isPrivate bool, description string) *Repo {
	s.repoSeq++
	repo := &Repo{
		ID:            s.repoSeq,
		Name:          name,
		Owner:         ViewerLogin,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: DefaultBranch,
		Refs:          map[string]string{},
		Objects:       map[string]*GitObject{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		Labels:        map[string]*Label{},
		Comments:      map[int]*Comment{},
		CreatedSeq:    s.NextSeq(),
		FetchCache:    map[string][]byte{},
		LsRefsCache:   map[string][]byte{},
	}
	s.Repos[name] = repo
	return repo
}

// GetRepo looks a repo up by name, or nil.
func (s *Store) GetRepo(name string) *Repo {
	return s.Repos[name]
}

// DeleteRepo deletes a repo by name.
func (s *Store) DeleteRepo(name string) bool {
	if _, ok := s.Repos[name]; !ok {
		return false
	}
	delete(s.Repos, name)
	return true
}

// InvalidateAdverts clears the cached advertisements and the fetch /
// ls-refs response caches for a repo. Every mutation path (push, contents
// PUT/DELETE, ref creation, merge) calls this.
func (s *Store) InvalidateAdverts(repo *Repo) {
	repo.AdvV0Upload = nil
	repo.AdvV0Receive = nil
	repo.AdvV2 = nil
	repo.FetchCache = map[string][]byte{}
	repo.LsRefsCache = map[string][]byte{}
}

// RefTip returns the commit a ref points at, or ("", false).
func (s *Store) RefTip(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveRef resolves a branch name, a full ref, or a raw sha to a commit
// oid, or ("", false).
func (s *Store) ResolveRef(repo *Repo, ref string) (string, bool) {
	if ref == "HEAD" {
		ref = "refs/heads/" + repo.DefaultBranch
	}
	if sha40Re.MatchString(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, "refs/") {
		oid, ok := repo.Refs[ref]
		return oid, ok
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	oid, ok := repo.Refs["refs/tags/"+ref]
	return oid, ok
}

// FilesAt returns the flat file map of a commit's tree (empty when the
// commit has no tree).
func (s *Store) FilesAt(repo *Repo, commitOID string) map[string]string {
	obj := repo.Objects[commitOID]
	if obj == nil || obj.Type != ObjCommit {
		return map[string]string{}
	}
	return FlattenTree(repo.Objects, ParseCommit(obj.Data).Tree)
}

// CreateIssue creates an issue in a repo (the next number, open, fresh id).
func (s *Store) CreateIssue(repo *Repo, title, body string) *Issue {
	repo.IssueSeq++
	issue := &Issue{
		ID:         s.NextSeq(),
		Number:     repo.IssueSeq,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     []string{},
		CreatedSeq: s.NextSeq(),
	}
	repo.Issues[issue.Number] = issue
	return issue
}

// CreatePull creates a pull request between two existing branches, or nil
// when a branch is missing or an open PR already covers the same head+base.
func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef string) *Pull {
	headTip, ok1 := repo.Refs["refs/heads/"+headRef]
	baseTip, ok2 := repo.Refs["refs/heads/"+baseRef]
	if !ok1 || !ok2 {
		return nil
	}
	for _, p := range repo.Pulls {
		if p.State == "open" && p.HeadRef == headRef && p.BaseRef == baseRef {
			return nil
		}
	}
	baseTree := ""
	if baseObj := repo.Objects[baseTip]; baseObj != nil && baseObj.Type == ObjCommit {
		baseTree = ParseCommit(baseObj.Data).Tree
	}
	headTree := ""
	if headObj := repo.Objects[headTip]; headObj != nil && headObj.Type == ObjCommit {
		headTree = ParseCommit(headObj.Data).Tree
	}
	repo.PullSeq++
	pull := &Pull{
		ID:         s.NextSeq(),
		Number:     repo.PullSeq,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    headRef,
		BaseRef:    baseRef,
		HeadSHA:    headTip,
		BaseSHA:    baseTip,
		Files:      DiffTrees(repo.Objects, baseTree, headTree),
		CreatedSeq: s.NextSeq(),
	}
	repo.Pulls[pull.Number] = pull
	return pull
}

// MergePull squash-merges an open, unconflicted PR onto its base branch.
func (s *Store) MergePull(repo *Repo, pull *Pull) bool {
	if pull.State != "open" || pull.Merged {
		return false
	}
	if s.PullConflicted(repo, pull) {
		return false
	}
	headTip, ok1 := repo.Refs["refs/heads/"+pull.HeadRef]
	baseTip, ok2 := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok1 || !ok2 {
		return false
	}
	headTree := ""
	if headObj := repo.Objects[headTip]; headObj != nil && headObj.Type == ObjCommit {
		headTree = ParseCommit(headObj.Data).Tree
	}
	commit := BuildCommit(headTree, []string{baseTip},
		"Merge pull request #"+strconv.Itoa(pull.Number)+" from "+pull.HeadRef, "", "", FixedEpochSec)
	sha := Sha1Hex(ObjCommit, commit)
	repo.Objects[sha] = &GitObject{Type: ObjCommit, Data: commit}
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = nil
	pull.MergeCommitSHA = &sha
	pull.JSONCache = nil
	s.InvalidateAdverts(repo)
	return true
}

// PullConflicted is true when the PR is conflicted: a path it changes
// differs between its recorded base and the current tip of its base branch.
func (s *Store) PullConflicted(repo *Repo, pull *Pull) bool {
	baseTip, ok := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok {
		return false
	}
	if baseTip == pull.BaseSHA {
		return false
	}
	baseFiles := s.FilesAt(repo, pull.BaseSHA)
	tipFiles := s.FilesAt(repo, baseTip)
	for _, file := range pull.Files {
		b, bOK := baseFiles[file.Filename]
		t, tOK := tipFiles[file.Filename]
		if !bOK {
			b = ""
		}
		if !tOK {
			t = ""
		}
		// Node compares Map.get results: undefined !== undefined is false,
		// so a file missing on both sides is not a conflict. Replicate by
		// comparing presence too.
		if bOK != tOK || b != t {
			return true
		}
	}
	return false
}
