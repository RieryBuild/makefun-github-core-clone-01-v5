// The entry <-> env lifecycle, as the trial env settles it: start the forge
// binary, learn the port it bound, tell the env that port, then poll until
// the env says the trial is over and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial
//      container). Without it there is nothing to talk to, so the program
//      exits 1 at once.
//   2. Spawn ./forge (the statically linked Go core committed beside this
//      file). It binds an ephemeral port and prints
//      "[forge] listening on port N" once the socket is live — that line is
//      both the port and the readiness signal, so there is no bind-then-close
//      race and no polling the socket. The binary inherits this process's
//      environment (MAKEFUN_SHOP_TOKEN, MAKEFUN_TRIAL_NOW).
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as the entry's base URL and dials it from
//      then on. The first register wins; a 409 means another entry already
//      registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with a
//      short exponential backoff; the process never exits before the env says
//      done.
//   5. Kill the forge child and exit 0. The harness then stops the env, which
//      writes the export the scorer reads.
//
// main.ts owns the lifecycle and nothing else: no package, no Node built-in
// beyond child_process and the fetch client, no model call. The forge itself
// is Go (go/*.go, committed beside the prebuilt binary).

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;
/** How long to wait for the forge binary to print its listening port. */
const FORGE_START_TIMEOUT_MS = 30_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The env client: register + done-polling, both retried with capped exponential backoff. */
class EnvClient {
  private readonly base: string;

  constructor(base: string) {
    this.base = base;
  }

  /** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
  private delay(attempt: number): number {
    return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
  }

  /**
   * POST the bound port until the env accepts it. 200 is the normal answer;
   * 409 means a first register already won, which is not this process's to
   * fix. 400 and 405 are contract errors and end the program; a network error
   * or another status is retried, since the env may still be starting.
   */
  async register(port: number): Promise<void> {
    for (let attempt = 0; ; attempt += 1) {
      try {
        const res = await fetch(`${this.base}/register`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ port }),
          signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
        });
        if (res.status === 200 || res.status === 409) return;
        if (res.status === 400 || res.status === 405) {
          throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
        }
      } catch (err) {
        if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
      }
      await sleep(this.delay(attempt));
    }
  }

  /** Poll GET /done until it answers {"done":true}; never return before that. */
  async waitForDone(): Promise<void> {
    for (let attempt = 0; ; attempt += 1) {
      try {
        const res = await fetch(`${this.base}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
        if (res.ok) {
          const body = (await res.json()) as { done?: unknown };
          if (body.done === true) return;
        }
      } catch {
        // The env is not answering yet, or the poll timed out: it is not done, so poll again.
      }
      await sleep(this.delay(attempt));
    }
  }
}

/**
 * Start the forge binary and wait for its listening-port line. Resolves with
 * the child and its port; rejects when the binary exits early or stays silent
 * past the timeout.
 */
function startForge(binaryPath: string): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(binaryPath, [], { stdio: ['ignore', 'pipe', 'pipe'] });
    let settled = false;
    let buffered = '';
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGKILL');
      reject(new Error(`the forge binary did not print its port within ${FORGE_START_TIMEOUT_MS} ms`));
    }, FORGE_START_TIMEOUT_MS);
    timer.unref?.();

    const fail = (message: string): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(message));
    };
    child.once('error', (err) => fail(`could not start the forge binary: ${err.message}`));
    child.once('exit', (code, signal) => {
      fail(`the forge binary exited early (code ${code}, signal ${signal})`);
    });
    child.stderr?.on('data', (chunk: Buffer) => {
      process.stderr.write(chunk);
    });
    child.stdout?.on('data', (chunk: Buffer) => {
      if (settled) {
        process.stdout.write(chunk);
        return;
      }
      buffered += chunk.toString('utf8');
      const match = /\[forge\] listening on port (\d+)/.exec(buffered);
      if (match === null) return;
      settled = true;
      clearTimeout(timer);
      process.stdout.write(buffered);
      child.stdout?.on('data', (later: Buffer) => {
        process.stdout.write(later);
      });
      resolve({ child, port: Number(match[1]) });
    });
  });
}

/** Start the forge, register its port, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const root = dirname(fileURLToPath(import.meta.url));
  const binaryPath = join(root, 'forge');
  const { child, port } = await startForge(binaryPath);
  process.stdout.write(`[entry] forge listening on port ${port}\n`);

  let stopping = false;
  const stopChild = (): void => {
    if (stopping) return;
    stopping = true;
    child.kill('SIGTERM');
    setTimeout(() => child.kill('SIGKILL'), 5_000).unref?.();
  };
  process.once('SIGTERM', () => {
    stopChild();
    process.exit(143);
  });
  process.once('SIGINT', () => {
    stopChild();
    process.exit(130);
  });
  child.once('exit', (code, signal) => {
    if (!stopping) {
      process.stderr.write(`[entry] the forge binary died mid-trial (code ${code}, signal ${signal})\n`);
      process.exit(1);
    }
  });

  const env = new EnvClient(ENV_URL);
  await env.register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await env.waitForDone();
  stopChild();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
