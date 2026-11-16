// app.ts — the forge's HTTP server: routing plus the REST v3 surface. Git smart
// HTTP, GraphQL and MCP live in their own modules and are wired in here; until a
// module exists its routes answer the JSON 404 like any unknown route.
//
// Every reply is JSON (never HTML) and carries a non-empty X-GitHub-Request-Id.
// A request id is a per-process counter, so two runs that make the same requests
// in the same order produce the same headers.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGit } from './git.ts';
import {
  DEFAULT_BRANCH,
  FIXED_EPOCH_SEC,
  FIXED_ISO,
  NOW_ISO,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  ZERO_OID,
  buildCommit,
  buildTree,
  diffTrees,
  flattenTree,
  lineDiff,
  parseCommit,
  parseTree,
  sha1Hex,
  treeOidFromFiles,
  type GitObject,
  type Issue,
  type Label,
  type Pull,
  type Repo,
  Store,
} from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

let requestSeq = 0;

export interface Ctx {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  path: string;
  query: URLSearchParams;
  segments: string[];
  body: Buffer;
  json: Record<string, unknown> | null;
  jsonBad: boolean;
}

function sendJson(ctx: Ctx, status: number, value: unknown, extra: Record<string, string> = {}): void {
  const text = value === null ? '' : `${JSON.stringify(value)}\n`;
  const headers: Record<string, string> = {
    'content-type': JSON_TYPE,
    'x-github-request-id': `req-${ctxSeq(ctx)}`,
    ...extra,
  };
  if (text !== '') headers['content-length'] = String(Buffer.byteLength(text));
  ctx.res.writeHead(status, headers);
  ctx.res.end(text);
}

function ctxSeq(ctx: Ctx): number {
  const n = (ctx as unknown as { seq: number }).seq;
  return n;
}

function sendEmpty(ctx: Ctx, status: number): void {
  ctx.res.writeHead(status, { 'x-github-request-id': `req-${ctxSeq(ctx)}` });
  ctx.res.end();
}

function err(ctx: Ctx, status: number, message: string): void {
  sendJson(ctx, status, { message });
}

/** The bearer token this process expects, or '' when any non-empty bearer will do. */
function authorized(ctx: Ctx): boolean {
  const header = ctx.req.headers.authorization ?? '';
  const m = /^Bearer (.+)$/.exec(header);
  if (m === null) return false;
  if (SHOP_TOKEN !== '') return m[1] === SHOP_TOKEN;
  return m[1] !== '';
}

function needAuth(ctx: Ctx): boolean {
  if (authorized(ctx)) return true;
  err(ctx, 401, 'Requires authentication');
  return false;
}

function pageParams(ctx: Ctx): { page: number; perPage: number } {
  const page = Math.max(1, parseInt(ctx.query.get('page') ?? '1', 10) || 1);
  const perPage = Math.min(100, Math.max(1, parseInt(ctx.query.get('per_page') ?? '30', 10) || 30));
  return { page, perPage };
}

/** Slice a list into one page; `total` is the full list length. */
function paginate<T>(list: T[], page: number, perPage: number): T[] {
  return list.slice((page - 1) * perPage, page * perPage);
}

/** A Link header for the next page, only when more pages follow. Absolute, same origin. */
function linkHeader(ctx: Ctx, page: number, perPage: number, total: number): Record<string, string> {
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  if (page >= totalPages) return {};
  const host = ctx.req.headers.host ?? 'localhost';
  const base = `http://${host}${ctx.path}`;
  const next = `${base}?page=${page + 1}&per_page=${perPage}`;
  const last = `${base}?page=${totalPages}&per_page=${perPage}`;
  return { link: `<${next}>; rel="next", <${last}>; rel="last"` };
}

// ---------------------------------------------------------------- JSON views

function repoObject(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.isPrivate,
    default_branch: repo.defaultBranch,
    description: repo.description,
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    clone_url: `https://github.com/${repo.owner}/${repo.name}.git`,
    has_issues: true,
  };
}

