// types.go — shared data types and constants for the Go forge port.
//
// This file is the CONTRACT: every build agent builds against these types.
// Do NOT rename, retype, or move these definitions. Method bodies live in
// the files named in GO-CONTRACT.md. All types are in package main (single
// Go module, no internal packages) so every file sees them.
//
// Determinism rules (scored: the suite's export must be byte-identical
// across two runs):
//   - NEVER read a wall clock or an unseeded random source in request
//     handling. All timestamps come from NowISO / TrialEpochSec (which read
//     MAKEFUN_TRIAL_NOW when the trial sets it, else the fixed epoch).
//   - NEVER iterate a Go map when the iteration order can reach output
//     bytes (JSON lists, pack object order, state dumps). Sort keys first.
//   - JSON encoding must match Node's JSON.stringify byte-for-byte for the
//     cached payloads: use json.Encoder with SetEscapeHTML(false) and a
//     trailing "\n" (see sendJson in server.go).
package main

import (
	"os"
	"time"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const FixedEpochSec = 1700000000
const FixedISO = "2023-11-14T22:13:20.000Z"
const CommitIdent = "Conformance <conformance@make.fun>"
const DefaultBranch = "main"
const FetchCacheMax = 16
const JSONType = "application/json; charset=utf-8"

// NowISO is the trial clock: MAKEFUN_TRIAL_NOW when the env sets it, else
// the fixed epoch. Computed once at startup, never from a live clock.
var NowISO = fixedNowISO()

func fixedNowISO() string {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if ms, err := time.Parse("2006-01-02T15:04:05.999Z07:00", raw); err == nil {
			_ = ms
		}
	}
	return FixedISO
}

// TrialEpochSec is NowISO as unix seconds (Node: Math.floor(Date.parse(NOW_ISO)/1000)).
func TrialEpochSec() int64 {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", NowISO)
	if err != nil {
		return FixedEpochSec
	}
	return t.Unix()
}

// ---------------------------------------------------------------- git objects

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType
	Data []byte
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ---------------------------------------------------------------- the store

type Issue struct {
	ID           int
	Number       int
	Title        string
	Body         string
	State        string // "open" | "closed"
	Labels       []string
	CreatedSeq   int
	JSONCache    []byte // rendered JSON bytes, nil when not built
	JSONCacheLen int
}

type Comment struct {
	ID         int
	Body       string
	CreatedSeq int
}

type PullFile struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	Files          []PullFile
	CreatedSeq     int
	JSONCache      []byte // rendered JSON bytes, nil when not built
}

type Label struct {
	Name  string
	Color string
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	IsPrivate     bool
	Description   string
	DefaultBranch string
	Refs          map[string]string
	Objects       map[string]*GitObject
	Issues        map[int]*Issue
	IssueSeq      int
	Pulls         map[int]*Pull
	PullSeq       int
	Labels        map[string]*Label
	Comments      map[int]*Comment
	CommentSeq    int
	CreatedSeq    int
	JSONCache     []byte // rendered JSON bytes, nil when not built
	JSONCacheLen  int
	AdvV0Upload   []byte
	AdvV0Receive  []byte
	AdvV2         []byte
	// FetchCache: full fetch responses (v0 and v2) keyed by a canonical
	// request fingerprint. Dropped by InvalidateAdverts on any ref/object
	// change. Bounded to FetchCacheMax entries, oldest evicted first.
	FetchCache map[string][]byte
	// LsRefsCache: ls-refs responses keyed by the joined ref-prefix list.
	LsRefsCache map[string][]byte
}

type Store struct {
	Repos map[string]*Repo
	// unexported counters, only move forward in creation order.
	repoSeq   int
	globalSeq int
}
