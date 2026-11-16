// mcp.ts — Model Context Protocol over Streamable HTTP at /mcp, speaking the
// modern era 2026-07-28 (server/discover, no handshake, no session) with the
// legacy initialize + notifications/initialized handshake as a fallback.
// JSON-RPC 2.0, plain JSON answers (no SSE needed), one process, the same
// in-memory store REST and GraphQL already share.
//
// Tool results carry both a text block (the JSON) and structuredContent, so
// the suite can read the object either way. A tool failure is isError:true;
// an unknown tool or a malformed request is a JSON-RPC error, never HTTP 500.

import {
  NOW_ISO,
  VIEWER_ID,
  VIEWER_LOGIN,
  flattenTree,
  parseCommit,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

export const MCP_SUPPORTED_VERSIONS: readonly string[] = ['2026-07-28', '2025-11-25', '2025-06-18'];
const SERVER_NAME = 'gh-forge';
const SERVER_VERSION = '1.0.0';

type JsonRecord = Record<string, unknown>;

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function textOf(value: unknown): string {
  return typeof value === 'string' ? value : JSON.stringify(value);
}

// ---------------------------------------------------------------- the tools list (built once, reused verbatim)

interface McpToolDef {
  name: string;
  description: string;
  inputSchema: JsonRecord;
}

function tool(name: string, description: string, properties: JsonRecord, required: string[]): McpToolDef {
  return { name, description, inputSchema: { type: 'object', properties, required } };
}

const TOOLS: readonly McpToolDef[] = [
  tool('create_repo', 'Create a repository for the authenticated user.', {
    name: { type: 'string', description: 'The repository name.' },
    description: { type: 'string', description: 'A short description of the repository.' },
    private: { type: 'boolean', description: 'Whether the repository is private.' },
  }, ['name']),
  tool('get_repo', 'Get a repository by owner and name.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
  }, ['owner', 'repo']),
  tool('create_issue', 'Create an issue in a repository.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    title: { type: 'string', description: 'The issue title.' },
    body: { type: 'string', description: 'The issue body.' },
  }, ['owner', 'repo', 'title']),
  tool('list_issues', 'List the issues of a repository.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    state: { type: 'string', description: 'open, closed, or all.', enum: ['open', 'closed', 'all'] },
  }, ['owner', 'repo']),
  tool('create_pull_request', 'Open a pull request from a head branch into a base branch.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    title: { type: 'string', description: 'The pull request title.' },
    head: { type: 'string', description: 'The head branch name.' },
    base: { type: 'string', description: 'The base branch name.' },
  }, ['owner', 'repo', 'title', 'head', 'base']),
  tool('merge_pull_request', 'Merge a pull request by number (squash).', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    pull_number: { type: 'integer', description: 'The pull request number.' },
  }, ['owner', 'repo', 'pull_number']),
];

// ---------------------------------------------------------------- REST-shaped objects (same fields REST returns)