function repoJson(repo: Repo): string {
  if (repo.jsonCache === null) repo.jsonCache = JSON.stringify(repoObject(repo));
  return repo.jsonCache;
}

function labelObject(label: Label): Record<string, unknown> {
  return { name: label.name, color: label.color, description: '' };
}

function issueObject(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return label !== undefined ? labelObject(label) : { name, color: 'ededed', description: '' };
    }),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function issueJson(repo: Repo, issue: Issue): string {
  if (issue.jsonCache === null) issue.jsonCache = JSON.stringify(issueObject(repo, issue));
  return issue.jsonCache;
}

function pullHeadSha(repo: Repo, pull: Pull): string {
  return repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha;
}

function pullBaseSha(repo: Repo, pull: Pull): string {
  return repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
}

function pullObject(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: { ref: pull.headRef, sha: pullHeadSha(repo, pull), label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pullBaseSha(repo, pull), label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: !repoPullConflicted(repo, pull),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function repoPullConflicted(repo: Repo, pull: Pull): boolean {
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (baseTip === undefined || baseTip === pull.baseSha) return false;
  const baseFiles = filesAtCommit(repo, pull.baseSha);
  const tipFiles = filesAtCommit(repo, baseTip);
  for (const file of pull.files) {
    if (baseFiles.get(file.filename) !== tipFiles.get(file.filename)) return true;
  }
  return false;
}

function filesAtCommit(repo: Repo, commitOid: string): Map<string, string> {
  const obj = repo.objects.get(commitOid);
  if (!obj || obj.type !== 'commit') return new Map();
  return flattenTree(repo.objects, parseCommit(obj.data).tree);
}

function identParts(ident: string): { name: string; email: string; date: string } {
  const m = /^(.*?) <(.*?)> (\d+) ([+-]\d{4})$/.exec(ident);
  if (m === null) return { name: ident, email: '', date: FIXED_ISO };
  return { name: m[1], email: m[2], date: new Date(Number(m[3]) * 1000).toISOString() };
}

// ---------------------------------------------------------------- helpers

function bodyString(ctx: Ctx, key: string): string {
  const v = ctx.json?.[key];
  return typeof v === 'string' ? v : '';
}

function bodyBool(ctx: Ctx, key: string, fallback: boolean): boolean {
  const v = ctx.json?.[key];
  return typeof v === 'boolean' ? v : fallback;
}

/** Resolve owner + repo from the path; 404 when the owner is not the viewer or the repo is missing. */
function openRepo(ctx: Ctx, store: Store, owner: string, name: string): Repo | null {
  if (owner !== VIEWER_LOGIN) {
    err(ctx, 404, 'Not Found');
    return null;
  }
  const repo = store.getRepo(name);
  if (repo === undefined) {
    err(ctx, 404, 'Not Found');
    return null;
  }
  return repo;
}

function decode(seg: string): string {
  try {
    return decodeURIComponent(seg);
  } catch {
    return seg;
  }
}

/** Create one commit on `branch` from a flat file map; returns the commit oid. */
function commitFiles(
  repo: Repo,
  branch: string,
  files: Map<string, string>,
  message: string,
): string {
  const treeOid = treeOidFromFiles(repo.objects, files);
  const parent = repo.refs.get(`refs/heads/${branch}`);
  const commit = buildCommit({
    tree: treeOid,
    parents: parent === undefined ? [] : [parent],
    message,
    timestamp: FIXED_EPOCH_SEC,
  });
  const oid = sha1Hex('commit', commit);
  repo.objects.set(oid, { type: 'commit', data: commit });
  repo.refs.set(`refs/heads/${branch}`, oid);
  return oid;
}

// ---------------------------------------------------------------- REST handlers

function handleRest(ctx: Ctx, store: Store): void {
  const s = ctx.segments; // after /api/v3
  const method = ctx.method;

  // GET /api/v3 — the API root (starter behavior: open, 200 + scopes).
  if (s.length === 0) {
    if (method === 'GET') {
      sendJson(ctx, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET /api/v3/meta
  if (s.length === 1 && s[0] === 'meta') {
    if (method !== 'GET' || !needAuth(ctx)) return;
    sendJson(ctx, 200, { installed_version: '1.0.0', verifiable_password_authentication: true });
    return;
  }

  // GET /api/v3/user
  if (s.length === 1 && s[0] === 'user') {
    if (method !== 'GET' || !needAuth(ctx)) return;
    sendJson(ctx, 200, { login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, type: 'User' });
    return;
  }

  // /api/v3/user/repos
  if (s.length === 2 && s[0] === 'user' && s[1] === 'repos') {
    if (!needAuth(ctx)) return;
    if (method === 'GET') {
      const all = [...store.repos.values()].sort((a, b) => a.createdSeq - b.createdSeq);
      const { page, perPage } = pageParams(ctx);
      sendJson(ctx, 200, paginate(all, page, perPage).map(repoObject), linkHeader(ctx, page, perPage, all.length));
      return;
    }
    if (method === 'POST') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const name = bodyString(ctx, 'name');
      if (name === '' || store.getRepo(name) !== undefined) {
        err(ctx, 422, name === '' ? 'Repository name is required' : `Repository '${name}' already exists`);
        return;
      }
      const repo = store.createRepo(name, bodyBool(ctx, 'private', false), bodyString(ctx, 'description'));
      sendJson(ctx, 201, repoObject(repo));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // /api/v3/repos/{owner}/{repo}[...]
  if (s.length >= 3 && s[0] === 'repos') {
    handleRepo(ctx, store, s);
    return;
  }

  err(ctx, 404, 'Not Found');
}

function handleRepo(ctx: Ctx, store: Store, s: string[]): void {
  const method = ctx.method;
  const owner = decode(s[1]);
  const name = decode(s[2]);
  const rest = s.slice(3);

  if (!needAuth(ctx)) return;
  const repo = openRepo(ctx, store, owner, name);
  if (repo === null) return;

  // GET/PATCH/DELETE /repos/{owner}/{repo}
  if (rest.length === 0) {
    if (method === 'GET') {
      ctx.res.writeHead(200, {
        'content-type': JSON_TYPE,
        'x-github-request-id': `req-${ctxSeq(ctx)}`,
        'content-length': String(Buffer.byteLength(repoJson(repo))),
      });
      ctx.res.end(repoJson(repo));
      return;
    }
    if (method === 'PATCH') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const desc = ctx.json.description;
      if (typeof desc === 'string') repo.description = desc;
      repo.jsonCache = null;
      sendJson(ctx, 200, repoObject(repo));
      return;
    }
    if (method === 'DELETE') {
      store.deleteRepo(name);
      sendEmpty(ctx, 204);
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET /repos/{owner}/{repo}/branches[/{branch}]
  if (rest[0] === 'branches') {
    if (method !== 'GET') {
      err(ctx, 404, 'Not Found');
      return;
    }
    const branches = [...repo.refs.keys()]
      .filter((r) => r.startsWith('refs/heads/'))
      .map((r) => r.slice('refs/heads/'.length))
      .sort();
    if (rest.length === 1) {
      sendJson(ctx, 200, branches.map((b) => ({ name: b, commit: { sha: repo.refs.get(`refs/heads/${b}`) ?? '' }, protected: false })));
      return;
    }
    const branch = decode(rest.slice(1).join('/'));
    const tip = repo.refs.get(`refs/heads/${branch}`);
    if (tip === undefined) {
      err(ctx, 404, 'Branch not found');
      return;
    }
    sendJson(ctx, 200, { name: branch, commit: { sha: tip }, protected: false });
    return;
  }

  // GET /repos/{owner}/{repo}/git/refs/heads/{branch}  |  POST /git/refs
  if (rest[0] === 'git' && rest[1] === 'refs') {
    if (method === 'GET' && rest.length >= 3) {
      const ref = decode(rest.slice(2).join('/'));
      const full = ref.startsWith('refs/') ? ref : `refs/${ref}`;
      const tip = repo.refs.get(full);
      if (tip === undefined) {
        err(ctx, 404, 'Not Found');
        return;
      }
      sendJson(ctx, 200, { ref: full, object: { sha: tip, type: 'commit' } });
      return;
    }
    if (method === 'POST' && rest.length === 2) {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const ref = bodyString(ctx, 'ref');
      const sha = bodyString(ctx, 'sha');
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha) || !repo.objects.has(sha)) {
        err(ctx, 422, 'Invalid ref or sha');
        return;
      }
      if (repo.refs.has(ref)) {
        err(ctx, 422, 'Reference already exists');
        return;
      }
      repo.refs.set(ref, sha);
      store.invalidateAdverts(repo);
      sendJson(ctx, 201, { ref, object: { sha, type: 'commit' } });
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET /repos/{owner}/{repo}/commits[?sha=<ref>]
  if (rest[0] === 'commits' && rest.length === 1) {
    if (method !== 'GET') {
      err(ctx, 404, 'Not Found');
      return;
    }
    const ref = ctx.query.get('sha') ?? repo.defaultBranch;
    const tip = store.resolveRef(repo, ref);
    if (tip === null) {
      err(ctx, 404, 'Not Found');
      return;
    }
    const { page, perPage } = pageParams(ctx);
    const walked: Record<string, unknown>[] = [];
    const seen = new Set<string>();
    const stack: string[] = [tip];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const c = parseCommit(obj.data);
      const author = identParts(c.author);
      const committer = identParts(c.committer);
      walked.push({
        sha: oid,
        commit: {
          message: c.message,
          author: { name: author.name, email: author.email, date: author.date },
          committer: { name: committer.name, email: committer.email, date: committer.date },
        },
        author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
        committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
        parents: c.parents.map((p) => ({ sha: p })),
      });
      for (const p of c.parents) stack.push(p);
    }
    sendJson(ctx, 200, paginate(walked, page, perPage), linkHeader(ctx, page, perPage, walked.length));
    return;
  }

  // /repos/{owner}/{repo}/contents/{path...}
  if (rest[0] === 'contents') {
    handleContents(ctx, store, repo, rest.slice(1));
    return;
  }

  // /repos/{owner}/{repo}/labels
  if (rest[0] === 'labels') {
    if (method === 'GET' && rest.length === 1) {
      sendJson(ctx, 200, [...repo.labels.values()].map(labelObject));
      return;
    }
    if (method === 'POST' && rest.length === 1) {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const labelName = bodyString(ctx, 'name');
      if (labelName === '') {
        err(ctx, 422, 'Label name is required');
        return;
      }
      if (repo.labels.has(labelName)) {
        err(ctx, 422, `Label '${labelName}' already exists`);
        return;
      }
      const label: Label = { name: labelName, color: bodyString(ctx, 'color') || 'ededed' };
      repo.labels.set(labelName, label);
      sendJson(ctx, 201, labelObject(label));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // /repos/{owner}/{repo}/issues[/{n}[/comments]]
  if (rest[0] === 'issues') {
    handleIssues(ctx, store, repo, rest.slice(1));
    return;
  }

  // /repos/{owner}/{repo}/pulls[/{n}[/files|merge]]
  if (rest[0] === 'pulls') {
    handlePulls(ctx, store, repo, rest.slice(1));
    return;
  }

  err(ctx, 404, 'Not Found');
}

function handleContents(ctx: Ctx, store: Store, repo: Repo, pathParts: string[]): void {
  const method = ctx.method;
  const path = pathParts.map(decode).join('/');
  const branch = bodyString(ctx, 'branch') || (ctx.query.get('ref') ?? '') || repo.defaultBranch;
  const ref = ctx.query.get('ref') ?? branch;
  const tip = store.resolveRef(repo, ref);

  if (method === 'GET') {
    if (tip === null) {
      err(ctx, 404, 'Not Found');
      return;
    }
    const files = filesAtCommit(repo, tip);
    if (path === '') {
      sendJson(ctx, 200, dirListing(repo, tip, ''));
      return;
    }
    const blobOid = files.get(path);
    if (blobOid !== undefined) {
      const blob = repo.objects.get(blobOid);
      const data = blob !== undefined ? blob.data : Buffer.alloc(0);
      sendJson(ctx, 200, {
        type: 'file',
        encoding: 'base64',
        size: data.length,
        name: path.split('/').pop() ?? path,
        path,
        sha: blobOid,
        content: data.toString('base64'),
      });
      return;
    }
    const listing = dirListing(repo, tip, path);
    if (listing === null) {
      err(ctx, 404, 'Not Found');
      return;
    }
    sendJson(ctx, 200, listing);
    return;
  }

  if (method === 'PUT' || method === 'DELETE') {
    if (ctx.json === null) {
      err(ctx, 400, 'Problems parsing JSON');
      return;
    }
    const message = bodyString(ctx, 'message') || (method === 'PUT' ? `Update ${path}` : `Delete ${path}`);
    const givenSha = bodyString(ctx, 'sha');
    const branchName = bodyString(ctx, 'branch') || repo.defaultBranch;
    // A named branch that does not exist yet starts at the default tip (or empty).
    let branchTip = repo.refs.get(`refs/heads/${branchName}`);
    if (branchTip === undefined && branchName !== repo.defaultBranch) {
      branchTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (branchTip !== undefined) repo.refs.set(`refs/heads/${branchName}`, branchTip);
    }
    const files = branchTip === undefined ? new Map<string, string>() : filesAtCommit(repo, branchTip);
    const current = files.get(path);

    if (method === 'PUT') {
      const contentB64 = bodyString(ctx, 'content');
      if (contentB64 === '') {
        err(ctx, 422, 'Content is required');
        return;
      }
      if (current !== undefined && givenSha !== '' && givenSha !== current) {
        err(ctx, 409, 'The sha does not match the current blob');
        return;
      }
      if (current !== undefined && givenSha === '') {
        err(ctx, 409, 'The sha was not supplied for an existing file');
        return;
      }
      const data = Buffer.from(contentB64, 'base64');
      const blobOid = sha1Hex('blob', data);
      repo.objects.set(blobOid, { type: 'blob', data });
      files.set(path, blobOid);
      const isNew = current === undefined;
      const commitOid = commitFiles(repo, branchName, files, message);
      store.invalidateAdverts(repo);
      sendJson(ctx, isNew ? 201 : 200, {
        content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: data.length, type: 'file', encoding: 'base64', content: contentB64 },
        commit: { sha: commitOid },
      });
      return;
    }

    // DELETE
    if (current === undefined) {
      err(ctx, 404, 'Not Found');
      return;
    }
    if (givenSha !== '' && givenSha !== current) {
      err(ctx, 409, 'The sha does not match the current blob');
      return;
    }
    files.delete(path);
    const commitOid = commitFiles(repo, branchName, files, message);
    store.invalidateAdverts(repo);
    sendJson(ctx, 200, { commit: { sha: commitOid } });
    return;
  }

  err(ctx, 404, 'Not Found');
}

/** Directory listing for `dir` inside a commit's tree, or null when it is not a directory. */
function dirListing(repo: Repo, commitOid: string, dir: string): Record<string, unknown>[] | null {
  const obj = repo.objects.get(commitOid);
  if (!obj || obj.type !== 'commit') return null;
  const rootTree = parseCommit(obj.data).tree;
  let treeOid = rootTree;
  if (dir !== '') {
    const parts = dir.split('/');
    let ok = true;
    for (const part of parts) {
      const treeObj = repo.objects.get(treeOid);
      if (!treeObj || treeObj.type !== 'tree') {
        ok = false;
        break;
      }
      const entry = parseTree(treeObj.data).find((e) => e.name === part && e.mode === '40000');
      if (entry === undefined) {
        ok = false;
        break;
      }
      treeOid = entry.oid;
    }
    if (!ok) return null;
  }
  const treeObj = repo.objects.get(treeOid);
  if (!treeObj || treeObj.type !== 'tree') return null;
  const prefix = dir === '' ? '' : `${dir}/`;
  return parseTree(treeObj.data).map((e) => {
    const isDir = e.mode === '40000';
    const entry: Record<string, unknown> = {
      type: isDir ? 'dir' : 'file',
      name: e.name,
      path: `${prefix}${e.name}`,
      sha: e.oid,
    };
    if (!isDir) {
      const blob = repo.objects.get(e.oid);
      entry.size = blob !== undefined ? blob.data.length : 0;
    }
    return entry;
  });
}

function handleIssues(ctx: Ctx, store: Store, repo: Repo, rest: string[]): void {
  const method = ctx.method;

  // POST / GET /issues
  if (rest.length === 0) {
    if (method === 'POST') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const title = bodyString(ctx, 'title');
      if (title === '') {
        err(ctx, 422, 'Title is required');
        return;
      }
      const labelsRaw = ctx.json.labels;
      const labels: string[] = Array.isArray(labelsRaw) ? labelsRaw.filter((l): l is string => typeof l === 'string') : [];
      for (const name of labels) {
        if (!repo.labels.has(name)) {
          err(ctx, 422, `Label '${name}' does not exist`);
          return;
        }
      }
      repo.issueSeq += 1;
      const issue: Issue = {
        id: store.nextSeq(),
        number: repo.issueSeq,
        title,
        body: bodyString(ctx, 'body'),
        state: 'open',
        labels,
        createdSeq: store.nextSeq(),
        jsonCache: null,
      };
      repo.issues.set(issue.number, issue);
      ctx.res.writeHead(201, {
        'content-type': JSON_TYPE,
        'x-github-request-id': `req-${ctxSeq(ctx)}`,
        'content-length': String(Buffer.byteLength(issueJson(repo, issue))),
      });
      ctx.res.end(issueJson(repo, issue));
      return;
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const direction = ctx.query.get('direction') ?? 'desc';
      let list = [...repo.issues.values()];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
      const { page, perPage } = pageParams(ctx);
      sendJson(ctx, 200, paginate(list, page, perPage).map((i) => issueObject(repo, i)), linkHeader(ctx, page, perPage, list.length));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  const number = parseInt(rest[0], 10);
  const issue = Number.isSafeInteger(number) ? repo.issues.get(number) : undefined;
  if (issue === undefined) {
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET / PATCH /issues/{n}
  if (rest.length === 1) {
    if (method === 'GET') {
      ctx.res.writeHead(200, {
        'content-type': JSON_TYPE,
        'x-github-request-id': `req-${ctxSeq(ctx)}`,
        'content-length': String(Buffer.byteLength(issueJson(repo, issue))),
      });
      ctx.res.end(issueJson(repo, issue));
      return;
    }
    if (method === 'PATCH') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const title = ctx.json.title;
      if (typeof title === 'string') issue.title = title;
      const body = ctx.json.body;
      if (typeof body === 'string') issue.body = body;
      const state = ctx.json.state;
      if (state === 'open' || state === 'closed') issue.state = state;
      issue.jsonCache = null;
      sendJson(ctx, 200, issueObject(repo, issue));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // /issues/{n}/comments
  if (rest.length === 2 && rest[1] === 'comments') {
    if (method === 'GET') {
      const { page, perPage } = pageParams(ctx);
      const all = [...repo.comments.values()]
        .filter((c) => (c as unknown as { issue: number }).issue === number)
        .sort((a, b) => a.createdSeq - b.createdSeq);
      sendJson(
        ctx,
        200,
        paginate(all, page, perPage).map((c) => ({
          id: c.id,
          body: c.body,
          user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
          created_at: NOW_ISO,
          updated_at: NOW_ISO,
        })),
        linkHeader(ctx, page, perPage, all.length),
      );
      return;
    }
    if (method === 'POST') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const commentBody = bodyString(ctx, 'body');
      if (commentBody === '') {
        err(ctx, 422, 'Body is required');
        return;
      }
      repo.commentSeq += 1;
      const comment = { id: store.nextSeq(), body: commentBody, createdSeq: store.nextSeq() };
      (comment as unknown as { issue: number }).issue = number;
      repo.comments.set(comment.id, comment as never);
      sendJson(ctx, 201, {
        id: comment.id,
        body: comment.body,
        user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
        created_at: NOW_ISO,
        updated_at: NOW_ISO,
      });
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  err(ctx, 404, 'Not Found');
}

function handlePulls(ctx: Ctx, store: Store, repo: Repo, rest: string[]): void {
  const method = ctx.method;

  // POST / GET /pulls
  if (rest.length === 0) {
    if (method === 'POST') {
      if (ctx.json === null) {
        err(ctx, 400, 'Problems parsing JSON');
        return;
      }
      const title = bodyString(ctx, 'title');
      const headRef = bodyString(ctx, 'head');
      const baseRef = bodyString(ctx, 'base') || repo.defaultBranch;
      if (title === '' || headRef === '') {
        err(ctx, 422, 'Title and head are required');
        return;
      }
      const headTip = repo.refs.get(`refs/heads/${headRef}`);
      const baseTip = repo.refs.get(`refs/heads/${baseRef}`);
      if (headTip === undefined || baseTip === undefined) {
        err(ctx, 422, 'Head or base branch does not exist');
        return;
      }
      for (const p of repo.pulls.values()) {
        if (p.state === 'open' && p.headRef === headRef && p.baseRef === baseRef) {
          err(ctx, 422, 'A pull request already exists for this head and base');
          return;
        }
      }
      const baseObj = repo.objects.get(baseTip);
      const headObj = repo.objects.get(headTip);
      const baseTree = baseObj !== undefined && baseObj.type === 'commit' ? parseCommit(baseObj.data).tree : '';
      const headTree = headObj !== undefined && headObj.type === 'commit' ? parseCommit(headObj.data).tree : '';
      repo.pullSeq += 1;
      const pull: Pull = {
        id: store.nextSeq(),
        number: repo.pullSeq,
        title,
        body: bodyString(ctx, 'body'),
        state: 'open',
        headRef,
        baseRef,
        headSha: headTip,
        baseSha: baseTip,
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        files: diffTrees(repo.objects, baseTree, headTree),
        createdSeq: store.nextSeq(),
        jsonCache: null,
      };
      repo.pulls.set(pull.number, pull);
      sendJson(ctx, 201, pullObject(repo, pull));
      return;
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let list = [...repo.pulls.values()];
      if (state !== 'all') list = list.filter((p) => p.state === state);
      list.sort((a, b) => b.createdSeq - a.createdSeq);
      const { page, perPage } = pageParams(ctx);
      sendJson(ctx, 200, paginate(list, page, perPage).map((p) => pullObject(repo, p)), linkHeader(ctx, page, perPage, list.length));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  const number = parseInt(rest[0], 10);
  const pull = Number.isSafeInteger(number) ? repo.pulls.get(number) : undefined;
  if (pull === undefined) {
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET /pulls/{n}
  if (rest.length === 1) {
    if (method === 'GET') {
      sendJson(ctx, 200, pullObject(repo, pull));
      return;
    }
    err(ctx, 404, 'Not Found');
    return;
  }

  // GET /pulls/{n}/files
  if (rest.length === 2 && rest[1] === 'files') {
    if (method !== 'GET') {
      err(ctx, 404, 'Not Found');
      return;
    }
    sendJson(
      ctx,
      200,
      pull.files.map((f) => ({ filename: f.filename, status: f.status, additions: f.additions, deletions: f.deletions, changes: f.changes })),
    );
    return;
  }

  // PUT /pulls/{n}/merge
  if (rest.length === 2 && rest[1] === 'merge') {
    if (method !== 'PUT') {
      err(ctx, 404, 'Not Found');
      return;
    }
    if (pull.state !== 'open' || pull.merged) {
      err(ctx, 405, 'Pull Request is not mergeable');
      return;
    }
    if (repoPullConflicted(repo, pull)) {
      err(ctx, 405, 'Merge conflict');
      return;
    }
    const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (headTip === undefined || baseTip === undefined) {
      err(ctx, 405, 'Pull Request is not mergeable');
      return;
    }
    const headObj = repo.objects.get(headTip);
    const headTree = headObj !== undefined && headObj.type === 'commit' ? parseCommit(headObj.data).tree : '';
    const commit = buildCommit({
      tree: headTree,
      parents: [baseTip],
      message: `Merge pull request #${pull.number} from ${pull.headRef}`,
      timestamp: FIXED_EPOCH_SEC,
    });
    const sha = sha1Hex('commit', commit);
    repo.objects.set(sha, { type: 'commit', data: commit });
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = NOW_ISO;
    pull.mergeCommitSha = sha;
    pull.jsonCache = null;
    store.invalidateAdverts(repo);
    sendJson(ctx, 200, { sha, merged: true, message: 'Pull Request successfully merged' });
    return;
  }

  err(ctx, 404, 'Not Found');
}

// ---------------------------------------------------------------- server

function pathSegments(raw: string): string[] {
  let path = raw;
  try {
    path = new URL(raw, 'http://entry').pathname;
  } catch {
    path = '/';
  }
  if (path.length > 1 && path.endsWith('/')) path = path.slice(0, -1);
  return path;
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    requestSeq += 1;
    const seq = requestSeq;
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      const path = pathSegments(req.url ?? '/');
      const method = req.method ?? 'GET';
      let query = new URLSearchParams();
      try {
        query = new URL(req.url ?? '/', 'http://entry').searchParams;
      } catch {
        // Keep an empty query.
      }
      let json: Record<string, unknown> | null = null;
      let jsonBad = false;
      // Only the JSON surfaces get a JSON parse: git smart HTTP posts binary packs.
      const isJsonPath = path.startsWith('/api/') || path === '/mcp';
      const needsJson = isJsonPath && (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE');
      if (needsJson && body.length > 0) {
        try {
          const parsed: unknown = JSON.parse(body.toString('utf8'));
          if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) json = parsed as Record<string, unknown>;
          else json = {};
        } catch {
          jsonBad = true;
        }
      } else if (needsJson) {
        json = {};
      }
      const ctx: Ctx = {
        req,
        res,
        method,
        path,
        query,
        segments: path.split('/').filter((x) => x !== ''),
        body,
        json: jsonBad ? null : json,
        jsonBad,
      };
      (ctx as unknown as { seq: number }).seq = seq;

      try {
        if (method === 'GET' && path === '/healthz') {
          sendJson(ctx, 200, { ok: true });
          return;
        }
        if (jsonBad) {
          err(ctx, 400, 'Problems parsing JSON');
          return;
        }
        if (path === '/api/v3' || path.startsWith('/api/v3/')) {
          const sub = path === '/api/v3' ? [] : path.slice('/api/v3/'.length).split('/').filter((x) => x !== '');
          handleRest({ ...ctx, segments: sub }, store);
          return;
        }
        // Git smart HTTP: /{owner}/{repo}.git/...
        const gitSegs = ctx.segments;
        if (gitSegs.length >= 2 && gitSegs[1].endsWith('.git')) {
          if (!needAuth(ctx)) return;
          const result = handleGit(
            store,
            decode(gitSegs[0]),
            decode(gitSegs[1].slice(0, -4)),
            method,
            gitSegs.slice(2),
            query,
            req.headers as Record<string, string | string[] | undefined>,
            body,
          );
          if (result !== null) {
            res.writeHead(result.status, {
              'content-type': result.contentType,
              'x-github-request-id': `req-${seq}`,
              'content-length': String(result.body.length),
            });
            res.end(result.body);
            return;
          }
        }
        // GraphQL and MCP land here in the next increment.
        err(ctx, 404, 'Not Found');
      } catch (e) {
        try {
          err(ctx, 500, e instanceof Error ? e.message : 'Internal Server Error');
        } catch {
          // The socket is already gone.
        }
      }
    });
    req.on('error', () => {
      try {
        res.destroy();
      } catch {
        // Already gone.
      }
    });
  });
}
