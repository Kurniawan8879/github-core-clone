// graphql.ts — a small GraphQL executor for the documents gh and
// github-mcp-server actually send: a tokenizer, a recursive-descent parser,
// a validator against a fixed schema table, and resolvers over the one store
// REST and git already share. No GraphQL library, stdlib only.
//
// Supported surface: Query.viewer/repository/node/__type, Repository and its
// issue/pull-request connections, Issue, PullRequest (+ files), the five
// mutations the suite sends, __type introspection, and batched operation
// arrays. Unknown fields are GraphQL errors (HTTP 200), never HTTP errors.

import {
  VIEWER_ID,
  VIEWER_LOGIN,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

// ---------------------------------------------------------------- tokenizer

type TokKind = 'name' | 'string' | 'int' | 'float' | 'punct' | 'spread' | 'eof';
interface Tok {
  kind: TokKind;
  value: string;
}

function tokenize(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  const isNameStart = (c: string): boolean => /[A-Za-z_]/.test(c);
  const isNameChar = (c: string): boolean => /[A-Za-z0-9_]/.test(c);
  while (i < src.length) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src[i + 1] === '.' && src[i + 2] === '.') {
      toks.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}():[]!=$@&|'.includes(c as string)) {
      toks.push({ kind: 'punct', value: c as string });
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1];
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc === '"' ? '"' : esc === '\\' ? '\\' : esc;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      toks.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c as string)) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j] as string)) j += 1;
      const text = src.slice(i, j);
      toks.push({ kind: text.includes('.') ? 'float' : 'int', value: text });
      i = j;
      continue;
    }
    if (isNameStart(c as string)) {
      let j = i;
      while (j < src.length && isNameChar(src[j] as string)) j += 1;
      toks.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(c)} in GraphQL document`);
  }
  toks.push({ kind: 'eof', value: '' });
  return toks;
}

// ---------------------------------------------------------------- parser

export type Value =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; items: Value[] }
  | { kind: 'object'; fields: Record<string, Value> };

export type Selection =
  | { kind: 'field'; alias: string; name: string; args: Record<string, Value>; children: Selection[] }
  | { kind: 'fragment'; typeCondition: string | null; children: Selection[] };

export interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  selections: Selection[];
}

class Parser {
  private pos = 0;
  private readonly toks: Tok[];

  constructor(toks: Tok[]) {
    this.toks = toks;
  }

  private peek(): Tok {
    return this.toks[this.pos] as Tok;
  }

  private next(): Tok {
    const tok = this.toks[this.pos] as Tok;
    this.pos += 1;
    return tok;
  }

  private expectPunct(value: string): void {
    const tok = this.next();
    if (tok.kind !== 'punct' || tok.value !== value) throw new Error(`expected '${value}' in GraphQL document, got '${tok.value}'`);
  }

  private expectName(what = 'a name'): string {
    const tok = this.next();
    if (tok.kind !== 'name') throw new Error(`expected ${what} in GraphQL document, got '${tok.value}'`);
    return tok.value;
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.peek().kind !== 'eof') {
      ops.push(this.parseOperation());
    }
    return ops;
  }

  private parseOperation(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    const first = this.peek();
    if (first.kind === 'name' && (first.value === 'query' || first.value === 'mutation')) {
      kind = this.next().value as 'query' | 'mutation';
    }
    let name: string | null = null;
    if (this.peek().kind === 'name') name = this.next().value;
    // Variable definitions: ($name: Type = default, ...) — names and defaults only.
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        this.expectPunct('$');
        this.expectName('a variable name');
        this.expectPunct(':');
        this.parseType();
        if (this.peek().kind === 'punct' && this.peek().value === '=') {
          this.next();
          this.parseValue();
        }
      }
      this.expectPunct(')');
    }
    // Directives on the operation (none of the suite's documents use any).
    this.skipDirectives();
    const selections = this.parseSelectionSet();
    return { kind, name, selections };
  }

  private parseType(): void {
    const tok = this.peek();
    if (tok.kind === 'punct' && tok.value === '[') {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName('a type name');
    }
    if (this.peek().kind === 'punct' && this.peek().value === '!') this.next();
  }

  private skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName('a directive name');
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.parseArguments();
    }
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const out: Selection[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      if (this.peek().kind === 'eof') throw new Error('unterminated selection set');
      out.push(this.parseSelection());
    }
    this.expectPunct('}');
    return out;
  }

  private parseSelection(): Selection {
    if (this.peek().kind === 'spread') {
      this.next();
      let typeCondition: string | null = null;
      if (this.peek().kind === 'name' && this.peek().value === 'on') {
        this.next();
        typeCondition = this.expectName('a type condition');
      } else if (this.peek().kind === 'name') {
        throw new Error('named fragments are not supported');
      }
      this.skipDirectives();
      return { kind: 'fragment', typeCondition, children: this.parseSelectionSet() };
    }
    const first = this.expectName('a field name');
    let alias = first;
    let name = first;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      name = this.expectName('a field name');
    }
    const args = this.peek().kind === 'punct' && this.peek().value === '(' ? this.parseArguments() : {};
    this.skipDirectives();
    const children = this.peek().kind === 'punct' && this.peek().value === '{' ? this.parseSelectionSet() : [];
    return { kind: 'field', alias, name, args, children };
  }

  private parseArguments(): Record<string, Value> {
    this.expectPunct('(');
    const args: Record<string, Value> = {};
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      const name = this.expectName('an argument name');
      this.expectPunct(':');
      args[name] = this.parseValue();
    }
    this.expectPunct(')');
    return args;
  }

  private parseValue(): Value {
    const tok = this.next();
    if (tok.kind === 'punct' && tok.value === '$') {
      return { kind: 'variable', name: this.expectName('a variable name') };
    }
    if (tok.kind === 'string') return { kind: 'string', value: tok.value };
    if (tok.kind === 'int') return { kind: 'int', value: parseInt(tok.value, 10) };
    if (tok.kind === 'float') return { kind: 'float', value: parseFloat(tok.value) };
    if (tok.kind === 'name') {
      if (tok.value === 'true') return { kind: 'boolean', value: true };
      if (tok.value === 'false') return { kind: 'boolean', value: false };
      if (tok.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: tok.value };
    }
    if (tok.kind === 'punct' && tok.value === '[') {
      const items: Value[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) items.push(this.parseValue());
      this.expectPunct(']');
      return { kind: 'list', items };
    }
    if (tok.kind === 'punct' && tok.value === '{') {
      const fields: Record<string, Value> = {};
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName('an object field name');
        this.expectPunct(':');
        fields[name] = this.parseValue();
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    throw new Error(`unexpected value token '${tok.value}'`);
  }
}

// ---------------------------------------------------------------- schema table (validation only)

const SCHEMA: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  User: ['login', 'id'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'url', 'createdAt', 'updatedAt'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'url', 'author', 'labels', 'comments'],
  IssueConnection: ['nodes', 'pageInfo', 'totalCount'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'endCursor', 'startCursor'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'createdAt', 'updatedAt', 'url', 'author', 'headRef', 'baseRef'],
  PullRequestConnection: ['nodes', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changes'],
  PullRequestChangedFileConnection: ['nodes', 'pageInfo', 'totalCount'],
  Ref: ['name', 'target'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'clientMutationId'],
  CommentEdge: ['node', 'cursor'],
  Comment: ['id', 'body', 'author', 'createdAt', 'updatedAt'],
  __Type: ['name', 'fields', 'enumValues', 'kind', 'description'],
  __Field: ['name', 'description', 'type'],
  __EnumValue: ['name', 'description'],
  Node: ['id'],
};

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'url', 'author', 'labels', 'comments'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'createdAt', 'updatedAt', 'url', 'author', 'headRef', 'baseRef'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'url', 'createdAt', 'updatedAt'],
  SearchType: [],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE', 'COMMIT', 'MILESTONE', 'PACKAGE', 'WIKI', 'PROJECT'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
};

// ---------------------------------------------------------------- global ids

function b64(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64');
}

function unb64(text: string): string | null {
  try {
    const decoded = Buffer.from(text, 'base64').toString('utf8');
    return decoded.includes(':') ? decoded : null;
  } catch {
    return null;
  }
}

export function repoGlobalId(repo: Repo): string {
  return b64(`Repository:${repo.id}`);
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return b64(`Issue:${repo.id}:${issue.number}`);
}

export function pullGlobalId(repo: Repo, pull: Pull): string {
  return b64(`PullRequest:${repo.id}:${pull.number}`);
}

export function commentGlobalId(repo: Repo, issueNumber: number, commentId: number): string {
  return b64(`IssueComment:${repo.id}:${issueNumber}:${commentId}`);
}

function cursorFor(repo: Repo, number: number): string {
  return b64(`IssueCursor:${repo.id}:${number}`);
}

function cursorNumber(cursor: string): number | null {
  const decoded = unb64(cursor);
  if (decoded === null) return null;
  const m = /^IssueCursor:(\d+):(\d+)$/.exec(decoded);
  return m !== null ? parseInt(m[2], 10) : null;
}

// ---------------------------------------------------------------- raw values (what resolvers return)

type Raw = Record<string, unknown> & { __typename: string };

function userRaw(): Raw {
  return { __typename: 'User', login: VIEWER_LOGIN, id: b64(`User:${VIEWER_ID}`) };
}

function refRaw(name: string): Raw {
  return { __typename: 'Ref', name };
}

function repoRaw(repo: Repo): Raw {
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  return {
    __typename: 'Repository',
    id: repoGlobalId(repo),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: userRaw(),
    defaultBranchRef: tip === undefined ? null : refRaw(repo.defaultBranch),
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
    description: repo.description,
    url: `https://github.com/${repo.owner}/${repo.name}`,
    _repo: repo,
  };
}

