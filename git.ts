// git.ts — git smart HTTP: upload-pack v2 (+ v0), receive-pack v0, pkt-lines,
// sideband framing, pack negotiation. One object store, shared with REST.
//
// Wire contract, as the suite's client speaks it:
//   GET  /{owner}/{repo}.git/info/refs?service=git-upload-pack   v2 when the
//        Git-Protocol: version=2 header is sent, else v0
//   GET  /{owner}/{repo}.git/info/refs?service=git-receive-pack  v0 only
//   POST /{owner}/{repo}.git/git-upload-pack   v2 commands (ls-refs, fetch) or v0 fetch
//   POST /{owner}/{repo}.git/git-receive-pack  v0 push
// Missing repo is 404 on info/refs and the server stays up. A malformed POST body
// is a 400, never a crash.

import {
  FETCH_CACHE_MAX,
  ZERO_OID,
  VIEWER_LOGIN,
  collectObjects,
  flattenTree,
  isAncestor,
  parseCommit,
  parseTree,
  readPack,
  sha1Hex,
  writePack,
  type GitObject,
  type Repo,
  type Store,
} from './store.ts';

// ---------------------------------------------------------------- pkt-lines

export interface Pkt {
  kind: 'flush' | 'delim' | 'end' | 'data';
  data: Buffer;
}

const FLUSH = Buffer.from('0000', 'ascii');
const DELIM = Buffer.from('0001', 'ascii');

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = data.length + 4;
  // One allocation: the length prefix and the payload share a single buffer.
  const out = Buffer.allocUnsafe(len);
  out.write(len.toString(16).padStart(4, '0'), 0, 4, 'ascii');
  data.copy(out, 4);
  return out;
}

function hexVal(b: number): number {
  if (b >= 48 && b <= 57) return b - 48; // 0-9
  if (b >= 97 && b <= 102) return b - 87; // a-f
  if (b >= 65 && b <= 70) return b - 55; // A-F
  return -1;
}

/** Read one pkt-line at `off`; null when the bytes run out first. */
export function readPktLine(buf: Buffer, off: number): { pkt: Pkt; next: number } | null {
  if (off + 4 > buf.length) return null;
  const h0 = hexVal(buf[off]);
  const h1 = hexVal(buf[off + 1]);
  const h2 = hexVal(buf[off + 2]);
  const h3 = hexVal(buf[off + 3]);
  if (h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0) throw new Error('bad pkt-line length');
  const len = (h0 << 12) | (h1 << 8) | (h2 << 4) | h3;
  if (len === 0) return { pkt: { kind: 'flush', data: Buffer.alloc(0) }, next: off + 4 };
  if (len === 1) return { pkt: { kind: 'delim', data: Buffer.alloc(0) }, next: off + 4 };
  if (len === 2) return { pkt: { kind: 'end', data: Buffer.alloc(0) }, next: off + 4 };
  if (len < 4 || off + len > buf.length) throw new Error('pkt-line truncated');
  return { pkt: { kind: 'data', data: buf.subarray(off + 4, off + len) }, next: off + len };
}

/** All data-packet payloads as strings, stopping at the first flush. The one trailing
 *  newline is stripped here, once, instead of a regex per line in every caller. */
export function pktLinesUntilFlush(buf: Buffer): { lines: string[]; next: number } {
  const lines: string[] = [];
  let off = 0;
  for (;;) {
    const read = readPktLine(buf, off);
    if (read === null) throw new Error('pkt-line stream ended before flush');
    if (read.pkt.kind === 'flush') return { lines, next: read.next };
    if (read.pkt.kind === 'data') {
      let line = read.pkt.data.toString('utf8');
      if (line.endsWith('\n')) line = line.slice(0, -1);
      lines.push(line);
    }
    off = read.next;
  }
}

/** Split pack bytes into sideband band-1 pkt-lines (each payload at most 65516). */
export function sidebandPackets(data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  for (let off = 0; off < data.length; off += 65515) {
    const chunk = data.subarray(off, Math.min(data.length, off + 65515));
    const framed = Buffer.allocUnsafe(chunk.length + 1);
    framed[0] = 1;
    chunk.copy(framed, 1);
    out.push(pktLine(framed));
  }
  return out;
}

// ---------------------------------------------------------------- ref lists

interface RefLine {
  oid: string;
  name: string;
  peeled?: string;
  symrefTarget?: string;
  unborn?: boolean;
}

