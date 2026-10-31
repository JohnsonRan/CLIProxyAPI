// End-to-end smoke test of the built-in usage store and the panel's /usage page.
// Expects the core on BASE (config.yaml here) and fake-upstream.js on :18999, then drives a
// headless Chrome/Edge over the DevTools protocol. Exits non-zero on the first failed check.
//   CHROME=/usr/bin/google-chrome BASE=http://127.0.0.1:18317 bun smoke.ts
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const BASE = process.env.BASE ?? 'http://127.0.0.1:18317';
const CHROME = process.env.CHROME ?? 'google-chrome';
const SHOTS = process.env.SHOTS ?? '';
const MGMT = { Authorization: 'Bearer mgmt-secret' };
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
/** Polls fn every 500 ms until it returns a truthy value or timeoutMs passes; returns the last value. */
const waitFor = async <T>(fn: () => Promise<T>, timeoutMs = 15000): Promise<T> => {
  const deadline = Date.now() + timeoutMs;
  let value = await fn();
  while (!value && Date.now() < deadline) {
    await sleep(500);
    value = await fn();
  }
  return value;
};
const failures: string[] = [];
const check = (name: string, ok: unknown, detail = '') => {
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failures.push(name);
};

const chat = (content: string, key = 'sk-smoke-client-key-0001', model = 'gpt-5') =>
  fetch(`${BASE}/v1/chat/completions`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'content-type': 'application/json' },
    body: JSON.stringify({ model, messages: [{ role: 'user', content }], reasoning_effort: 'high' }),
  }).then((r) => r.status);
const api = async (path: string) =>
  (await fetch(`${BASE}/v8/management/observability/usage/${path}`, { headers: MGMT })).json();

// 1. Traffic and API checks.
const statuses = await Promise.all([
  ...Array.from({ length: 6 }, () => chat('hello')),
  ...Array.from({ length: 3 }, () => chat('hi', 'sk-smoke-client-key-0002', 'gpt-5-mini')),
  chat('fail please'),
]);
check('chat requests', statuses.filter((s) => s === 200).length === 9, statuses.join(' '));
// Usage events are written in batches about once a second.
await waitFor(async () => (await api('events?limit=50')).events?.length === 10);
const stats = await api('stats');
check('stats totals', stats.totals?.requests === 10 && stats.totals?.failed === 1, JSON.stringify(stats.totals));
check('stats exact for recent range', stats.exact === true);
const events = await api('events?limit=50');
check('events stored', events.events?.length === 10);
check('transport recorded', events.events?.every((e: { transport: string }) => e.transport === 'http'));
const rt = await api('realtime?minutes=15');
check('realtime counts', rt.current?.rpm_1m >= 10 && typeof rt.in_flight === 'number');
check('options', (await api('options')).models?.length === 2);

// 2. Browser checks.
const proc = spawn(
  CHROME,
  ['--headless=new', '--no-sandbox', '--remote-debugging-port=9333', `--user-data-dir=${mkdtempSync(join(tmpdir(), 'smoke-'))}`, '--window-size=1440,1000', 'about:blank'],
  { stdio: 'ignore' }
);
let wsUrl = '';
for (let i = 0; i < 60 && !wsUrl; i++) {
  await sleep(250);
  try {
    const list = (await (await fetch('http://127.0.0.1:9333/json')).json()) as { type: string; webSocketDebuggerUrl: string }[];
    wsUrl = list.find((t) => t.type === 'page')?.webSocketDebuggerUrl ?? '';
  } catch {
    // Browser still starting.
  }
}
check('browser started', wsUrl);
if (!wsUrl) process.exit(1);
const ws = new WebSocket(wsUrl);
await new Promise((r) => (ws.onopen = r));
let id = 0;
const pending = new Map<number, (v: { result?: { result?: { value?: unknown }; data?: string } }) => void>();
const consoleErrors: string[] = [];
ws.onmessage = (ev) => {
  const msg = JSON.parse(String(ev.data));
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)!(msg);
    pending.delete(msg.id);
  }
  if (msg.method === 'Runtime.exceptionThrown') consoleErrors.push(String(msg.params.exceptionDetails.text));
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
    consoleErrors.push(msg.params.args.map((a: { value?: unknown; description?: string }) => a.value ?? a.description).join(' '));
  }
};
const cdp = (method: string, params: object = {}) =>
  new Promise<{ result?: { result?: { value?: unknown }; data?: string } }>((resolve) => {
    const n = ++id;
    pending.set(n, resolve);
    ws.send(JSON.stringify({ id: n, method, params }));
  });