function issueRaw(repo: Repo, issue: Issue): Raw {
  return {
    __typename: 'Issue',
    id: issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    body: issue.body,
    author: userRaw(),
    _repo: repo,
    _issue: issue,
  };
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'closed' ? 'CLOSED' : 'OPEN';
}

function pullRaw(repo: Repo, pull: Pull): Raw {
  return {
    __typename: 'PullRequest',
    id: pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pullState(pull),
    merged: pull.merged,
    headRefName: pull.headRef,
    baseRefName: pull.baseRef,
    _repo: repo,
    _pull: pull,
  };
}

function commentRaw(repo: Repo, issueNumber: number, comment: { id: number; body: string }): Raw {
  return {
    __typename: 'Comment',
    id: commentGlobalId(repo, issueNumber, comment.id),
    body: comment.body,
    author: userRaw(),
  };
}

// ---------------------------------------------------------------- execution

interface GqlError {
  message: string;
}

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  errors: GqlError[];
}

function resolveValue(value: Value, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'variable':
      return variables[value.name] ?? null;
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'list':
      return value.items.map((item) => resolveValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(value.fields)) out[k] = resolveValue(v, variables);
      return out;
    }
  }
}

function resolveArgs(args: Record<string, Value>, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(args)) out[k] = resolveValue(v, variables);
  return out;
}