function sortedRefs(repo: Repo): RefLine[] {
  const out: RefLine[] = [];
  const names = [...repo.refs.keys()].sort();
  for (const name of names) {
    const oid = repo.refs.get(name) as string;
    const line: RefLine = { oid, name };
    const obj = repo.objects.get(oid);
    if (obj !== undefined && obj.type === 'tag') {
      const m = /^object ([0-9a-f]{40})/m.exec(obj.data.toString('utf8'));
      if (m !== null) line.peeled = m[1];
    }
    out.push(line);
  }
  return out;
}

function defaultTip(repo: Repo): string | null {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
}

/** The HEAD line for ls-refs / v0: a real ref when the repo has one, unborn otherwise. */
function headLine(repo: Repo): RefLine {
  const tip = defaultTip(repo);
  if (tip === null) return { oid: ZERO_OID, name: 'HEAD', symrefTarget: `refs/heads/${repo.defaultBranch}`, unborn: true };
  return { oid: tip, name: 'HEAD', symrefTarget: `refs/heads/${repo.defaultBranch}` };
}

const UPLOAD_CAPS =
  'side-band-64k multi_ack_detailed thin-pack ofs-delta shallow symref=HEAD:refs/heads/main agent=gh-forge/1.0 object-format=sha1';
const RECEIVE_CAPS =
  'report-status side-band-64k delete-refs ofs-delta thin-pack symref=HEAD:refs/heads/main agent=gh-forge/1.0 object-format=sha1';

/** v0 advertisement: service line, flush, then ref lines, caps on the first line. */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_CAPS : RECEIVE_CAPS;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), FLUSH];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    const tip = defaultTip(repo);
    const first: string[] = [];
    if (tip !== null) first.push(`${tip} HEAD\0${caps}\n`);
    else first.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
    for (const line of first) parts.push(pktLine(line));
    for (const ref of refs) {
      parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.peeled !== undefined) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/** v2 advertisement: capability lines only, first line exactly `version 2`. */
function v2Advertisement(): Buffer {
  return Buffer.concat([
    pktLine('version 2\n'),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow wait-for-done\n'),
    pktLine('agent=gh-forge/1.0\n'),
    pktLine('object-format=sha1\n'),
    FLUSH,
  ]);
}

const V2_ADV = v2Advertisement();

// ---------------------------------------------------------------- fetch

interface FetchRequest {
  wants: string[];
  haves: string[];
  shallows: string[];
  depth?: number;
  deepenSince?: number;
  deepenNot: string[];
}

function committerTs(commitData: Buffer): number {
  const m = /committer .*? (\d+) [+-]\d{4}\n/.exec(commitData.toString('utf8'));
  return m !== null ? Number(m[1]) : 0;
}

interface WalkResult {
  walked: Set<string>;
  sendCommits: Set<string>;
}

/**
 * Walk from the wants, stopping at haves and at the depth / deepen-since limits.
 * Tag wants resolve to their target commit. `shallows` are walked through (their
 * parents are wanted) but never sent themselves.
 */
