import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { inject } from 'vitest';

declare module 'vitest' {
  export interface ProvidedContext {
    wranglerStatePath: string;
  }
}

export const BASE_URL = 'http://localhost:8799';

// api/ — the directory holding wrangler.toml, so `wrangler d1 execute` targets
// the same local D1 the dev server (setup.ts) runs against.
const PROJECT_DIR = resolve(__dirname, '../..');

// Bootstrap admin seeded by setup.ts (adminUpsertSql). Fixtures that need an
// admin session (e.g. createTestUser -> POST /users) log in as this user.
const ADMIN_USERNAME = '__testadmin__';
const ADMIN_PASSWORD = 'testpass123';

// --- Direct local-D1 access -------------------------------------------------
// Replaces the old dev-only /admin/sql[/write] HTTP endpoints. The integration
// server runs `wrangler dev --local`; its D1 is the same local sqlite that
// `wrangler d1 execute --local` targets (see setup.ts, which applies migrations
// the same way). execFileSync avoids the shell so SQL quoting is safe.
function d1Execute(sql: string, json = false): string {
  const statePath = inject('wranglerStatePath');
  if (!statePath) throw new Error('Missing isolated Wrangler test state from global setup');
  const args = ['wrangler', 'd1', 'execute', 'threads', '--local', '--config', 'wrangler.toml', '--persist-to', statePath];
  if (json) args.push('--json');
  args.push('--command', sql);
  try {
    return execFileSync('npx', args, { cwd: PROJECT_DIR, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (e: any) {
    throw new Error(`d1 execute failed for [${sql}]: ${e.stderr || e.stdout || e.message}`, { cause: e });
  }
}

/** Run a write/DDL statement against the local test D1. Throws on SQL error. */
export function execSql(sql: string): void {
  d1Execute(sql);
}

/** Run a SELECT against the local test D1 and return the result rows. */
export function querySql<T = any>(sql: string): T[] {
  const out = d1Execute(sql, true);
  // `wrangler d1 execute --json` prints an array of { results, success, meta }.
  const start = out.indexOf('[');
  const parsed = JSON.parse(start >= 0 ? out.slice(start) : out);
  const first = Array.isArray(parsed) ? parsed[0] : parsed;
  return (first?.results ?? []) as T[];
}

// SQL string-literal escaping for the execSql/querySql helpers above.
export function sqlLit(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

// --- Admin session ----------------------------------------------------------
// Cached login as the bootstrap admin. Used by createTestUser (POST /users is
// gated by the caller's is_admin flag — no server secret).
let adminCookie: string | null = null;
async function adminSession(): Promise<string> {
  if (adminCookie) return adminCookie;
  const res = await fetch(`${BASE_URL}/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Forwarded-For': 'test-admin-bootstrap' },
    body: JSON.stringify({ username: ADMIN_USERNAME, password: ADMIN_PASSWORD }),
  });
  if (!res.ok) {
    throw new Error(`bootstrap admin login failed: ${res.status} ${await res.text()}`);
  }
  const cookie = res.headers.get('set-cookie') ?? '';
  adminCookie = cookie.match(/session=([^;]+)/)?.[1] ?? '';
  if (!adminCookie) throw new Error('bootstrap admin login returned no session cookie');
  return adminCookie;
}

export async function createTestUser(
  username: string,
  password: string = 'testpass123',
  displayName?: string,
  role?: string,
): Promise<{ id: string; username: string; displayName: string | null }> {
  const email = `${username}@example.com`;
  const cookie = await adminSession();
  const res = await fetch(`${BASE_URL}/users`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: `session=${cookie}` },
    body: JSON.stringify({ username, password, email, displayName, role }),
  });
  if (!res.ok && res.status !== 409) {
    const body = await res.text();
    throw new Error(`Failed to create user ${username}: ${res.status} ${body}`);
  }
  if (res.status === 409) {
    // User already exists — return stub; callers use loginUser() for tokens
    return { id: '', username, displayName: displayName ?? null };
  }
  return res.json();
}

/**
 * Promote a user to admin (is_admin = 1). There is no in-app "promote to admin"
 * endpoint, so tests create fixture admins directly in D1.
 * Admins are the ones allowed to create private channels.
 */
export async function makeAdmin(username: string): Promise<void> {
  execSql(`UPDATE users SET is_admin = 1 WHERE username = ${sqlLit(username)}`);
}

/**
 * Trigger the scheduled() cron handler (saved-draft delivery + webhook drain +
 * session prune) via wrangler's --test-scheduled endpoint. Replaces the old
 * POST /saved-drafts/deliver HTTP trigger.
 */
export async function triggerScheduledDelivery(): Promise<void> {
  let lastTransportError: unknown;
  for (let attempt = 0; attempt < 2; attempt++) {
    let res: Response;
    try {
      res = await fetch(`${BASE_URL}/__scheduled`, { method: 'GET' });
    } catch (error) {
      lastTransportError = error;
      if (attempt === 0) {
        await new Promise((resolve) => setTimeout(resolve, 100));
        continue;
      }
      throw error;
    }

    if (!res.ok) {
      throw new Error(`scheduled trigger failed: ${res.status} ${await res.text()}`);
    }
    return;
  }

  throw lastTransportError;
}

export async function loginUser(
  username: string,
  password: string = 'testpass123',
): Promise<{ user: any; sessionToken: string; cookie: string }> {
  const res = await fetch(`${BASE_URL}/auth/login`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      // Unique IP per user so parallel test files don't share a rate-limit bucket
      'X-Forwarded-For': `test-${username}`,
    },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`Failed to login ${username}: ${res.status} ${body}`);
  }
  const cookie = res.headers.get('set-cookie') ?? '';
  const sessionToken = cookie.match(/session=([^;]+)/)?.[1] ?? '';
  const user = await res.json();
  return { user, sessionToken, cookie };
}

/** Type-erased res.json() — use in tests to avoid `unknown` property access errors */
export async function json(res: Response): Promise<any> {
  return res.json();
}

export function authedFetch(
  sessionToken: string,
  path: string,
  options?: RequestInit,
): Promise<Response> {
  // FormData bodies must NOT receive a forced JSON Content-Type — the runtime
  // sets the multipart boundary itself, and an `application/json` header makes
  // the worker fail to parse the upload. Omit it for FormData; callers can still
  // override via options.headers.
  const isFormData = typeof FormData !== 'undefined' && options?.body instanceof FormData;
  return fetch(`${BASE_URL}${path}`, {
    ...options,
    headers: {
      ...(isFormData ? {} : { 'Content-Type': 'application/json' }),
      Cookie: `session=${sessionToken}`,
      ...options?.headers,
    },
  });
}