function repoOf(ctx: ExecCtx, raw: Raw): Repo | null {
  const repo = raw._repo as Repo | undefined;
  return repo !== undefined ? repo : null;
}

function findRepoByGlobalId(ctx: ExecCtx, id: string): Repo | null {
  const decoded = unb64(id);
  if (decoded === null) return null;
  const m = /^Repository:(\d+)$/.exec(decoded);
  if (m === null) return null;
  const numeric = parseInt(m[1], 10);
  for (const repo of ctx.store.repos.values()) if (repo.id === numeric) return repo;
  return null;
}

function decodeGlobalId(id: string): { type: string; repoId: number; rest: string[] } | null {
  const decoded = unb64(id);
  if (decoded === null) return null;
  const parts = decoded.split(':');
  if (parts.length < 2) return null;
  const type = parts[0] as string;
  const repoId = parseInt(parts[1] as string, 10);
  if (!Number.isSafeInteger(repoId)) return null;
  return { type, repoId, rest: parts.slice(2) };
}

function findRepoById(ctx: ExecCtx, numeric: number): Repo | null {
  for (const repo of ctx.store.repos.values()) if (repo.id === numeric) return repo;
  return null;
}

function issuesConnection(ctx: ExecCtx, repo: Repo, args: Record<string, unknown>): Raw {
  let list = [...repo.issues.values()];
  const states = args.states as string[] | undefined;
  if (Array.isArray(states) && states.length > 0) {
    list = list.filter((issue) => states.includes(issue.state === 'open' ? 'OPEN' : 'CLOSED'));
  }
  const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
  const dir = orderBy?.direction === 'ASC' ? 1 : -1;
  list.sort((a, b) => dir * (a.createdSeq - b.createdSeq));
  const after = args.after as string | null | undefined;
  if (typeof after === 'string' && after !== '') {
    const n = cursorNumber(after);
    if (n !== null) list = list.filter((issue) => (dir === 1 ? issue.number > n : issue.number < n));
  }
  const first = typeof args.first === 'number' ? args.first : list.length;
  const page = list.slice(0, first);
  const hasNextPage = list.length > first;
  const endCursor = page.length > 0 ? cursorFor(repo, (page[page.length - 1] as Issue).number) : null;
  return {
    __typename: 'IssueConnection',
    nodes: page.map((issue) => issueRaw(repo, issue)),
    pageInfo: { __typename: 'PageInfo', hasNextPage, endCursor, hasPreviousPage: false, startCursor: null },
    totalCount: list.length,
  };
}