function walkWants(repo: Repo, req: FetchRequest): WalkResult {
  // A commit in both haves and shallows is a shallow boundary the client holds
  // as an object but whose parents it lacks: traversal goes THROUGH it (within
  // the depth limit). A plain have stops traversal — its history is held in full.
  const shallowSet = new Set<string>(req.shallows);
  const haveSet = new Set<string>(req.haves);
  for (const ref of req.deepenNot) {
    const tip = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`);
    if (tip !== undefined) haveSet.add(tip);
  }
  const walkStop = new Set<string>([...haveSet].filter((oid) => !shallowSet.has(oid)));
  const walked = new Set<string>();
  const stack: { oid: string; depth: number }[] = [];
  for (const want of req.wants) {
    const obj = repo.objects.get(want);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      const m = /^object ([0-9a-f]{40})/m.exec(obj.data.toString('utf8'));
      if (m !== null) stack.push({ oid: m[1], depth: 1 });
      continue;
    }
    stack.push({ oid: want, depth: 1 });
  }
  while (stack.length > 0) {
    const { oid, depth } = stack.pop() as { oid: string; depth: number };
    if (walked.has(oid) || walkStop.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    if (req.deepenSince !== undefined && committerTs(obj.data) < req.deepenSince) continue;
    walked.add(oid);
    if (req.depth !== undefined && depth >= req.depth) continue;
    for (const p of parseCommit(obj.data).parents) stack.push({ oid: p, depth: depth + 1 });
  }
  const sendCommits = new Set<string>();
  for (const oid of walked) {
    if (!haveSet.has(oid) && !shallowSet.has(oid)) sendCommits.add(oid);
  }
  return { walked, sendCommits };
}

/**
 * The objects the client already holds. A plain have is held in full: the commit,
 * its ancestors, and every tree and blob under them. A shallow boundary is held
 * only as itself plus its tree subtree — its parents are exactly what a deepen
 * is asking for, so they must never land in this set.
 */
function heldObjects(repo: Repo, haves: string[], shallows: string[]): Set<string> {
  const held = new Set<string>();
  const shallowSet = new Set<string>(shallows);
  const fullTips = haves.filter((oid) => !shallowSet.has(oid));
  for (const oid of collectObjects(repo.objects, fullTips).keys()) held.add(oid);
  for (const oid of shallows) {
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    held.add(oid);
    if (obj.type !== 'commit') continue;
    const tree = parseCommit(obj.data).tree;
    const addTree = (treeOid: string): void => {
      if (held.has(treeOid)) return;
      const t = repo.objects.get(treeOid);
      if (t === undefined || t.type !== 'tree') return;
      held.add(treeOid);
      for (const e of parseTree(t.data)) if (e.mode === '40000') addTree(e.oid);
    };
    addTree(tree);
    for (const blobOid of flattenTree(repo.objects, tree).values()) held.add(blobOid);
  }
  return held;
}

function collectTreeInto(repo: Repo, treeOid: string, held: Set<string>, out: Map<string, GitObject>): void {
  if (held.has(treeOid) || out.has(treeOid)) return;
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return;
  out.set(treeOid, obj);
  for (const e of parseTree(obj.data)) {
    if (e.mode === '40000') collectTreeInto(repo, e.oid, held, out);
    else if (!held.has(e.oid) && !out.has(e.oid)) {
      const blob = repo.objects.get(e.oid);
      if (blob !== undefined) out.set(e.oid, blob);
    }
  }
}

/** The objects a fetch sends: the new commits, their trees and blobs, minus held ones. */
function fetchObjects(repo: Repo, req: FetchRequest, sendCommits: Set<string>): Map<string, GitObject> {
  const held = heldObjects(repo, req.haves, req.shallows);
  const out = new Map<string, GitObject>();
  for (const want of req.wants) {
    const obj = repo.objects.get(want);
    if (obj !== undefined && obj.type === 'tag' && !held.has(want)) out.set(want, obj);
  }
  for (const oid of sendCommits) {
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    if (!held.has(oid)) out.set(oid, obj);
    collectTreeInto(repo, parseCommit(obj.data).tree, held, out);
  }
  return out;
}

/**
 * Shallow-info lines for a fetch: `shallow K` for every boundary commit K (a commit
 * whose parent is not available to the client after this fetch), `unshallow K` for
 * every client shallow boundary that is no longer a boundary.
 */
function shallowInfo(repo: Repo, req: FetchRequest, walked: Set<string>, sendCommits: Set<string>): string[] {
  const haveSet = new Set<string>(req.haves);
  for (const ref of req.deepenNot) {
    const tip = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`);
    if (tip !== undefined) haveSet.add(tip);
  }
  const shallowSet = new Set<string>(req.shallows);
  const available = new Set<string>([...sendCommits, ...haveSet, ...shallowSet]);
  const lines: string[] = [];
  const candidates = new Set<string>([...sendCommits]);
  for (const oid of shallowSet) if (walked.has(oid)) candidates.add(oid);
  for (const oid of candidates) {
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    const parents = parseCommit(obj.data).parents;
    if (parents.length === 0) continue;
    const boundary = parents.some((p) => !available.has(p));
    if (!boundary) {
      if (shallowSet.has(oid)) lines.push(`unshallow ${oid}\n`);
      continue;
    }
    lines.push(`shallow ${oid}\n`);
  }
  return lines;
}

function wantedRefs(repo: Repo, wants: string[]): { oid: string; ref: string }[] {
  const out: { oid: string; ref: string }[] = [];
  for (const want of wants) {
    for (const [name, oid] of repo.refs) {
      if (oid === want) out.push({ oid: want, ref: name });
    }
  }
  return out;
}

/** The v2 fetch response: acknowledgments, shallow-info, wanted-refs, packfile.
 *  The whole response is cached per (repo, request fingerprint): a fetch's bytes
 *  depend only on the request and the repo's objects/refs, and invalidateAdverts
 *  drops the cache on any mutation, so a cached response is always current. */
