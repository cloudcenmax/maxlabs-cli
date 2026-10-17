// Opt-in live acceptance-test relay. It counts usage for every planner and child
// request without placing real credentials in either test client's environment.
import { createServer } from 'node:http';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { OAuthSession, defaultAuthStore } from '../src/auth.ts';
import { parseUsage } from '../src/transport.ts';

const authURL = (process.env.MAXLABS_AUTH_URL || 'https://console.maxlabs.cenmax.in').replace(/\/$/, '');
const oauth = new OAuthSession(authURL, defaultAuthStore());
const reportPath = resolve(process.env.GREENFIELD_E2E_REPORT || '/private/tmp/maxlabs-greenfield-e2e/usage.json');
const port = Number(process.env.GREENFIELD_E2E_PROXY_PORT || 18790);
const maxCalls = Number(process.env.GREENFIELD_E2E_MAX_CALLS || 120);
let report = { started_at: new Date().toISOString(), updated_at: '', calls: [], totals_by_role: {}, note: 'Provider-reported usage. Reasoning is a subset of output; cached input is counted separately. Unknown receipts are explicitly flagged. This script performs live billable inference only when a client submits requests.' };
try { const previous = JSON.parse(await readFile(reportPath, "utf8")); if (Array.isArray(previous.calls) && previous.started_at) report = previous; } catch (error) { if (error.code !== "ENOENT") throw error; }
let writes = Promise.resolve();
function persist() {
  report.updated_at = new Date().toISOString();
  const totals = {};
  for (const call of report.calls) {
    const key = `${call.role}:${call.model}`;
    totals[key] ||= { calls: 0, unknown_usage: 0, uncachedInputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: 0, reasoningTokens: 0, webSearches: 0 };
    totals[key].calls++;
    if (!call.usage) totals[key].unknown_usage++;
    for (const [field, value] of Object.entries(call.usage || {})) totals[key][field] += value;
  }
  report.totals_by_role = totals;
  const snapshot = JSON.stringify(report, null, 2) + '\n';
  writes = writes.then(async () => { await mkdir(dirname(reportPath), { recursive: true }); await writeFile(reportPath + '.tmp', snapshot, { mode: 0o600 }); await rename(reportPath + '.tmp', reportPath); });
  return writes;
}
function classify(body) {
  const first = body.messages?.find(m => m.role === 'user')?.content || '';
  const text = typeof first === 'string' ? first : JSON.stringify(first);
  if (text.startsWith('Research only.')) return 'research';
  if (text.startsWith('Implement only')) return (/"role"\s*:\s*"qa"/.test(text) || /Task role:\s*"qa"/.test(text)) ? 'qa' : 'worker';
  return 'planner';
}
const server = createServer(async (req, res) => {
  const path = req.url?.split('?')[0];
  if (!((req.method === 'GET' && path === '/v1/models') || (req.method === 'POST' && path === '/v1/chat/completions'))) { res.writeHead(404); res.end('Not a test inference endpoint'); return; }
  let call;
  const started = Date.now();
  const abort = new AbortController();
  res.on('close', () => { if (!res.writableEnded) abort.abort(); });
  try {
    let requestBody = '';
    for await (const chunk of req) { requestBody += chunk; if (requestBody.length > 8_000_000) throw new Error('Test request exceeds 8 MB'); }
    const body = requestBody ? JSON.parse(requestBody) : {};
    if (req.method === 'POST') {
      if (report.calls.length >= maxCalls) { res.writeHead(429); res.end(JSON.stringify({ error: 'Live E2E call budget reached' })); return; }
      call = { id: report.calls.length + 1, role: classify(body), model: body.model, started_at: new Date().toISOString(), status: 0, duration_ms: 0, usage: null };
      report.calls.push(call);
      console.log(`[request ${call.id}] ${call.role} / ${call.model}`);
    }
    const token = await oauth.accessToken(abort.signal);
    if (!token) throw new Error('Sign in to MaxLabs CLI before running live acceptance tests');
    const upstream = await fetch(`${authURL}/app/v1${path.slice(3)}`, { method: req.method, headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json', accept: 'text/event-stream', 'x-session-id': String(req.headers['x-session-id'] || 'greenfield-e2e') }, body: req.method === 'POST' ? requestBody : undefined, signal: abort.signal });
    if (call) call.status = upstream.status;
    const contentType = upstream.headers.get('content-type') || 'application/json';
    res.writeHead(upstream.status, { 'content-type': contentType, 'cache-control': 'no-cache', 'x-accel-buffering': 'no' });
    let buffer = '';
    let jsonBody = '';
    const consume = line => {
      if (!line.startsWith('data:')) return;
      try { const chunk = JSON.parse(line.slice(5).trim()); if (chunk.usage && call) { call.raw_usage = chunk.usage; call.usage = parseUsage(chunk.usage); } } catch {}
    };
    if (upstream.body) for await (const chunk of upstream.body) {
      res.write(chunk);
      const text = Buffer.from(chunk).toString('utf8');
      if (contentType.includes('event-stream')) { buffer += text; const lines = buffer.split('\n'); buffer = lines.pop(); for (const line of lines) consume(line.replace(/\r$/, '')); }
      else if (jsonBody.length < 8_000_000) jsonBody += text;
    }
    if (buffer) consume(buffer);
    if (!contentType.includes('event-stream') && call) { try { const payload = JSON.parse(jsonBody); if (payload.usage) { call.raw_usage = payload.usage; call.usage = parseUsage(payload.usage); } else call.error = payload.error?.message || payload.error || `HTTP ${upstream.status}`; } catch {} }
    if (call) { call.duration_ms = Date.now() - started; console.log(`[receipt ${call.id}] ${JSON.stringify(call.usage || { unknown: true, status: call.status })}`); await persist(); }
    res.end();
  } catch (error) {
    if (call) { call.error = error.message; call.duration_ms = Date.now() - started; await persist(); }
    if (!res.headersSent) res.writeHead(502, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ error: error.message }));
  }
});
server.listen(port, '127.0.0.1', () => console.log(`Live E2E relay: http://127.0.0.1:${port}/v1 · report ${reportPath}`));
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { void persist().finally(() => server.close(() => process.exit())); });