function pullsConnection(repo: Repo, args: Record<string, unknown>): Raw {
  let list = [...repo.pulls.values()].filter((pull) => pull.state === 'open');
  list.sort((a, b) => b.createdSeq - a.createdSeq);
  const first = typeof args.first === 'number' ? args.first : list.length;
  const page = list.slice(0, first);
  return {
    __typename: 'PullRequestConnection',
    nodes: page.map((pull) => pullRaw(repo, pull)),
    pageInfo: { __typename: 'PageInfo', hasNextPage: list.length > first, endCursor: null, hasPreviousPage: false, startCursor: null },
    totalCount: list.length,
  };
}

function filesConnection(pull: Pull, args: Record<string, unknown>): Raw {
  const first = typeof args.first === 'number' ? args.first : pull.files.length;
  const page = pull.files.slice(0, first);
  return {
    __typename: 'PullRequestChangedFileConnection',
    nodes: page.map((f) => ({ __typename: 'PullRequestChangedFile', path: f.filename, additions: f.additions, deletions: f.deletions, changes: f.changes })),
    pageInfo: { __typename: 'PageInfo', hasNextPage: pull.files.length > first, endCursor: null, hasPreviousPage: false, startCursor: null },
    totalCount: pull.files.length,
  };
}

function typeRaw(name: string): Raw | null {
  const fields = TYPE_FIELDS[name];
  const enums = TYPE_ENUM_VALUES[name];
  if (fields === undefined && enums === undefined) return null;
  return {
    __typename: '__Type',
    name,
    fields: (fields ?? []).map((f) => ({ __typename: '__Field', name: f })),
    enumValues: (enums ?? []).map((e) => ({ __typename: '__EnumValue', name: e })),
    kind: enums !== undefined ? 'ENUM' : 'OBJECT',
  };
}