function repoObject(repo: Repo): JsonRecord {
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

function issueObject(repo: Repo, issue: Issue): JsonRecord {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return label !== undefined ? { name: label.name, color: label.color, description: '' } : { name, color: 'ededed', description: '' };
    }),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function pullObject(repo: Repo, pull: Pull): JsonRecord {
  const headSha = repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha;
  const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: { ref: pull.headRef, sha: headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: baseSha, label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: !pull.merged && pull.state === 'open' && !repoPullConflicted(repo, pull),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: NOW_ISO,
    updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function repoPullConflicted(repo: Repo, pull: Pull): boolean {
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (baseTip === undefined || baseTip === pull.baseSha) return false;
  const baseFiles = repoFilesAt(repo, pull.baseSha);
  const tipFiles = repoFilesAt(repo, baseTip);
  for (const file of pull.files) {
    if (baseFiles.get(file.filename) !== tipFiles.get(file.filename)) return true;
  }
  return false;
}

function repoFilesAt(repo: Repo, commitOid: string): Map<string, string> {
  const obj = repo.objects.get(commitOid);
  if (obj === undefined || obj.type !== 'commit') return new Map();
  return flattenTree(repo.objects, parseCommit(obj.data).tree);
}

// ---------------------------------------------------------------- tool results

interface ToolResult {
  content: { type: string; text: string }[];
  structuredContent?: unknown;
  isError?: boolean;
}

function okResult(value: unknown): ToolResult {
  return { content: [{ type: 'text', text: textOf(value) }], structuredContent: value };
}

function errorResult(message: string): ToolResult {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function argString(args: JsonRecord, key: string): string | null {
  const value = args[key];
  return typeof value === 'string' && value !== '' ? value : null;
}

function argNumber(args: JsonRecord, key: string): number | null {
  const value = args[key];
  if (typeof value === 'number' && Number.isInteger(value)) return value;
  if (typeof value === 'string' && /^\d+$/.test(value)) return Number(value);
  return null;
}

function openRepo(store: Store, args: JsonRecord): Repo | null {
  const owner = argString(args, 'owner');
  const name = argString(args, 'repo');
  if (owner === null || name === null || owner !== VIEWER_LOGIN) return null;
  return store.getRepo(name) ?? null;
}

// ---------------------------------------------------------------- the six tools

function callCreateRepo(store: Store, args: JsonRecord): ToolResult {
  const name = argString(args, 'name');
  if (name === null) return errorResult('create_repo: missing required argument: name');
  if (store.getRepo(name) !== undefined) return errorResult(`create_repo: repository ${name} already exists`);
  const description = typeof args.description === 'string' ? args.description : '';
  const isPrivate = args.private === true;
  const repo = store.createRepo(name, isPrivate, description);
  return okResult(repoObject(repo));
}

function callGetRepo(store: Store, args: JsonRecord): ToolResult {
  const repo = openRepo(store, args);
  if (repo === null) return errorResult('get_repo: repository not found');
  return okResult(repoObject(repo));
}

function callCreateIssue(store: Store, args: JsonRecord): ToolResult {
  const repo = openRepo(store, args);
  if (repo === null) return errorResult('create_issue: repository not found');
  const title = argString(args, 'title');
  if (title === null) return errorResult('create_issue: missing required argument: title');
  const body = typeof args.body === 'string' ? args.body : '';
  const issue = store.createIssue(repo, title, body);
  return okResult(issueObject(repo, issue));
}

function callListIssues(store: Store, args: JsonRecord): ToolResult {
  const repo = openRepo(store, args);
  if (repo === null) return errorResult('list_issues: repository not found');
  const state = typeof args.state === 'string' ? args.state : 'open';
  const list = [...repo.issues.values()]
    .filter((issue) => (state === 'all' ? true : issue.state === state))
    .sort((a, b) => b.createdSeq - a.createdSeq)
    .map((issue) => issueObject(repo, issue));
  return okResult({ issues: list });
}

function callCreatePullRequest(store: Store, args: JsonRecord): ToolResult {
  const repo = openRepo(store, args);
  if (repo === null) return errorResult('create_pull_request: repository not found');
  const title = argString(args, 'title');
  const head = argString(args, 'head');
  const base = argString(args, 'base');
  if (title === null) return errorResult('create_pull_request: missing required argument: title');
  if (head === null) return errorResult('create_pull_request: missing required argument: head');
  if (base === null) return errorResult('create_pull_request: missing required argument: base');
  const pull = store.createPull(repo, title, '', head, base);
  if (pull === null) return errorResult('create_pull_request: head or base branch does not exist, or a pull request already exists');
  return okResult(pullObject(repo, pull));
}

function callMergePullRequest(store: Store, args: JsonRecord): ToolResult {
  const repo = openRepo(store, args);
  if (repo === null) return errorResult('merge_pull_request: repository not found');
  const number = argNumber(args, 'pull_number');
  if (number === null) return errorResult('merge_pull_request: missing required argument: pull_number');
  const pull = repo.pulls.get(number);
  if (pull === undefined) return errorResult(`merge_pull_request: pull request #${number} not found`);
  if (!store.mergePull(repo, pull)) return errorResult(`merge_pull_request: pull request #${number} is not mergeable`);
  return okResult(pullObject(repo, pull));
}

function callTool(store: Store, name: string, args: JsonRecord): ToolResult | null {
  switch (name) {
    case 'create_repo':
      return callCreateRepo(store, args);
    case 'get_repo':
      return callGetRepo(store, args);
    case 'create_issue':
      return callCreateIssue(store, args);
    case 'list_issues':
      return callListIssues(store, args);
    case 'create_pull_request':
      return callCreatePullRequest(store, args);
    case 'merge_pull_request':
      return callMergePullRequest(store, args);
    default:
      return null;
  }
}

// ---------------------------------------------------------------- JSON-RPC plumbing

export interface McpAnswer {
  status: number;
  value: unknown | null;
}

function rpcResult(id: string | number | null, result: unknown): JsonRecord {
  return { jsonrpc: '2.0', id, result };
}

function rpcError(id: string | number | null, code: number, message: string, data?: unknown): JsonRecord {
  const error: JsonRecord = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: '2.0', id, error };
}

function serverInfo(): JsonRecord {
  return { name: SERVER_NAME, version: SERVER_VERSION };
}

function discoverResult(): JsonRecord {
  return {
    supportedVersions: [...MCP_SUPPORTED_VERSIONS],
    capabilities: { tools: {} },
    serverInfo: serverInfo(),
    instructions: 'A GitHub-compatible git forge: create repositories, issues and pull requests over MCP tools.',
    _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo() },
  };
}

/**
 * Handle one parsed POST /mcp body. Auth and the JSON parse already happened
 * in app.ts; a body that is not a JSON-RPC object is an invalid request.
 * Notifications (no id) answer 202 with an empty body.
 */
export function handleMcp(store: Store, body: unknown): McpAnswer {
  if (!isRecord(body)) return { status: 200, value: rpcError(null, -32600, 'Invalid Request') };
  const method = body.method;
  const rawId = body.id;
  const id: string | number | null = typeof rawId === 'string' || typeof rawId === 'number' ? rawId : null;
  if (typeof method !== 'string') return { status: 200, value: rpcError(id, -32600, 'Invalid Request') };

  if (method === 'server/discover') {
    return { status: 200, value: rpcResult(id, discoverResult()) };
  }
  if (method === 'initialize') {
    const params = isRecord(body.params) ? body.params : {};
    const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : null;
    const negotiated = offered !== null && MCP_SUPPORTED_VERSIONS.includes(offered) ? offered : MCP_SUPPORTED_VERSIONS[0];
    return {
      status: 200,
      value: rpcResult(id, { protocolVersion: negotiated, capabilities: { tools: {} }, serverInfo: serverInfo() }),
    };
  }
  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    return { status: 202, value: null };
  }
  if (method === 'ping') {
    return { status: 200, value: rpcResult(id, {}) };
  }
  if (method === 'tools/list') {
    return { status: 200, value: rpcResult(id, { tools: TOOLS }) };
  }
  if (method === 'tools/call') {
    const params = isRecord(body.params) ? body.params : {};
    const name = typeof params.name === 'string' ? params.name : null;
    const args = isRecord(params.arguments) ? params.arguments : {};
    if (name === null || name === '') return { status: 200, value: rpcError(id, -32602, 'tools/call: missing tool name') };
    const result = callTool(store, name, args);
    if (result === null) return { status: 200, value: rpcError(id, -32602, `tools/call: unknown tool "${name}"`) };
    return { status: 200, value: rpcResult(id, result) };
  }
  return { status: 200, value: rpcError(id, -32601, `Method not found: ${method}`) };
}
