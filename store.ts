// store.ts — the forge's whole state, in memory: git objects, refs, repos, issues,
// pull requests, labels and comments. One store, shared by REST, git smart HTTP,
// GraphQL and MCP, so every surface reads the same objects.
//
// Determinism: nothing here reads a wall clock or an unseeded random source. Commit
// timestamps and created/updated strings come from one fixed epoch (or, when the trial
// sets it, MAKEFUN_TRIAL_NOW). Counters only move forward in creation order.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const FIXED_EPOCH_SEC = 1700000000;
export const FIXED_ISO = '2023-11-14T22:13:20.000Z';
export const COMMIT_IDENT = 'Conformance <conformance@make.fun>';
export const DEFAULT_BRANCH = 'main';

// The trial clock: MAKEFUN_TRIAL_NOW is an ISO string when the env sets it.
function trialNowIso(): string {
  const raw = process.env.MAKEFUN_TRIAL_NOW ?? '';
  if (raw !== '') {
    const ms = Date.parse(raw);
    if (Number.isFinite(ms)) return new Date(ms).toISOString();
  }
  return FIXED_ISO;
}
export const NOW_ISO: string = trialNowIso();

export function trialEpochSec(): number {
  const ms = Date.parse(NOW_ISO);
  return Math.floor(ms / 1000);
}

// ---------------------------------------------------------------- git objects

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjectType;
  data: Buffer;
}

const TYPE_NUM: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const NUM_TYPE: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** The git object id: SHA-1 over "<type> <size>\0" + bytes. */
export function sha1Hex(type: ObjectType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(head).update(data).digest('hex');
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

/** Git's tree sort: names compared byte-wise, directories as if ending in '/'. */
export function sortTreeEntries(entries: TreeEntry[]): TreeEntry[] {
  return [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
}

/** Serialize tree entries to raw tree bytes. */
export function buildTree(entries: TreeEntry[]): Buffer {
  const parts: Buffer[] = [];
  for (const e of sortTreeEntries(entries)) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Parse raw tree bytes back into entries. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const nul = data.indexOf(0x00, sp);
    if (sp === -1 || nul === -1 || nul + 21 > data.length) break;
    entries.push({
      mode: data.subarray(i, sp).toString('utf8'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    i = nul + 21;
  }
  return entries;
}

export interface ParsedCommit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

/** Parse a commit object's headers and message. */
export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split === -1 ? text : text.slice(0, split);
  const message = split === -1 ? '' : text.slice(split + 2);
  const parents: string[] = [];
  let tree = '';
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

/** Build a commit object. Identity defaults to the conformance identity, fixed time. */
export function buildCommit(opts: {
  tree: string;
  parents: string[];
  message: string;
  author?: string;
  committer?: string;
  timestamp?: number;
}): Buffer {
  const ts = opts.timestamp ?? FIXED_EPOCH_SEC;
  const author = `${opts.author ?? COMMIT_IDENT} ${ts} +0000`;
  const committer = `${opts.committer ?? COMMIT_IDENT} ${ts} +0000`;
  let s = `tree ${opts.tree}\n`;
  for (const p of opts.parents) s += `parent ${p}\n`;
  s += `author ${author}\ncommitter ${committer}\n\n${opts.message}`;
  return Buffer.from(s, 'utf8');
}

/**
 * Build a tree for a flat path -> blob-oid map, storing every intermediate tree in
 * `objects`. Returns the root tree oid.
 */
export function treeOidFromFiles(objects: Map<string, GitObject>, files: Map<string, string>): string {
  const buildDir = (prefix: string): string => {
    const entries: TreeEntry[] = [];
    const subdirs = new Set<string>();
    for (const [path, oid] of files) {
      if (!path.startsWith(prefix)) continue;
      const rest = path.slice(prefix.length);
      if (rest === '') continue;
      const slash = rest.indexOf('/');
      if (slash === -1) entries.push({ mode: '100644', name: rest, oid });
      else subdirs.add(rest.slice(0, slash));
    }
    for (const dir of subdirs) {
      entries.push({ mode: '40000', name: dir, oid: buildDir(`${prefix}${dir}/`) });
    }
    const buf = buildTree(entries);
    const oid = sha1Hex('tree', buf);
    objects.set(oid, { type: 'tree', data: buf });
    return oid;
  };
  return buildDir('');
}

/** Flatten a tree into path -> blob-oid, recursing into subdirectories. */
export function flattenTree(
  objects: Map<string, GitObject>,
  treeOid: string,
  prefix = '',
  out: Map<string, string> = new Map(),
): Map<string, string> {
  const obj = objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    if (e.mode === '40000') flattenTree(objects, e.oid, `${prefix}${e.name}/`, out);
    else out.set(`${prefix}${e.name}`, e.oid);
  }
  return out;
}

/** True when `ancestor` is reachable from `tip` by following parent links. */
export function isAncestor(objects: Map<string, GitObject>, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const stack: string[] = [tip];
  while (stack.length > 0) {
    const cur = stack.pop() as string;
    if (cur === ancestor) return true;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const obj = objects.get(cur);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return false;
}

/**
 * Every object reachable from `tips`, not crossing into `exclude` (objects the other
 * side already holds). Returns the objects themselves, keyed by oid.
 */
export function collectObjects(
  objects: Map<string, GitObject>,
  tips: string[],
  exclude: Set<string> = new Set(),
): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  const seen = new Set<string>(exclude);
  const stack: string[] = [...tips];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = objects.get(oid);
    if (!obj) continue;
    out.set(oid, obj);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      stack.push(c.tree);
      for (const p of c.parents) stack.push(p);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) stack.push(e.oid);
    } else if (obj.type === 'tag') {
      const m = /^object ([0-9a-f]{40})/m.exec(obj.data.toString('utf8'));
      if (m !== null) stack.push(m[1]);
    }
  }
  return out;
}

