import { stat, realpath, lstat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { extname, isAbsolute, relative, resolve } from 'node:path';
import { Workspace } from './tools.ts';
import { safeTaskPath } from './greenfield.ts';

// This is a narrow shell lexer, not a shell interpreter. Unsupported syntax
// falls back to the ordinary guard. Only validated output destinations disappear
// from the command used for permission assessment; execution uses the original.
const tokens = /(?:"[^"\\]*"|'[^'\\]*'|[^\s'"|&;<>\\])+|&&|\|\||&>|>>|>&|[|;>\n]/g;
const privatePart = /^(?:\.aws|\.ssh|\.npmrc|\.netrc|\.pypirc|\.git-credentials|auth\.json|credentials\.json|service-account\.json)$/i;
function word(raw: string): string {
  let quote = '', value = '';
  for (const char of raw) {
    if (quote) { if (char === quote) quote = ''; else value += char; }
    else if (char === '"' || char === "'") quote = char;
    else value += char;
  }
  return value;
}
async function localOutput(root: string, target: string): Promise<boolean> {
  if (['/dev/null', '/dev/stdout', '/dev/stderr'].includes(target) || (process.platform === 'win32' && target.toUpperCase() === 'NUL')) return true;
  const candidate = resolve(root, target);
  if (target.split(/[\\/]/).some(part => part === '..' || privatePart.test(part))) return false;
  for (const directory of [root, tmpdir(), ...(process.platform === 'win32' ? [] : ['/tmp'])]) {
    const rel = relative(directory, candidate);
    if (!safeTaskPath(rel)) continue;
    if (directory !== root && (!isAbsolute(target) || !['.log', '.txt', '.tap', '.xml', '.json'].includes(extname(candidate).toLowerCase()))) continue;
    try {
      const ws = await Workspace.open(directory);
      const path = await ws.path(rel, true);
      if (!safeTaskPath(relative(ws.root, path))) continue;
      try { if (!(await stat(path)).isFile()) continue; }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') continue; }
      return true;
    } catch { /* Check the next permitted output root. */ }
  }
  return false;
}
export async function workerAutoCommand(root: string, command: string): Promise<string | undefined> {
  if (/[$`\\\r<]/.test(command)) return undefined;
  const parts = [...command.matchAll(tokens)];
  let consumed = 0;
  for (const part of parts) {
    if (command.slice(consumed, part.index).trim()) return undefined;
    consumed = part.index! + part[0].length;
  }
  if (command.slice(consumed).trim()) return undefined;
  const ranges: Array<[number, number, string]> = [];
  // Models often repeat their assigned cwd. It must not look like an escape
  // merely because the runner worktree lives under an absolute metadata path.
  if (parts.length >= 3 && word(parts[0][0]) === 'cd' && parts[2][0] === '&&') {
    try {
      const workspace = await Workspace.open(root);
      if (await realpath(resolve(root, word(parts[1][0]))) === workspace.root) ranges.push([parts[0].index!, parts[2].index! + 2, " "]);
    } catch { /* Other directory changes retain ordinary review. */ }
  }
  for (let i = 0; i < parts.length; i++) {
    const token = parts[i], op = token[0];
    if (!['>', '>>', '>&', '&>'].includes(op)) continue;
    const next = parts[++i];
    if (!next) return undefined;
    const target = word(next[0]);
    if (op === '>&') { if (!/^[12]$/.test(target)) return undefined; }
    else if (!await localOutput(root, target)) return undefined;
    let start = token.index!;
    const previous = parts[i - 2];
    if (previous && /^\d+$/.test(previous[0]) && previous.index! + previous[0].length === start) start = previous.index!;
    ranges.push([start, next.index! + next[0].length, " "]);
  }
  // Bootstrapping a private worker copy from its non-secret template is normal
  // setup. Existing environment files are preserved unless cp explicitly uses -n.
  for (let i = 0; i < parts.length; i++) {
    if (word(parts[i][0]) !== 'cp' || (i > 0 && !['&&', '||', ';', '\n'].includes(parts[i-1][0]))) continue;
    let j=i+1, preserve=false;
    if (parts[j]?.[0] === '-n') { preserve=true; j++; }
    if (parts[j]?.[0] === '--') j++;
    if (!parts[j] || !parts[j+1] || word(parts[j][0]) !== '.env.example' || word(parts[j+1][0]) !== '.env') continue;
    if (parts[j+2] && !['&&', '||', ';', '\n'].includes(parts[j+2][0])) continue;
    try {
      const ws=await Workspace.open(root), template=await ws.path('.env.example');
      if (!(await stat(template)).isFile() || !safeTaskPath(relative(ws.root,template))) continue;
      try { const existing=await lstat(resolve(ws.root,'.env')); if (!preserve || !existing.isFile()) continue; }
      catch(error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') continue; }
      ranges.push([parts[i].index!,parts[j+1].index!+parts[j+1][0].length, "true"]);
    } catch { /* A missing or unsafe template retains normal review. */ }
  }
  // File metadata is not file content. Listing/statting the worker's .env
  // must not inherit the approval rule for reading its secret values.
  let metadata = false;
  for (const [index, part] of parts.entries()) {
    if (['&&', '||', ';', '|', '\n'].includes(part[0])) { metadata = false; continue; }
    const previous = parts[index-1];
    const commandStart = !previous || ['&&', '||', ';', '|', '\n'].includes(previous[0]);
    if (commandStart) metadata = ['ls', 'stat', 'test', '[', '/bin/ls', '/usr/bin/ls', '/bin/stat', '/usr/bin/stat', '/bin/test', '/usr/bin/test', '/bin/[', '/usr/bin/['].includes(word(part[0]));
    const value = word(part[0]);
    if (ranges.some(([start,end])=>part.index!>=start && part.index!+part[0].length<=end)) continue;
    if (!commandStart && !value.includes('=') && /(?:^|\/)\.env\.example$/.test(value)) {
      try {
        const ws=await Workspace.open(root),path=await ws.path(value,true);
        const rel=relative(ws.root,path);
        if (safeTaskPath(rel) && /(?:^|\/)\.env\.example$/.test(rel)) { ranges.push([part.index!,part.index!+part[0].length,'worker-env-template']); continue; }
      } catch { /* Templates in other folders keep ordinary review. */ }
    }
    if (commandStart || !metadata || value.startsWith('-')) continue;
    try {
      const ws = await Workspace.open(root);
      await ws.path(value, true);
      ranges.push([part.index!,part.index!+part[0].length, "worker-local-metadata"]);
    } catch { /* Outside metadata remains a separate operation to review. */ }
  }
  ranges.sort((a,b)=>a[0]-b[0]);
  let assessed = command;
  for (const [start, end, replacement] of ranges.reverse()) assessed = assessed.slice(0, start) + replacement + assessed.slice(end);
  return assessed.trim() || undefined;
}