const ev = async (expression: string) =>
  (await cdp('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })).result?.result?.value;
const shot = async (name: string) => {
  if (!SHOTS) return;
  const r = await cdp('Page.captureScreenshot', { format: 'png' });
  writeFileSync(join(SHOTS, name), Buffer.from(r.result?.data ?? '', 'base64'));
};
// Tabs are addressed by id so the checks do not depend on the UI language.
const clickTab = (tab: string) => ev(`document.getElementById('usage-tab-${tab}')?.click(); 1`);

await cdp('Runtime.enable');
await cdp('Page.enable');
await cdp('Emulation.setFocusEmulationEnabled', { enabled: true });
await cdp('Page.navigate', { url: `${BASE}/management.html` });
await sleep(3000);
await ev(`(() => { const el = document.querySelector('input[name="cpa-management-key"]');
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, 'mgmt-secret');
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); return 1; })()`);
await waitFor(async () => !(await ev(`!!document.querySelector('input[name="cpa-management-key"]')`)));
await ev(`location.hash = '#/usage'; 1`);
await waitFor(async () => Boolean(await ev(`!!document.getElementById('usage-tab-overview') && document.querySelectorAll('tbody tr').length > 0`)));
const header = String(await ev(`document.querySelector('main')?.innerText ?? ''`));
check('overview renders', Boolean(await ev(`!!document.getElementById('usage-tab-overview')`)) && /\b10\b/.test(header), header.slice(0, 120));
// The per-key table loads with the rest of the overview; wait for it rather than a fixed delay.
check('client key name shown', await waitFor(async () => /Smoke Alice/.test(String(await ev(`document.querySelector('main')?.innerText ?? ''`))), 10000));
await shot('overview.png');

await clickTab('events');
const rowCount = async () => Number(await ev(`document.querySelectorAll('tbody tr').length`));
const rows = await waitFor(async () => ((await rowCount()) === 10 ? 10 : 0), 10000);
check('events tab rows', rows === 10, String(rows));
await ev(`[...document.querySelectorAll('tbody tr button')].find(b => /gpt-5-mini/.test(b.innerText))?.click(); 1`);
check('click-to-filter', await waitFor(async () => (await rowCount()) === 3, 10000));
await ev(`document.querySelector('tbody tr[tabindex]')?.click(); 1`);
await sleep(800);
// Keep a handle on this dialog: other dialogs (notices on a fresh profile) may be open too.
check('detail dialog opens', await ev(`!!(window.__detail = [...document.querySelectorAll('[role=dialog]')].pop())`));
await ev(`(() => { const o = window.__detail.closest('.modal-overlay');
  o.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
  o.dispatchEvent(new MouseEvent('click', { bubbles: true })); return 1; })()`);
// The dialog unmounts after its close animation.
check('backdrop closes dialog', await waitFor(async () => !(await ev(`window.__detail.isConnected`)), 5000));
await ev(`document.querySelectorAll('[class*=chipRemove]').forEach((b) => b.click()); 1`);
await sleep(1500);

await clickTab('realtime');
const before = await waitFor(rowCount, 10000);
await Promise.all([chat('live 1'), chat('live 2')]);
// The feed polls every 5 seconds.
await waitFor(async () => (await rowCount()) >= before + 2, 15000);
const after = await rowCount();
check('realtime feed grows', after === before + 2, `${before} -> ${after}`);
await shot('realtime.png');

for (const tab of ['analysis', 'pricing', 'overview']) {
  await clickTab(tab);
  await sleep(2500);
}
check('no console errors', consoleErrors.length === 0, consoleErrors.join(' | '));

ws.close();
proc.kill();
console.log(failures.length ? `\n${failures.length} check(s) failed` : '\nall checks passed');
process.exit(failures.length ? 1 : 0);