/** All commit oids reachable from `tip`, newest-first by walk order (parents after child). */
export function walkCommits(objects: Map<string, GitObject>, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const stack: string[] = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    out.push(oid);
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return out;
}

// ---------------------------------------------------------------- pack files

function packEntryHeader(typeNum: number, size: number): Buffer {
  const bytes: number[] = [];
  let b = (typeNum << 4) | (size & 0x0f);
  size >>= 4;
  while (size > 0) {
    bytes.push(b | 0x80);
    b = size & 0x7f;
    size >>= 7;
  }
  bytes.push(b);
  return Buffer.from(bytes);
}

/** Serialize objects into a pack v2 (plain objects only, no deltas). */
export function writePack(objs: Map<string, GitObject> | GitObject[]): Buffer {
  const list: GitObject[] = Array.isArray(objs) ? objs : [...objs.values()];
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(list.length, 8);
  parts.push(head);
  for (const obj of list) {
    parts.push(packEntryHeader(TYPE_NUM[obj.type], obj.data.length));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

export interface PackObject extends GitObject {
  oid: string;
}

/**
 * Parse a pack v2. The compressed length of each entry is found by binary search:
 * inflateSync throws on a truncated stream and silently ignores trailing bytes, so
 * the shortest prefix that inflates is the entry's true end. Throws on a bad magic,
 * a bad version, a bad trailer checksum, or a delta entry (this forge never sends
 * deltas and the suite's client never writes them).
 */
export function readPack(buf: Buffer): PackObject[] {
  if (buf.length < 32) throw new Error('pack too short');
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a pack file');
  if (buf.readUInt32BE(4) !== 2) throw new Error('unsupported pack version');
  const count = buf.readUInt32BE(8);
  const trailer = buf.subarray(buf.length - 20);
  const expect = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest();
  if (!expect.equals(trailer)) throw new Error('pack checksum mismatch');
  const out: PackObject[] = [];
  let off = 12;
  const end = buf.length - 20;
  for (let n = 0; n < count; n += 1) {
    if (off >= end) throw new Error('pack truncated');
    let byte = buf[off];
    off += 1;
    const typeNum = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      if (off >= end) throw new Error('pack truncated');
      byte = buf[off];
      off += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    const type = NUM_TYPE[typeNum];
    if (type === undefined) throw new Error(`unsupported pack object type ${typeNum}`);
    if (typeNum === 6 || typeNum === 7) throw new Error('delta objects are not accepted here');
    // Smallest prefix that inflates is the entry's compressed length.
    let lo = 1;
    let hi = end - off;
    let found = -1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      try {
        const data = inflateSync(buf.subarray(off, off + mid));
        if (data.length === size) {
          found = mid;
          hi = mid - 1;
        } else {
          // Inflated fine but the size is wrong: this is not a whole entry.
          lo = mid + 1;
        }
      } catch {
        lo = mid + 1;
      }
    }
    if (found === -1) throw new Error('pack entry does not inflate');
    const data = inflateSync(buf.subarray(off, off + found));
    off += found;
    out.push({ type, data, oid: sha1Hex(type, data) });
  }
  return out;
}

// ---------------------------------------------------------------- the store

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdSeq: number;
  jsonCache: string | null;
}

export interface Comment {
  id: number;
  body: string;
  createdSeq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  files: PullFile[];
  createdSeq: number;
  jsonCache: string | null;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Label {
  name: string;
  color: string;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  isPrivate: boolean;
  description: string;
  defaultBranch: string;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  issues: Map<number, Issue>;
  issueSeq: number;
  pulls: Map<number, Pull>;
  pullSeq: number;
  labels: Map<string, Label>;
  comments: Map<number, Comment>;
  commentSeq: number;
  createdSeq: number;
  jsonCache: string | null;
  advV0Upload: Buffer | null;
  advV0Receive: Buffer | null;
  advV2: Buffer | null;
}

/** A line diff: added/deleted line counts between two texts. */
export function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  const a = oldText === '' ? [] : oldText.split('\n');
  const b = newText === '' ? [] : newText.split('\n');
  // LCS lengths, one row at a time (files here are small).
  let prev = new Array<number>(b.length + 1).fill(0);
  for (let i = 1; i <= a.length; i += 1) {
    const cur = new Array<number>(b.length + 1).fill(0);
    for (let j = 1; j <= b.length; j += 1) {
      cur[j] = a[i - 1] === b[j - 1] ? prev[j - 1] + 1 : Math.max(prev[j], cur[j - 1]);
    }
    prev = cur;
  }
  const common = prev[b.length];
  return { additions: b.length - common, deletions: a.length - common };
}

