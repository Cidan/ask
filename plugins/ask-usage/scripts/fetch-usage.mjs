#!/usr/bin/env node
// ask-usage plugin hook.
//
// Parent (SessionStart hook) does a 30s throttle check on $CLAUDE_PLUGIN_ROOT/.cache.json
// and either exits immediately (fresh) or spawns a detached child that performs the
// actual fetch. Child does: credential load (file-only, then darwin Keychain fallback
// when file is absent) → GET api.anthropic.com/api/oauth/usage → atomic write to
// .cache.json. Silent-fail on every error path. NEVER calls the OAuth refresh
// endpoint — claude itself handles refresh; we're a passive reader.

import { readFileSync, writeFileSync, statSync, existsSync, renameSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { userInfo, homedir, platform } from 'node:os';
import { spawn, execFileSync } from 'node:child_process';
import https from 'node:https';
import { fileURLToPath } from 'node:url';
import { createHash, randomBytes } from 'node:crypto';

const THROTTLE_MS = 30_000;
const API_TIMEOUT_MS = 10_000;
const USAGE_HOST = 'api.anthropic.com';
const USAGE_PATH = '/api/oauth/usage';
const OAUTH_BETA = 'oauth-2025-04-20';

const pluginRoot = process.env.CLAUDE_PLUGIN_ROOT;
if (!pluginRoot) process.exit(0);
const cacheFile = join(pluginRoot, '.cache.json');

const CHILD_MARKER = 'ASK_USAGE_FETCH_CHILD';
const isChild = process.env[CHILD_MARKER] === '1';

if (isChild) {
  runFetch().catch(() => {}).finally(() => process.exit(0));
} else {
  runThrottleAndSpawn();
}

function runThrottleAndSpawn() {
  try {
    const st = statSync(cacheFile);
    if (Date.now() - st.mtimeMs < THROTTLE_MS) return;
  } catch {
    // no cache yet — proceed
  }
  try {
    const scriptPath = fileURLToPath(import.meta.url);
    const child = spawn(process.execPath, [scriptPath], {
      detached: true,
      stdio: 'ignore',
      env: { ...process.env, [CHILD_MARKER]: '1' },
    });
    child.unref();
  } catch {
    // spawn failure → just give up this round
  }
}

async function runFetch() {
  const creds = loadCredentials();
  if (!creds || !creds.accessToken) return;
  if (creds.expiresAt && creds.expiresAt <= Date.now()) return;

  const resp = await fetchUsage(creds.accessToken);
  if (!resp) return;

  const out = normalize(resp);
  if (!out) return;

  writeCacheAtomic(out);
}

function configDir() {
  return process.env.CLAUDE_CONFIG_DIR || join(homedir(), '.claude');
}

function loadCredentials() {
  const fromFile = readCredentialsFile();
  if (fromFile && fromFile.accessToken) return fromFile;
  if (platform() === 'darwin') {
    const fromKeychain = readKeychainCredentials();
    if (fromKeychain && fromKeychain.accessToken) return fromKeychain;
  }
  return null;
}

function readCredentialsFile() {
  try {
    const path = join(configDir(), '.credentials.json');
    if (!existsSync(path)) return null;
    const raw = readFileSync(path, 'utf-8');
    const parsed = JSON.parse(raw);
    const creds = parsed.claudeAiOauth || parsed;
    if (!creds || !creds.accessToken) return null;
    return { accessToken: creds.accessToken, expiresAt: creds.expiresAt };
  } catch {
    return null;
  }
}

function keychainServiceName() {
  const dir = process.env.CLAUDE_CONFIG_DIR;
  if (!dir) return 'Claude Code-credentials';
  // Match claude's own hash convention so we look up the right per-profile entry.
  const hash = createHash('sha256').update(dir).digest('hex').slice(0, 8);
  return `Claude Code-credentials-${hash}`;
}

function readKeychainCredentials() {
  try {
    const service = keychainServiceName();
    const user = (userInfo().username || '').trim();
    const args = user
      ? ['find-generic-password', '-s', service, '-a', user, '-w']
      : ['find-generic-password', '-s', service, '-w'];
    const out = execFileSync('/usr/bin/security', args, {
      encoding: 'utf-8',
      timeout: 2000,
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
    if (!out) return null;
    const parsed = JSON.parse(out);
    const creds = parsed.claudeAiOauth || parsed;
    if (!creds || !creds.accessToken) return null;
    return { accessToken: creds.accessToken, expiresAt: creds.expiresAt };
  } catch {
    return null;
  }
}

function fetchUsage(accessToken) {
  const host = process.env.ASK_USAGE_API_HOST || USAGE_HOST;
  return new Promise((resolve) => {
    let req;
    try {
      req = https.request(
        {
          hostname: host,
          path: USAGE_PATH,
          method: 'GET',
          headers: {
            'Authorization': `Bearer ${accessToken}`,
            'anthropic-beta': OAUTH_BETA,
            'Content-Type': 'application/json',
          },
          timeout: API_TIMEOUT_MS,
        },
        (res) => {
          let data = '';
          res.on('data', (chunk) => { data += chunk; });
          res.on('end', () => {
            if (res.statusCode == null || res.statusCode < 200 || res.statusCode >= 300) {
              resolve(null);
              return;
            }
            try {
              resolve(JSON.parse(data));
            } catch {
              resolve(null);
            }
          });
        },
      );
    } catch {
      resolve(null);
      return;
    }
    req.on('error', () => resolve(null));
    req.on('timeout', () => { try { req.destroy(); } catch {} resolve(null); });
    req.end();
  });
}

function normalize(r) {
  if (!r || typeof r !== 'object') return null;
  const fh = r.five_hour || {};
  const sd = r.seven_day || {};
  const out = {
    timestamp: Date.now(),
    fiveHourPercent: clampPct(fh.utilization),
    weeklyPercent: clampPct(sd.utilization),
    fiveHourResetsAt: toIsoOrNull(fh.resets_at),
    weeklyResetsAt: toIsoOrNull(sd.resets_at),
  };
  return out;
}

function clampPct(v) {
  const n = Number(v);
  if (!Number.isFinite(n)) return 0;
  if (n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

function toIsoOrNull(s) {
  if (!s || typeof s !== 'string') return null;
  const t = Date.parse(s);
  if (Number.isNaN(t)) return null;
  return new Date(t).toISOString();
}

function writeCacheAtomic(obj) {
  try {
    const dir = dirname(cacheFile);
    const tmp = join(dir, `.cache.json.${randomBytes(4).toString('hex')}.tmp`);
    writeFileSync(tmp, JSON.stringify(obj), { mode: 0o600 });
    renameSync(tmp, cacheFile);
  } catch {
    // best-effort
  }
}
