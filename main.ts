// main.ts — the entrypoint: bind an ephemeral port, register it with the env,
// then wait until the env says the trial is done.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to talk
//      to, so exit 1 right away. A --standalone flag runs the server locally for
//      testing instead (it never registers and never exits on its own).
//   2. Listen on port 0: the kernel picks a free port. Never 8080 — the env owns it.
//   3. POST {"port": N} to $MAKEFUN_ENV_URL/register. A 200 registers; a 409 means
//      another entry already won the slot, which is fine — keep waiting.
//   4. Poll GET $MAKEFUN_ENV_URL/done until it answers {"done": true}, then close
//      the server and exit 0. Retries back off exponentially; a failed poll is
//      never a reason to exit early.

import { createApp } from './app.ts';

const STANDALONE = process.argv.includes('--standalone');
const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '' && !STANDALONE) {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_FIRST_MS = 250;
const RETRY_MAX_MS = 2_000;

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

async function main(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  process.stdout.write(`[entry] listening on port ${address.port}\n`);
  if (STANDALONE) {
    process.stdout.write('[entry] standalone mode: no env to register with\n');
    return;
  }
  await register(address.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  if (!STANDALONE) process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