/** The file-level diff between two trees: one PullFile per changed path. */
export function diffTrees(
  objects: Map<string, GitObject>,
  baseTree: string,
  headTree: string,
): PullFile[] {
  const base = baseTree === '' ? new Map<string, string>() : flattenTree(objects, baseTree);
  const head = headTree === '' ? new Map<string, string>() : flattenTree(objects, headTree);
  const files: PullFile[] = [];
  const names = new Set<string>([...base.keys(), ...head.keys()]);
  for (const name of [...names].sort()) {
    const b = base.get(name);
    const h = head.get(name);
    if (b === h) continue;
    if (b === undefined && h !== undefined) {
      const blob = objects.get(h);
      const text = blob !== undefined ? blob.data.toString('utf8') : '';
      const lines = text === '' ? 0 : text.split('\n').length;
      files.push({ filename: name, status: 'added', additions: lines, deletions: 0, changes: lines });
    } else if (h === undefined && b !== undefined) {
      const blob = objects.get(b);
      const text = blob !== undefined ? blob.data.toString('utf8') : '';
      const lines = text === '' ? 0 : text.split('\n').length;
      files.push({ filename: name, status: 'removed', additions: 0, deletions: lines, changes: lines });
    } else if (b !== undefined && h !== undefined) {
      const oldBlob = objects.get(b);
      const newBlob = objects.get(h);
      const d = lineDiff(
        oldBlob !== undefined ? oldBlob.data.toString('utf8') : '',
        newBlob !== undefined ? newBlob.data.toString('utf8') : '',
      );
      files.push({ filename: name, status: 'modified', additions: d.additions, deletions: d.deletions, changes: d.additions + d.deletions });
    }
  }
  return files;
}

export class Store {
  repos = new Map<string, Repo>();
  private repoSeq = 0;
  private globalSeq = 0;

  nextSeq(): number {
    this.globalSeq += 1;
    return this.globalSeq;
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo {
    this.repoSeq += 1;
    const repo: Repo = {
      id: this.repoSeq,
      name,
      owner: VIEWER_LOGIN,
      isPrivate,
      description,
      defaultBranch: DEFAULT_BRANCH,
      refs: new Map(),
      objects: new Map(),
      issues: new Map(),
      issueSeq: 0,
      pulls: new Map(),
      pullSeq: 0,
      labels: new Map(),
      comments: new Map(),
      commentSeq: 0,
      createdSeq: this.nextSeq(),
      jsonCache: null,
      advV0Upload: null,
      advV0Receive: null,
      advV2: null,
    };
    this.repos.set(name, repo);
    return repo;
  }

  getRepo(name: string): Repo | undefined {
    return this.repos.get(name);
  }

  deleteRepo(name: string): boolean {
    return this.repos.delete(name);
  }

  /** Any ref change invalidates the cached advertisements for that repo. */
  invalidateAdverts(repo: Repo): void {
    repo.advV0Upload = null;
    repo.advV0Receive = null;
    repo.advV2 = null;
  }

  /** The commit a ref points at, or null when the ref does not exist. */
  refTip(repo: Repo, ref: string): string | null {
    return repo.refs.get(ref) ?? null;
  }

  /** Resolve a branch name, a full ref, or a raw sha to a commit oid, or null. */
  resolveRef(repo: Repo, ref: string): string | null {
    if (ref === 'HEAD') ref = `refs/heads/${repo.defaultBranch}`;
    if (/^[0-9a-f]{40}$/.test(ref)) return repo.objects.has(ref) ? ref : null;
    if (ref.startsWith('refs/')) return repo.refs.get(ref) ?? null;
    return repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`) ?? null;
  }

  /** The flat file map of a commit's tree (empty when the commit has no tree). */
  filesAt(repo: Repo, commitOid: string): Map<string, string> {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return new Map();
    return flattenTree(repo.objects, parseCommit(obj.data).tree);
  }

  /** True when the PR is conflicted: a path it changes differs between its recorded
   *  base and the current tip of its base branch. */
  pullConflicted(repo: Repo, pull: Pull): boolean {
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (baseTip === null || baseTip === undefined) return false;
    if (baseTip === pull.baseSha) return false;
    const baseFiles = this.filesAt(repo, pull.baseSha);
    const tipFiles = this.filesAt(repo, baseTip);
    for (const file of pull.files) {
      if (baseFiles.get(file.filename) !== tipFiles.get(file.filename)) return true;
    }
    return false;
  }
}