function fetchCacheKey(version: 'v0' | 'v2', req: FetchRequest): string {
  const sorted = (xs: string[]): string => [...xs].sort().join(',');
  return [
    version,
    sorted(req.wants),
    sorted(req.haves),
    sorted(req.shallows),
    req.depth ?? '',
    req.deepenSince ?? '',
    sorted(req.deepenNot),
  ].join('|');
}

/** Store a fetch response, evicting the oldest entry past the bound. */
function fetchCachePut(repo: Repo, key: string, body: Buffer): void {
  if (repo.fetchCache.size >= FETCH_CACHE_MAX) {
    const oldest = repo.fetchCache.keys().next().value;
    if (oldest !== undefined) repo.fetchCache.delete(oldest);
  }
  repo.fetchCache.set(key, body);
}

function v2FetchResponse(repo: Repo, req: FetchRequest): Buffer {
  const key = fetchCacheKey('v2', req);
  const cached = repo.fetchCache.get(key);
  if (cached !== undefined) return cached;
  const built = v2FetchResponseUncached(repo, req);
  fetchCachePut(repo, key, built);
  return built;
}

function v2FetchResponseUncached(repo: Repo, req: FetchRequest): Buffer {
  const { walked, sendCommits } = walkWants(repo, req);
  const objects = fetchObjects(repo, req, sendCommits);
  const parts: Buffer[] = [];
  const sections: Buffer[][] = [];

  const ackLines: string[] = [];
  if (req.haves.length === 0) {
    ackLines.push('NAK\n');
  } else {
    for (const want of req.wants) {
      if (repo.objects.has(want)) ackLines.push(`ACK ${want}\n`);
    }
    ackLines.push('ready\n');
  }
  sections.push([pktLine('acknowledgments\n'), ...ackLines.map((l) => pktLine(l))]);

  const shallowLines = shallowInfo(repo, req, walked, sendCommits);
  if (shallowLines.length > 0) {
    sections.push([pktLine('shallow-info\n'), ...shallowLines.map((l) => pktLine(l))]);
  }

  const refs = wantedRefs(repo, req.wants);
  if (refs.length > 0) {
    sections.push([pktLine('wanted-refs\n'), ...refs.map((r) => pktLine(`${r.oid} ${r.ref}\n`))]);
  }

  if (objects.size > 0) {
    const pack = writePack(objects);
    sections.push([pktLine('packfile\n'), ...sidebandPackets(pack)]);
  }

  sections.forEach((section, i) => {
    if (i > 0) parts.push(DELIM);
    parts.push(...section);
  });
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/** The v0 fetch response: bare NAK/ACK/shallow lines, then a sideband pack. */
function v0FetchResponse(repo: Repo, req: FetchRequest): Buffer {
  const key = fetchCacheKey('v0', req);
  const cached = repo.fetchCache.get(key);
  if (cached !== undefined) return cached;
  const built = v0FetchResponseUncached(repo, req);
  fetchCachePut(repo, key, built);
  return built;
}

function v0FetchResponseUncached(repo: Repo, req: FetchRequest): Buffer {
  const { walked, sendCommits } = walkWants(repo, req);
  const objects = fetchObjects(repo, req, sendCommits);
  const parts: Buffer[] = [];
  if (req.haves.length === 0) {
    parts.push(pktLine('NAK\n'));
  } else {
    for (const want of req.wants) {
      if (repo.objects.has(want)) parts.push(pktLine(`ACK ${want}\n`));
    }
  }
  for (const line of shallowInfo(repo, req, walked, sendCommits)) parts.push(pktLine(line));
  if (objects.size > 0) {
    parts.push(...sidebandPackets(writePack(objects)));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function parseV2FetchBody(body: Buffer): FetchRequest {
  const { lines } = pktLinesUntilFlush(body);
  const req: FetchRequest = { wants: [], haves: [], shallows: [], deepenNot: [] };
  let seenDelim = false;
  for (const raw of lines) {
    const line = raw;
    if (line === '') continue;
    if (line === 'command=fetch') continue;
    if (line.startsWith('agent=') || line.startsWith('object-format=')) continue;
    if (line === 'thin-pack' || line === 'no-progress' || line === 'ofs-delta' || line === 'done') continue;
    if (line.startsWith('want ')) req.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) req.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) req.shallows.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) req.depth = parseInt(line.slice(7), 10);
    else if (line.startsWith('deepen-since ')) req.deepenSince = parseInt(line.slice(13), 10);
    else if (line.startsWith('deepen-not ')) req.deepenNot.push(line.slice(11).trim());
    else if (line.startsWith('filter ')) {
      // No filter is advertised, so a filter line here is ignored.
      continue;
    }
    void seenDelim;
  }
  return req;
}