function mergeableOf(ctx: ExecCtx, repo: Repo, pull: Pull): string {
  if (pull.merged || pull.state !== 'open') return 'UNKNOWN';
  return ctx.store.pullConflicted(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
}

/**
 * Read one field of a raw value. Returns null for a missing nullable field.
 * Mutations run here (they are fields of the Mutation type).
 */
function readField(ctx: ExecCtx, parentType: string, raw: Raw | null, name: string, args: Record<string, unknown>): unknown {
  if (raw === null) return null;
  if (name === '__typename') return raw.__typename;

  if (parentType === 'Query') {
    if (name === 'viewer') return userRaw();
    if (name === 'repository') {
      const owner = args.owner as string | undefined;
      const repoName = args.name as string | undefined;
      if (owner !== VIEWER_LOGIN || typeof repoName !== 'string') return null;
      const repo = ctx.store.getRepo(repoName);
      return repo === undefined ? null : repoRaw(repo);
    }
    if (name === 'node') {
      const id = args.id as string | undefined;
      if (typeof id !== 'string') return null;
      const decoded = decodeGlobalId(id);
      if (decoded === null) return null;
      const repo = findRepoById(ctx, decoded.repoId);
      if (repo === null) return null;
      if (decoded.type === 'Repository') return repoRaw(repo);
      if (decoded.type === 'Issue') {
        const issue = repo.issues.get(parseInt(decoded.rest[0] as string, 10));
        return issue === undefined ? null : issueRaw(repo, issue);
      }
      if (decoded.type === 'PullRequest') {
        const pull = repo.pulls.get(parseInt(decoded.rest[0] as string, 10));
        return pull === undefined ? null : pullRaw(repo, pull);
      }
      return null;
    }
    if (name === '__type') {
      const typeName = args.name as string | undefined;
      return typeof typeName === 'string' ? typeRaw(typeName) : null;
    }
  }

  if (parentType === 'Repository') {
    const repo = repoOf(ctx, raw);
    if (repo === null) return null;
    if (name === 'issue') {
      const number = args.number as number | undefined;
      if (typeof number !== 'number') return null;
      const issue = repo.issues.get(number);
      return issue === undefined ? null : issueRaw(repo, issue);
    }
    if (name === 'issues') return issuesConnection(ctx, repo, args);
    if (name === 'pullRequests') return pullsConnection(repo, args);
    if (name === 'pullRequest') {
      const number = args.number as number | undefined;
      if (typeof number !== 'number') return null;
      const pull = repo.pulls.get(number);
      return pull === undefined ? null : pullRaw(repo, pull);
    }
    if (name === 'owner') return userRaw();
    if (name === 'defaultBranchRef') {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return tip === undefined ? null : refRaw(repo.defaultBranch);
    }
  }

  if (parentType === 'IssueConnection' || parentType === 'PullRequestConnection' || parentType === 'PullRequestChangedFileConnection') {
    if (name === 'nodes') return (raw.nodes as unknown[]) ?? [];
    if (name === 'pageInfo') return raw.pageInfo ?? null;
    if (name === 'totalCount') return raw.totalCount ?? 0;
  }

  if (parentType === 'PullRequest') {
    const repo = repoOf(ctx, raw);
    const pull = raw._pull as Pull | undefined;
    if (repo === null || pull === undefined) return null;
    if (name === 'mergeable') return mergeableOf(ctx, repo, pull);
    if (name === 'files') return filesConnection(pull, args);
  }

  if (parentType === 'Mutation') {
    const input = args.input as Record<string, unknown> | undefined;
    if (input === undefined) {
      ctx.errors.push({ message: `Field "${name}" requires an input argument` });
      return null;
    }
    if (name === 'createIssue') {
      const repo = findRepoByGlobalId(ctx, String(input.repositoryId ?? ''));
      if (repo === null) {
        ctx.errors.push({ message: 'createIssue: unknown repositoryId' });
        return null;
      }
      const issue = ctx.store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''));
      return { __typename: 'CreateIssuePayload', issue: issueRaw(repo, issue) };
    }
    if (name === 'closeIssue') {
      const decoded = typeof input.issueId === 'string' ? decodeGlobalId(input.issueId) : null;
      if (decoded === null || decoded.type !== 'Issue') {
        ctx.errors.push({ message: 'closeIssue: unknown issueId' });
        return null;
      }
      const repo = findRepoById(ctx, decoded.repoId);
      const issue = repo?.issues.get(parseInt(decoded.rest[0] as string, 10));
      if (repo === null || issue === undefined) {
        ctx.errors.push({ message: 'closeIssue: unknown issueId' });
        return null;
      }
      issue.state = 'closed';
      issue.jsonCache = null;
      issue.jsonCacheLen = 0;
      return { __typename: 'CloseIssuePayload', issue: issueRaw(repo, issue) };
    }
    if (name === 'createPullRequest') {
      const repo = findRepoByGlobalId(ctx, String(input.repositoryId ?? ''));
      if (repo === null) {
        ctx.errors.push({ message: 'createPullRequest: unknown repositoryId' });
        return null;
      }
      const pull = ctx.store.createPull(repo, String(input.title ?? ''), '', String(input.headRefName ?? ''), String(input.baseRefName ?? ''));
      if (pull === null) {
        ctx.errors.push({ message: 'createPullRequest: head or base branch does not exist, or a pull request already exists' });
        return null;
      }
      return { __typename: 'CreatePullRequestPayload', pullRequest: pullRaw(repo, pull) };
    }
    if (name === 'mergePullRequest') {
      const decoded = typeof input.pullRequestId === 'string' ? decodeGlobalId(input.pullRequestId) : null;
      if (decoded === null || decoded.type !== 'PullRequest') {
        ctx.errors.push({ message: 'mergePullRequest: unknown pullRequestId' });
        return null;
      }
      const repo = findRepoById(ctx, decoded.repoId);
      const pull = repo?.pulls.get(parseInt(decoded.rest[0] as string, 10));
      if (repo === null || pull === undefined) {
        ctx.errors.push({ message: 'mergePullRequest: unknown pullRequestId' });
        return null;
      }
      if (!ctx.store.mergePull(repo, pull)) {
        ctx.errors.push({ message: 'mergePullRequest: pull request is not mergeable' });
        return null;
      }
      return { __typename: 'MergePullRequestPayload', pullRequest: pullRaw(repo, pull) };
    }
    if (name === 'addComment') {
      const decoded = typeof input.subjectId === 'string' ? decodeGlobalId(input.subjectId) : null;
      if (decoded === null || decoded.type !== 'Issue') {
        ctx.errors.push({ message: 'addComment: unknown subjectId' });
        return null;
      }
      const repo = findRepoById(ctx, decoded.repoId);
      const issueNumber = parseInt(decoded.rest[0] as string, 10);
      const issue = repo?.issues.get(issueNumber);
      if (repo === null || issue === undefined) {
        ctx.errors.push({ message: 'addComment: unknown subjectId' });
        return null;
      }
      repo.commentSeq += 1;
      const comment = { id: ctx.store.nextSeq(), body: String(input.body ?? ''), createdSeq: ctx.store.nextSeq() };
      (comment as unknown as { issue: number }).issue = issueNumber;
      repo.comments.set(comment.id, comment as never);
      return {
        __typename: 'AddCommentPayload',
        commentEdge: { __typename: 'CommentEdge', node: commentRaw(repo, issueNumber, comment), cursor: null },
      };
    }
  }

  if (parentType === '__Type') {
    if (name === 'fields') return (raw.fields as unknown[]) ?? [];
    if (name === 'enumValues') return (raw.enumValues as unknown[]) ?? [];
  }

  // A plain scalar field stored on the raw value.
  if (name in raw && typeof raw[name] !== 'object') return raw[name];
  if (name in raw) return raw[name];
  return null;
}

