import { spawn, execSync, execFileSync, type ChildProcess } from 'child_process';
import { resolve } from 'path';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import type { TestProject } from 'vitest/node';
import { adminUpsertSql } from '../../scripts/seed-lib.mjs';

const PORT = 8799;
const PROJECT_DIR = resolve(__dirname, '../..');
const SHUTDOWN_TIMEOUT_MS = 5000;
let wranglerProcess: ChildProcess | undefined;
let statePath: string | undefined;

const delay = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

function waitForProcessClose(proc: ChildProcess, timeoutMs: number): Promise<boolean> {
  return new Promise((resolve) => {
    const finish = (closed: boolean) => {
      clearTimeout(timer);
      proc.off('close', onClose);
      resolve(closed);
    };
    const onClose = () => finish(true);

    const timer = setTimeout(() => finish(false), timeoutMs);
    proc.once('close', onClose);
  });
}

function signalWranglerProcess(proc: ChildProcess, signal: 'SIGTERM' | 'SIGKILL'): void {
  if (!proc.pid) return;

  try {
    // Wrangler may spawn workers under npx; on POSIX, signal the whole group.
    if (process.platform !== 'win32') {
      process.kill(-proc.pid, signal);
      return;
    }
  } catch {
    // Fall back to the direct child below.
  }

  proc.kill(signal);
}

async function waitForServer(url: string, timeoutMs = 30000): Promise<void> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetch(url);
      // Any response means server is up (even 404 is fine)
      await res.body?.cancel();
      if (res) return;
    } catch {
      // Connection refused, keep retrying
    }
    await delay(500);
  }
  throw new Error(`Server did not start within ${timeoutMs}ms`);
}

export async function setup(project: TestProject) {
  statePath = mkdtempSync(resolve(tmpdir(), 'threads-api-tests-'));
  project.provide('wranglerStatePath', statePath);
  const persistArgs = ['--persist-to', statePath];
  // Kill any stale wrangler process on the test port from a previous run
  try {
    execSync(`lsof -ti :${PORT} | xargs kill -9`, { stdio: 'pipe' });
    // Brief pause to let the port free up
    await delay(500);
  } catch {
    // No process on port — expected on clean runs
  }

  // Apply all migrations to local D1 before starting the server. A single
  // Wrangler invocation is much faster in CI than shelling out once per file.
  execFileSync('npx', ['wrangler', 'd1', 'migrations', 'apply', 'threads', '--local', '--config', 'wrangler.toml', ...persistArgs], {
    cwd: PROJECT_DIR,
    stdio: 'pipe',
    env: { ...process.env, CI: 'true' },
  });

  // Seed the bootstrap admin the fixtures log in as: createTestUser() calls
  // POST /users, which requires an is_admin session (no server secret anymore).
  // helpers.ts logs in as this user and mints everything else over HTTP.
  execFileSync('npx', ['wrangler', 'd1', 'execute', 'threads', '--local', '--config', 'wrangler.toml', ...persistArgs, '--command', await adminUpsertSql('__testadmin__', 'testpass123', 'Test Admin', '__testadmin__@example.com', true)], {
    cwd: PROJECT_DIR,
    stdio: 'pipe',
  });

  // Start wrangler dev server.
  wranglerProcess = spawn('npx', [
    'wrangler',
    'dev',
    '--config', 'wrangler.toml',
    '--port', String(PORT),
    '--local',
    ...persistArgs,
    '--test-scheduled',
    // Override wrangler.toml's ENVIRONMENT="production" so the server runs in
    // development mode for the test suite.
    '--var', 'ENVIRONMENT:development',
    // CI does not have local .dev.vars; keep test-only secrets explicit.
    '--var', 'VAPID_PUBLIC_KEY:BA6n6rqLHmUaIgnp5JMo3erC-sfHdA9A5y_4_JsXjBxyG1f-UZM6E112vR49kvbAdzluqL5MgJzQTuA3qj7Pm1g',
    '--var', 'VAPID_PRIVATE_KEY:HBTUbA2kqY9DZzKtDaNHS_X7AVTv7iUQM8KaI0kk7s0',
    '--var', 'VAPID_SUBJECT:mailto:test@example.com',
    '--var', 'CREDENTIAL_MASTER_KEY:test-credential-master-key-v1',
    '--var', 'MFA_ENCRYPTION_KEY:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY',
    '--var', 'INSTANCE_SECRET:test-instance-secret-that-is-at-least-32-bytes',
    // Never inherit a developer's real Resend key into integration tests. Email
    // delivery behavior is exercised with a stub in the Worker service suite.
    '--var', 'RESEND_API_KEY:',
    // Force the secure default for webhook URL validation, overriding any local
    // .dev.vars that enables the localhost escape hatch - tests assert the SSRF
    // guard rejects localhost/private targets.
    '--var', 'WEBHOOK_ALLOW_INSECURE_URLS:false',
    '--var', 'APP_ORIGIN:https://test-instance.example',
  ], {
    cwd: PROJECT_DIR,
    detached: process.platform !== 'win32',
    stdio: ['ignore', 'ignore', 'pipe'],
    env: { ...process.env },
  });

  // Log stderr for debugging failures
  wranglerProcess.stderr?.on('data', (data: Buffer) => {
    const msg = data.toString();
    if (msg.includes('ERROR') || msg.includes('error')) {
      console.error('[wrangler stderr]', msg);
    }
  });

  wranglerProcess.on('error', (err) => {
    console.error('[wrangler process error]', err);
  });

  await waitForServer(`http://localhost:${PORT}/channels`);
}

export async function teardown() {
  const proc = wranglerProcess;
  wranglerProcess = undefined;
  if (!proc) {
    if (statePath) rmSync(statePath, { recursive: true, force: true });
    statePath = undefined;
    return;
  }

  if (proc.exitCode === null && proc.signalCode === null) {
    signalWranglerProcess(proc, 'SIGTERM');
    const closed = await waitForProcessClose(proc, SHUTDOWN_TIMEOUT_MS);

    if (!closed && proc.exitCode === null && proc.signalCode === null) {
      signalWranglerProcess(proc, 'SIGKILL');
      await waitForProcessClose(proc, SHUTDOWN_TIMEOUT_MS);
    }
  }

  proc.stderr?.destroy();
  if (statePath) rmSync(statePath, { recursive: true, force: true });
  statePath = undefined;
}