function parseV0FetchBody(body: Buffer): FetchRequest {
  const req: FetchRequest = { wants: [], haves: [], shallows: [], deepenNot: [] };
  // Section 1: wants, shallow, deepen. Section 2: haves, done. Two flushes.
  const first = pktLinesUntilFlush(body);
  for (const raw of first.lines) {
    const line = raw;
    if (line.startsWith('want ')) {
      const rest = line.slice(5).trim();
      req.wants.push(rest.split(' ')[0]);
    } else if (line.startsWith('shallow ')) req.shallows.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) req.depth = parseInt(line.slice(7), 10);
    else if (line.startsWith('deepen-since ')) req.deepenSince = parseInt(line.slice(13), 10);
    else if (line.startsWith('deepen-not ')) req.deepenNot.push(line.slice(11).trim());
  }
  const rest = body.subarray(first.next);
  if (rest.length > 0) {
    const second = pktLinesUntilFlush(rest);
    for (const raw of second.lines) {
      const line = raw;
      if (line.startsWith('have ')) req.haves.push(line.slice(5).trim());
    }
  }
  return req;
}

// ---------------------------------------------------------------- ls-refs

function lsRefsResponse(repo: Repo, prefixes: string[]): Buffer {
  const key = prefixes.join('\n');
  const cached = repo.lsRefsCache.get(key);
  if (cached !== undefined) return cached;
  const built = lsRefsResponseUncached(repo, prefixes);
  repo.lsRefsCache.set(key, built);
  return built;
}

function lsRefsResponseUncached(repo: Repo, prefixes: string[]): Buffer {
  const parts: Buffer[] = [];
  const head = headLine(repo);
  const refs = sortedRefs(repo);
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  if (keep(head.name)) {
    if (head.unborn === true) parts.push(pktLine(`unborn ${head.name} symref-target:${head.symrefTarget}\n`));
    else parts.push(pktLine(`${head.oid} ${head.name} symref-target:${head.symrefTarget}\n`));
  }
  for (const ref of refs) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (ref.peeled !== undefined) line += ` peeled:${ref.peeled}`;
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function parseLsRefsBody(body: Buffer): string[] {
  const { lines } = pktLinesUntilFlush(body);
  const prefixes: string[] = [];
  for (const raw of lines) {
    const line = raw;
    if (line.startsWith('ref-prefix ')) prefixes.push(line.slice(11).trim());
  }
  return prefixes;
}

// ---------------------------------------------------------------- receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function parsePushBody(body: Buffer): { commands: PushCommand[]; pack: Buffer | null } {
  const { lines, next } = pktLinesUntilFlush(body);
  const commands: PushCommand[] = [];
  for (const raw of lines) {
    const line = raw;
    const nul = line.indexOf('\0');
    const head = nul >= 0 ? line.slice(0, nul) : line;
    const parts = head.split(' ');
    if (parts.length !== 3) throw new Error('bad push command line');
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (!/^[0-9a-f]{40}$/.test(oldOid) || !/^[0-9a-f]{40}$/.test(newOid)) throw new Error('bad push object id');
    if (!ref.startsWith('refs/') || ref.length > 200 || /[\s\x00-\x1f]/.test(ref)) throw new Error('bad push ref');
    commands.push({ oldOid, newOid, ref });
  }
  if (commands.length === 0) throw new Error('push has no commands');
  const pack = next < body.length ? body.subarray(next) : null;
  return { commands, pack };
}

