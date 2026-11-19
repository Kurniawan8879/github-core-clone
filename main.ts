// main.ts — MINIMAL Go feasibility probe entrypoint (github-core-clone-01).
//
// Same handshake as the Node entry (gh-p4ab/main.ts), but the HTTP server is
// a statically linked Go binary spawned as a child process:
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to
//      talk to, so exit 1 right away. A --standalone flag runs the server
//      locally for testing instead (it never registers and never exits).
//   2. Spawn ./goserver/forge with port "0" (kernel picks a free port; never
//      8080 — the env owns it). Read the real port from the child's stdout
//      line "[go] listening on <port>", then poll /healthz until it answers
//      (20 s boot budget, matching BOOT_BUDGET_MS in the kit's wire.ts).
//   3. POST {"port": N} to $MAKEFUN_ENV_URL/register. A 200 registers; a 409
//      means another entry already won the slot, which is fine — keep waiting.
//   4. Poll GET $MAKEFUN_ENV_URL/done until it answers {"done": true}, then
//      kill the child and exit 0. Retries back off exponentially; a failed
//      poll is never a reason to exit early.
//   5. If the Go child crashes or never becomes healthy, exit 1 with a clear
//      stderr line — a dead forge scores 0, so fail loudly, not silently.
//
// Node 22, stdlib only. The Go binary is vendored source in this repo
// (goserver/forge, built CGO_ENABLED=0, -trimpath, -ldflags "-s -w").

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const STANDALONE = process.argv.includes('--standalone');
const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '' && !STANDALONE) {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_FIRST_MS = 250;
const RETRY_MAX_MS = 2_000;
const BOOT_BUDGET_MS = 20_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_FIRST_MS * 2 ** Math.min(attempt, 8));
}

async function postJson(url: string, body: unknown): Promise<number> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
  });
  await res.text();
  return res.status;
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const status = await postJson(`${ENV_URL}/register`, { port });
      if (status === 200 || status === 409) return;
      if (status === 400 || status === 405) throw new Error(`register answered HTTP ${status}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // Not answering yet: poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForHealth(port: number, deadlineMs: number): Promise<void> {
  for (;;) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(2_000) });
      if (res.ok) return;
    } catch {
      // Not up yet.
    }
    if (Date.now() > deadlineMs) throw new Error(`go child did not answer /healthz within ${BOOT_BUDGET_MS} ms`);
    await sleep(50);
  }
}

async function main(): Promise<void> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const bin = join(dir, 'goserver', 'forge');
  const child: ChildProcess = spawn(bin, ['0'], { stdio: ['ignore', 'pipe', 'pipe'] });
  let childExited: { code: number | null; signal: NodeJS.Signals | null } | null = null;
  child.on('exit', (code, signal) => {
    childExited = { code, signal };
  });
  child.stderr?.on('data', (chunk: Buffer) => process.stderr.write(chunk));
  const portPromise = new Promise<number>((resolve, reject) => {
    let buf = '';
    child.stdout?.on('data', (chunk: Buffer) => {
      buf += chunk.toString('utf8');
      const m = buf.match(/\[go\] listening on (\d+)/);
      if (m) resolve(Number(m[1]));
    });
    child.on('error', reject);
    child.on('exit', (code, signal) => reject(new Error(`go child exited early (code=${code} signal=${signal})`)));
  });
  const deadlineMs = Date.now() + BOOT_BUDGET_MS;
  const port = await Promise.race([
    portPromise,
    sleep(BOOT_BUDGET_MS).then(() => {
      throw new Error(`go child did not print its port within ${BOOT_BUDGET_MS} ms`);
    }),
  ]);
  process.stdout.write(`[entry] go child listening on port ${port}\n`);
  await waitForHealth(port, deadlineMs);
  if (childExited !== null) throw new Error(`go child exited before register (code=${childExited.code})`);
  if (STANDALONE) {
    process.stdout.write('[entry] standalone mode: no env to register with\n');
    return; // stays alive serving until killed; the child dies with this process group
  }
  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  child.kill('SIGTERM');
  await new Promise<void>((resolve) => {
    if (childExited !== null) resolve();
    else child.once('exit', () => resolve());
    setTimeout(() => {
      try {
        child.kill('SIGKILL');
      } catch {
        // Already gone.
      }
      resolve();
    }, 2_000).unref();
  });
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  if (!STANDALONE) process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