/** Execute one operation's selections against a root raw value. */
function executeSelections(
  ctx: ExecCtx,
  selections: Selection[],
  parentType: string,
  parent: Raw | null,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'fragment') {
      if (selection.typeCondition !== null && parent !== null && selection.typeCondition !== parent.__typename) continue;
      if (selection.typeCondition !== null && parent === null) continue;
      Object.assign(out, executeSelections(ctx, selection.children, parentType, parent));
      continue;
    }
    const allowed = SCHEMA[parentType];
    if (allowed !== undefined && !allowed.includes(selection.name) && selection.name !== '__typename') {
      ctx.errors.push({ message: `Cannot query field "${selection.name}" on type "${parentType}".` });
      out[selection.alias] = null;
      continue;
    }
    const args = resolveArgs(selection.args, ctx.variables);
    const value = readField(ctx, parentType, parent, selection.name, args);
    out[selection.alias] = executeValue(ctx, selection.children, value);
  }
  return out;
}

/** Recurse into a resolved value: scalars pass through, objects/lists re-execute. */
function executeValue(ctx: ExecCtx, children: Selection[], value: unknown): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => executeValue(ctx, children, entry));
  if (typeof value === 'object') {
    const raw = value as Raw;
    if (children.length === 0) return value;
    if (typeof raw.__typename !== 'string') return value;
    return executeSelections(ctx, children, raw.__typename, raw);
  }
  return value;
}

/** Execute one parsed operation; returns the {data, errors} envelope. */
export function executeOperation(
  store: Store,
  operation: Operation,
  variables: Record<string, unknown>,
): { data: Record<string, unknown> | null; errors: GqlError[] } {
  const ctx: ExecCtx = { store, variables, errors: [] };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const root: Raw = { __typename: rootType };
  try {
    const data = executeSelections(ctx, operation.selections, rootType, root);
    return { data, errors: ctx.errors };
  } catch (e) {
    ctx.errors.push({ message: e instanceof Error ? e.message : 'execution failed' });
    return { data: null, errors: ctx.errors };
  }
}

/** Parse one document string into operations; throws on a syntax error. */
export function parseDocument(src: string): Operation[] {
  return new Parser(tokenize(src)).parseDocument();
}

/** Run one {query, variables, operationName} request. */
export function executeRequest(
  store: Store,
  body: { query?: unknown; variables?: unknown; operationName?: unknown },
): { data: Record<string, unknown> | null; errors: GqlError[] } {
  if (typeof body.query !== 'string' || body.query.trim() === '') {
    return { data: null, errors: [{ message: 'Must provide query string.' }] };
  }
  let operations: Operation[];
  try {
    operations = parseDocument(body.query);
  } catch (e) {
    return { data: null, errors: [{ message: e instanceof Error ? e.message : 'syntax error' }] };
  }
  if (operations.length === 0) {
    return { data: null, errors: [{ message: 'Must provide query string.' }] };
  }
  let operation = operations[0] as Operation;
  if (typeof body.operationName === 'string') {
    const named = operations.find((op) => op.name === body.operationName);
    if (named === undefined) {
      return { data: null, errors: [{ message: `Unknown operation named "${body.operationName}".` }] };
    }
    operation = named;
  }
  const variables = (body.variables !== null && typeof body.variables === 'object' && !Array.isArray(body.variables)
    ? (body.variables as Record<string, unknown>)
    : {}) as Record<string, unknown>;
  return executeOperation(store, operation, variables);
}