function pushReport(repo: Repo, store: Store, commands: PushCommand[], pack: Buffer | null): Buffer {
  const lines: string[] = [];
  let unpackError: string | null = null;
  if (pack !== null && pack.length > 0) {
    try {
      const objects = readPack(pack);
      for (const obj of objects) repo.objects.set(obj.oid, { type: obj.type, data: obj.data });
    } catch (e) {
      unpackError = e instanceof Error ? e.message : 'unpack failed';
    }
  }
  if (unpackError !== null) {
    lines.push(`unpack ${unpackError}\n`);
    for (const c of commands) lines.push(`ng ${c.ref} unpack failed\n`);
  } else {
    lines.push('unpack ok\n');
    for (const c of commands) {
      const current = repo.refs.get(c.ref);
      if (c.newOid === ZERO_OID) {
        if (current === undefined) {
          lines.push(`ng ${c.ref} remote ref does not exist\n`);
        } else {
          repo.refs.delete(c.ref);
          lines.push(`ok ${c.ref}\n`);
        }
        continue;
      }
      if (c.oldOid === ZERO_OID) {
        if (current !== undefined) {
          lines.push(`ng ${c.ref} already exists\n`);
        } else if (!repo.objects.has(c.newOid)) {
          lines.push(`ng ${c.ref} object not found\n`);
        } else {
          repo.refs.set(c.ref, c.newOid);
          lines.push(`ok ${c.ref}\n`);
        }
        continue;
      }
      if (current === undefined) {
        lines.push(`ng ${c.ref} remote ref does not exist\n`);
      } else if (c.oldOid !== current) {
        lines.push(`ng ${c.ref} non-fast-forward\n`);
      } else if (!repo.objects.has(c.newOid)) {
        lines.push(`ng ${c.ref} object not found\n`);
      } else if (!isAncestor(repo.objects, c.oldOid, c.newOid)) {
        lines.push(`ng ${c.ref} non-fast-forward\n`);
      } else {
        repo.refs.set(c.ref, c.newOid);
        lines.push(`ok ${c.ref}\n`);
      }
    }
    void store;
  }
  store.invalidateAdverts(repo);
  const parts: Buffer[] = [];
  for (const line of lines) parts.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.from(line, 'utf8')])));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- routing

export interface GitResult {
  status: number;
  contentType: string;
  body: Buffer;
}

function gitError(status: number, message: string): GitResult {
  return { status, contentType: 'application/json; charset=utf-8', body: Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8') };
}

/**
 * Handle one git smart-HTTP request. `repo` is null when the repo does not exist.
 * Returns null only when the path is not a git route at all.
 */
export function handleGit(
  store: Store,
  owner: string,
  repoName: string,
  method: string,
  rest: string[],
  query: URLSearchParams,
  headers: Record<string, string | string[] | undefined>,
  body: Buffer,
): GitResult | null {
  if (rest.length === 0) return null;
  const repo = store.getRepo(repoName);
  const isInfoRefs = rest[0] === 'info' && rest[1] === 'refs';
  const service = query.get('service') ?? '';
  const gitProtocol = (headers['git-protocol'] as string | undefined) ?? '';

  if (isInfoRefs && method === 'GET') {
    if (repo === undefined || owner !== VIEWER_LOGIN) return gitError(404, 'Not Found');
    if (service === 'git-upload-pack') {
      if (gitProtocol === 'version=2') {
        return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: V2_ADV };
      }
      if (repo.advV0Upload === null) repo.advV0Upload = v0Advertisement(repo, 'git-upload-pack');
      return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: repo.advV0Upload };
    }
    if (service === 'git-receive-pack') {
      if (repo.advV0Receive === null) repo.advV0Receive = v0Advertisement(repo, 'git-receive-pack');
      return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: repo.advV0Receive };
    }
    return gitError(404, 'Not Found');
  }

  if (rest[0] === 'git-upload-pack' && method === 'POST') {
    if (repo === undefined || owner !== VIEWER_LOGIN) return gitError(404, 'Not Found');
    try {
      const first = readPktLine(body, 0);
      const firstText = first !== null && first.pkt.kind === 'data' ? first.pkt.data.toString('utf8') : '';
      if (firstText.startsWith('command=ls-refs')) {
        const prefixes = parseLsRefsBody(body);
        return { status: 200, contentType: 'application/x-git-upload-pack-result', body: lsRefsResponse(repo, prefixes) };
      }
      if (firstText.startsWith('command=fetch')) {
        const req = parseV2FetchBody(body);
        return { status: 200, contentType: 'application/x-git-upload-pack-result', body: v2FetchResponse(repo, req) };
      }
      // Anything else is a v0 fetch.
      const req = parseV0FetchBody(body);
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: v0FetchResponse(repo, req) };
    } catch {
      return gitError(400, 'Bad request');
    }
  }

  if (rest[0] === 'git-receive-pack' && method === 'POST') {
    if (repo === undefined || owner !== VIEWER_LOGIN) return gitError(404, 'Not Found');
    try {
      const { commands, pack } = parsePushBody(body);
      return { status: 200, contentType: 'application/x-git-receive-pack-result', body: pushReport(repo, store, commands, pack) };
    } catch {
      return gitError(400, 'Bad request');
    }
  }

  return null;
}
