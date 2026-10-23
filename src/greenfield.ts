import { createHash, randomBytes, randomUUID } from "node:crypto";
import { copyFile, mkdir, readFile, readdir, lstat, realpath, rename, rm, stat, utimes, writeFile } from "node:fs/promises";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { ServiceError } from "./transport.ts";
import { TeamExecutionReview } from "./team-steering.ts";
import { prepareQARepair } from "./team-qa.ts";
import { recoverBudgetTask, recoveryChild } from "./team-recovery.ts";
import type { Recovery, WorkerAction } from "./team-recovery.ts";
import { flexibleChild, runFlexibleWave } from "./team-flexible.ts";
import { applyExecution, type ExecutionWorkspace } from "./team-workspace.ts";
import { inputFingerprint } from "./team-files.ts";
import { Agent, type AgentHooks } from "./agent.ts";
import { PermissionGuard } from "./permission.ts";
import { SessionLog, SteeringQueue } from "./session.ts";
import { ToolRegistry, Workspace, createTools } from "./tools.ts";
import type { AgentSettings, ToolResult } from "./types.ts";
import type { GatewayTransport } from "./transport.ts";

export const teamPack = JSON.parse(await readFile(new URL("../greenfield/pack.json", import.meta.url), "utf8"));
export const environmentCommands = ["php --version", "composer --version", "node --version", "npm --version"];
export const teamInstructions: string = teamPack.instructions.join("\n\n");
export function teamWorkerStepLimit(configured: number): number { return configured > 0 ? Math.min(configured, teamPack.worker_step_limit) : teamPack.worker_step_limit; }
export interface Artifact { kind: string; id: string; path: string; hash: string; summary: string; revision: number }
export interface TaskRun { quota_window?:string; quota_reset?:string; qa_test_files?:string[]; qa_repair_rounds?:number; reviewed_source_hash?:string; repair_from?:string; recovery_trail?: Recovery[]; recovery?: Recovery; recent_actions?: WorkerAction[]; stop_reason?: string; steps_used?: number; step_limit?: number; usage?: { input_tokens: number; cached_input_tokens: number; output_tokens: number; reasoning_tokens: number; calls: number }; duration_ms?: number; branch?: string; workspace?: string; commit?: string; merged?: boolean; id?: string; archive?: string; status: string; attempts: number; output: string; bootstrap?: { command: string; passed: boolean; output: string; log?: string }; checks: Array<{ final?: boolean; command: string; passed: boolean; output: string; log?: string }> }
export interface TeamState {
  execution_mode: "flexible" | "scoped"; execution?: ExecutionWorkspace;
  schema_version: number; revision: number; artifacts: Artifact[];
  approvals: Record<string, string>; runs: Record<string, TaskRun>; references: Record<string, string>;
  files: Record<string, string>;
}
export interface TeamStatus extends TeamState { stage: string; gates: Record<string, string>; errors: string[]; ready: string[]; kinds: Record<string, string[]> }
const arrayKeys = new Set(["users", "workflows", "scope", "exclusions", "open_questions", "screens", "components", "states", "entities", "authorization", "operations", "acceptance", "edge_cases", "depends_on", "requirements", "read", "write", "checks", "sources", "unknowns", "findings", "blockers"]);
const slug = /^[a-z][a-z0-9-]{0,79}$/;
const digest = (s: string | Uint8Array): string => createHash("sha256").update(s).digest("hex");
const emptyState = (): TeamState => ({ execution_mode: "flexible", schema_version: 1, revision: 0, artifacts: [], approvals: {}, runs: {}, references: {}, files: {} });
export function safeTaskPath(path: string): boolean {
  return !!path && !isAbsolute(path) && !/[\\:*?"<>|]/.test(path) && !path.split("/").some(segment => { const p = segment.toLowerCase(); return !p || p === "." || p === ".." || p.endsWith(".") || segment.trim() !== segment || p === ".maxlabs" || p === ".git" || p === ".env" || (p.startsWith(".env.") && p !== ".env.example"); });
}
export function allowedCheck(command: string): boolean {
  return !/[\n\r;&|<>`$(){}\\]/.test(command) && !command.includes("..") && !/(?:^|\s)(?:--env|--configuration|-c|--help|--version|--list-tests|--listTests|--passWithNoTests|--dry-run)(?:=|\s|$)/.test(command)
    && teamPack.check_prefixes.some((p: string) => command === p || command.startsWith(p + " "));
}
export class TeamStore {
  #saveQueue: Promise<unknown> = Promise.resolve();
  readonly workspace: Workspace;
  constructor(workspace: Workspace) { this.workspace = workspace; }
  async path(path: string, missing = false): Promise<string> { return this.workspace.path(`.maxlabs/team/${path}`, missing); }
  async load(): Promise<TeamState> {
    try {
      const s = JSON.parse(await readFile(await this.path("state.json"), "utf8"));
      if (s.schema_version !== 1 || !Array.isArray(s.artifacts) || !s.approvals || !s.runs || !s.references) throw new Error("Unsupported or invalid team state");
      s.files ||= {}; s.execution_mode ||= "scoped";
      return s;
    } catch (e) { if ((e as NodeJS.ErrnoException).code === "ENOENT") return emptyState(); throw e; }
  }
  async persist(s: TeamState): Promise<void> {
    const path = await this.path("state.json", true);
    const temporary = `${path}.${randomUUID()}.tmp`;
    await writeFile(temporary, JSON.stringify(s, null, 2) + "\n", { mode: 0o600 });
    await rename(temporary, path);
  }
  async locked<T>(fn: (s: TeamState) => Promise<T>): Promise<T> {
    const directory = await this.path("", true);
    await mkdir(directory, { recursive: true });
    const lock = await this.path(".lock", true);
    try { await mkdir(lock); }
    catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
      const age = Date.now() - (await stat(lock)).mtimeMs;
      if (age < 120_000) throw new Error("Another team operation is active. Retry when it finishes.");
      const recovery = `${lock}.recovery`;
      await mkdir(recovery);
      try {
        if (Date.now() - (await stat(lock)).mtimeMs < 120_000) throw new Error("Another team operation recovered the lease");
        await rm(lock, { recursive: true }); await mkdir(lock);
      } finally { await rm(recovery, { recursive: true, force: true }); }
    }
    const heartbeat = setInterval(() => { const now = new Date(); void utimes(lock, now, now).catch(() => {}); }, 10_000);
    try {
      const s = await this.load();
      for (const run of Object.values(s.runs)) if (["running", "checking"].includes(run.status)) { run.status = "interrupted"; run.output = "Previous execution was interrupted; inspect changes before retrying."; }
      return await fn(s);
    } finally { clearInterval(heartbeat); await rm(lock, { recursive: true, force: true }); }
  }
  async content(a: Artifact): Promise<Record<string, any>> {
    if (!a.path.startsWith("artifacts/") || !safeTaskPath(a.path)) throw new Error("Invalid artifact path");
    const raw = await readFile(await this.path(a.path), "utf8");
    if (digest(raw) !== a.hash) throw new Error(`Artifact ${a.id} changed outside the workflow; save a new revision.`);
    return JSON.parse(raw);
  }
  async fileHash(path: string): Promise<string> { return inputFingerprint(path, p => this.workerPath(p)); }
  async workerPath(path: string): Promise<string> {
    const candidate = await this.workspace.path(path, true);
    const metadata = await this.path("", true);
    if (!safeTaskPath(relative(this.workspace.root, candidate)) || candidate === metadata || candidate.startsWith(metadata + sep)) throw new Error("Worker paths cannot target workflow state or protected files");
    return candidate;
  }
  async preview(): Promise<string> {
    const s = await this.load(); const a = s.artifacts.find(a => a.kind === "design");
    if (!a) throw new Error("No design has been saved yet");
    const d = await this.content(a);
    const policy = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; font-src data:; form-action 'none'; base-uri 'none'">`;
    const html = /<head[^>]*>/i.test(d.prototype_html) ? d.prototype_html.replace(/<head[^>]*>/i, (m: string) => m + policy) : policy + d.prototype_html;
    const path = await this.path(`preview-${a.revision}.html`, true); await writeFile(path, html, { mode: 0o600 }); return path;
  }
  async bootstrap(version: string, execute: (command: string, signal?: AbortSignal) => Promise<ToolResult>, signal?: AbortSignal): Promise<ToolResult> {
    if (!/^\d+(?:\.\d+)?\.\*$/.test(version)) return { text: "Bootstrap requires a confirmed Laravel version, e.g. 13.*", isError: true };
    const skeletonReady = async (root: string): Promise<boolean> => {
      for (const file of ["composer.json", "artisan", "bootstrap/app.php"]) { try { if (!(await stat(await this.workspace.path(relative(this.workspace.root, resolve(root, file))))).isFile()) return false; } catch { return false; } }
      return true;
    };
    let owned = false;
    try {
      const marker = JSON.parse(await readFile(await this.path("bootstrap.json"), "utf8"));
      if (marker.version !== version) return { text: "Bootstrap version differs from the existing workflow skeleton; revise the plan rather than replacing it", isError: true };
      owned = true;
      if (marker.status !== "copying" && await skeletonReady(this.workspace.root)) return { text: "Laravel skeleton already created and verified by this workflow" };
    } catch (e) { if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e; }
    if (!owned) { try { await lstat(resolve(this.workspace.root, "composer.json")); return { text: "Bootstrap will not replace an existing application with composer.json", isError: true }; } catch (e) { if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e; } }
    const staging = `.maxlabs/team/scaffold-${randomUUID()}`;
    const result = await execute(`composer create-project laravel/laravel "${staging}" "${version}" --no-install --no-interaction --no-scripts`, signal);
    if (result.isError) return result;
    const source = await this.workspace.path(staging);
    try {
    if (!await skeletonReady(source)) return { text: "Composer did not produce a complete Laravel skeleton (composer.json, artisan, bootstrap/app.php)", isError: true };
    await writeFile(await this.path("bootstrap.json", true), JSON.stringify({ version, status: "copying" }), { mode: 0o600 });
    const copy = async (from: string, to: string): Promise<void> => {
      for (const entry of await readdir(from, { withFileTypes: true })) {
        if (entry.isSymbolicLink() || [".git", ".maxlabs"].includes(entry.name) || (entry.name !== ".env.example" && (entry.name === ".env" || entry.name.startsWith(".env.")))) continue;
        const destination = await this.workspace.path(relative(this.workspace.root, resolve(to, entry.name)), true);
        if (entry.isDirectory()) { await mkdir(destination, { recursive: true }); await copy(resolve(from, entry.name), destination); }
        else {
          try { const existing = await lstat(destination); if (!existing.isFile()) throw new Error(`Bootstrap destination ${entry.name} is not a regular file`); continue; }
          catch (e) { if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e; }
          await copyFile(resolve(from, entry.name), destination, 1);
        }
      }
    };
    await copy(source, this.workspace.root);
    try {
      const example = await readFile(await this.workspace.path(".env.example"), "utf8");
      const environment = example.replace(/^APP_KEY=.*$/m, `APP_KEY=base64:${randomBytes(32).toString("base64")}`);
      await writeFile(await this.workspace.path(".env", true), environment, { mode: 0o600, flag: "wx" });
    } catch (e) { if (!["ENOENT", "EEXIST"].includes((e as NodeJS.ErrnoException).code || "")) throw e; }
    try { await writeFile(await this.workspace.path("database/database.sqlite", true), "", { mode: 0o600, flag: "wx" }); }
    catch (e) { if (!["ENOENT", "EEXIST"].includes((e as NodeJS.ErrnoException).code || "")) throw e; }
    await writeFile(await this.path("bootstrap.json", true), JSON.stringify({ version, status: "complete" }), { mode: 0o600 });
    return { text: `Created official Laravel ${version} skeleton, preserving existing files. Dependencies are installed by approved task checks.` };
    } finally { await rm(source, { recursive: true, force: true }); }
  }
  async save(kind: string, id: string, content: Record<string, any>): Promise<TeamStatus> {
    const result = this.#saveQueue.then(() => this.#save(kind, id, content));
    this.#saveQueue = result.catch(() => {});
    return result;
  }
  async #save(kind: string, id: string, content: Record<string, any>): Promise<TeamStatus> {
    return this.locked(async s => {
      const flex = s.execution_mode === "flexible";
      const required: string[] | undefined = (flex ? teamPack.flexible_kinds : teamPack.kinds)[kind];
      if (typeof kind !== "string" || typeof id !== "string" || !Array.isArray(required) || !slug.test(id)) throw new Error("Unknown artifact kind or invalid lowercase slug ID");
      if (!content || Array.isArray(content) || typeof content !== "object") throw new Error("content must be a JSON object");
      if (JSON.stringify(content).length > 200_000) throw new Error("Artifact exceeds 200 KB; split it into smaller contracts");
      if (s.artifacts.length >= 128 && !s.artifacts.some(a => a.kind === kind && a.id === id)) throw new Error("Beta supports 128 artifacts per project");
      if (kind === "task" && s.artifacts.filter(a => a.kind === "task" && a.id !== id).length >= 64) throw new Error("Split the release into at most 64 tasks");
      if (flex) {
        content = structuredClone(content);
        for (const key of teamPack.kinds[kind] || []) if (arrayKeys.has(key)) content[key] ??= [];
        if (kind === "task") content.role ||= "implementation";
        if (kind === "spec") { content.stack ||= {}; content.acceptance = content.acceptance.map((v: any, i: number) => typeof v === "string" ? { id: `outcome-${i+1}`, expect: v } : v); }
        for (const key of arrayKeys) if (key in content && !Array.isArray(content[key])) throw new Error(`${key} must be an array`);
      }
      for (const key of required) {
        if (!(key in content)) throw new Error(`${kind}.${key} is required`);
        if (arrayKeys.has(key) && !Array.isArray(content[key])) throw new Error(`${key} must be an array`);
      }
      if (typeof content.summary !== "string" || !content.summary.trim()) throw new Error("summary must be nonempty");
      if (kind === "task" && flex) {
        if (content.serial !== undefined && typeof content.serial !== "boolean") throw new Error("serial must be a boolean");
        if (!["foundation", "implementation", "qa"].includes(content.role) || !content.acceptance.length) throw new Error("Task needs a role and observable acceptance outcomes");
        if (content.depends_on.some((v: unknown) => typeof v !== "string" || !v)) throw new Error("Dependencies must contain task IDs");
      }
      if (kind === "task" && !flex) {
        if (content.bootstrap !== undefined && (content.role !== "foundation" || typeof content.bootstrap !== "string" || !/^\d+(?:\.\d+)?\.\*$/.test(content.bootstrap))) throw new Error("Only the foundation task may bootstrap a confirmed Laravel version");
        if (!["foundation", "implementation", "qa"].includes(content.role)) throw new Error("Invalid task role");
        if (!content.requirements.length || !content.acceptance.length || !content.checks.length) throw new Error("Tasks require requirements, acceptance, and checks");
        for (const key of ["read", "write", "requirements", "depends_on", "checks"]) if (content[key].some((v: unknown) => typeof v !== "string" || !v.trim())) throw new Error(`${key} must contain strings`);
        for (const p of [...content.read, ...content.write]) if (!safeTaskPath(p)) throw new Error("Task paths must be exact workspace-relative files outside .maxlabs, .git and credentials");
        for (const c of content.checks) if (typeof c !== "string" || !allowedCheck(c)) throw new Error(`Unsupported verification command: ${c}`);
        if (content.role === "qa" && content.write.length) throw new Error("QA tasks are read-only");
      }
      if (kind === "design" && (typeof content.prototype_html !== "string" || !content.prototype_html.trim())) throw new Error("Design requires prototype_html");
      if (!flex && kind === "integration" && !["documented", "schema_validated", "sandbox_tested", "unknown", "conflicting"].includes(content.evidence_status)) throw new Error("Invalid evidence_status");
      s.revision++;
      const path = `artifacts/${kind}-${id}-${s.revision}.json`;
      const raw = JSON.stringify(content, null, 2) + "\n";
      await mkdir(await this.path("artifacts", true), { recursive: true });
      await writeFile(await this.path(path, true), raw, { mode: 0o600, flag: "wx" });
      s.artifacts = s.artifacts.filter(a => !(a.kind === kind && a.id === id));
      s.artifacts.push({ kind, id, path, hash: digest(raw), summary: content.summary, revision: s.revision });
      if (["brief", "design", "spec", "task", "integration", "review"].includes(kind)) {
        for (const run of Object.values(s.runs)) run.status = "stale";
      }
      await this.persist(s);
      return this.status(s);
    });
  }
  async status(s?: TeamState): Promise<TeamStatus> {
    if (!s) {
      s = await this.load();
      let active = false;
      try { active = Date.now() - (await stat(await this.path(".lock"))).mtimeMs < 120_000; } catch (e) { if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e; }
      if (!active) for (const run of Object.values(s.runs)) if (["running", "checking"].includes(run.status)) { run.status = "interrupted"; run.output = "Previous execution was interrupted; inspect changes before retrying."; }
    }
    const flex = s.execution_mode === "flexible";
    const docs = new Map<string, Record<string, any>>();
    const errors: string[] = [];
    for (const a of s.artifacts) { try { docs.set(`${a.kind}:${a.id}`, await this.content(a)); } catch (e) { errors.push(String((e as Error).message)); } }
    const select = (kind: string): Artifact[] => s!.artifacts.filter(a => a.kind === kind);
    const gate = (kinds: string[], execution = false): string => digest(s!.artifacts.filter(a => kinds.includes(a.kind)).map(a => `${a.kind}:${a.id}:${a.hash}`).sort().join("\n") + "\nreferences:" + Object.entries(s!.references).sort().map(([k,v]) => `${k}:${v}`).join("\n") + (flex && execution ? "\nexecution:flexible-v1" : ""));
    const gates = { design: gate(["brief", "design"]), spec: gate(["brief", "design", "spec", "task", "integration", "review"], true), release: gate(Object.keys(teamPack.kinds).filter(k => k !== "evidence"), true) };
    const approved = (g: string): boolean => s!.approvals[g] === gates[g];
    const specs = select("spec");
    const requirements = new Set<string>();
    for (const a of specs) {
      const spec = docs.get(`spec:${a.id}`);
      if (!spec) continue;
      if (spec.open_questions.length) errors.push("Spec has unresolved questions");
      if (!flex && spec.stack?.framework !== "laravel") errors.push("This beta requires stack.framework: laravel");
      for (const key of (flex ? ["acceptance"] : ["entities", "authorization", "operations", "acceptance"])) if (!spec[key].length) errors.push(`Spec ${key} must be defined`);
      for (const ac of spec.acceptance) {
        if (!ac || typeof ac.id !== "string" || !ac.id || typeof ac.expect !== "string" || !ac.expect.trim() || requirements.has(ac.id)) errors.push("Spec acceptance needs unique IDs and concrete expect statements");
        else requirements.add(ac.id);
      }
    }
    for (const a of select("brief")) if (docs.get(`brief:${a.id}`)?.open_questions.length) errors.push("Brief has unresolved questions");
    for (const a of select("integration")) { const d = docs.get(`integration:${a.id}`); if (d && (d.unknowns.length || ["unknown", "conflicting"].includes(d.evidence_status))) errors.push(`Integration ${a.id} has unresolved evidence`); }
    for (const a of select("review")) if (docs.get(`review:${a.id}`)?.blockers.length) errors.push("Review has blocking findings");
    const tasks = select("task");
    const taskMap = new Map(tasks.map(a => [a.id, docs.get(`task:${a.id}`)]));
    const foundations = tasks.filter(a => taskMap.get(a.id)?.role === "foundation");
    const qa = tasks.filter(a => taskMap.get(a.id)?.role === "qa");
    if (specs.length !== 1) errors.push("Exactly one spec is required");
    if (select("brief").length !== 1 || select("design").length !== 1) errors.push("Exactly one brief and design are required");
    if ((!flex && foundations.length !== 1) || !qa.length) errors.push(flex ? "Plan needs at least one independent QA task" : "Plan needs exactly one foundation task and at least one independent QA task");
    if (!select("review").length) errors.push("A review artifact is required");
    const memo = new Map<string, Set<string>>();
    const ancestors = (id: string, visiting = new Set<string>()): Set<string> => {
      if (visiting.has(id)) { errors.push(`Dependency cycle at ${id}`); return new Set(); }
      if (memo.has(id)) return memo.get(id)!;
      visiting.add(id);
      const out = new Set<string>();
      for (const dep of taskMap.get(id)?.depends_on || []) {
        if (typeof dep !== "string" || !taskMap.has(dep)) { errors.push(`Missing dependency ${dep}`); continue; }
        out.add(dep); for (const p of ancestors(dep, new Set(visiting))) out.add(p);
      }
      memo.set(id, out); return out;
    };
    const covered = new Set<string>();
    const qaCovered = new Set<string>();
    const ready: string[] = [];
    for (const a of tasks) {
      const task = taskMap.get(a.id); if (!task) continue;
      for (const path of (flex ? [] : task.write)) {
        try { if ((await stat(await this.workerPath(path))).isDirectory()) errors.push(`${a.id}: write path ${path} is a directory; list exact writable files`); }
        catch (e) { if ((e as NodeJS.ErrnoException).code !== "ENOENT") errors.push(`${a.id}: ${String((e as Error).message)}`); }
      }
      if (!flex && task.role === "foundation") {
        if (!task.bootstrap && task.checks.some((c: string) => c.startsWith("composer install"))) {
          try { await stat(resolve(this.workspace.root, "composer.json")); } catch (e) { if ((e as NodeJS.ErrnoException).code === "ENOENT") errors.push(`${a.id}: missing composer.json; set the foundation task bootstrap field to the confirmed Laravel version`); else throw e; }
        }
        for (const aSpec of specs) { const major = docs.get(`spec:${aSpec.id}`)?.stack?.major; if (task.bootstrap && major && task.bootstrap.split(".")[0] !== String(major)) errors.push(`${a.id}: bootstrap version conflicts with spec.stack.major`); }
      }
      const deps = ancestors(a.id);
      if (!flex && task.role !== "foundation" && foundations.length === 1 && !deps.has(foundations[0].id)) errors.push(`${a.id} must depend on foundation`);
      if (!flex && task.role === "foundation" && task.depends_on.length) errors.push("Foundation cannot have dependencies");
      if (task.role === "qa") for (const t of tasks) if ((taskMap.get(t.id)?.role === "implementation" || (flex && taskMap.get(t.id)?.role === "foundation")) && !deps.has(t.id)) errors.push(`${a.id} must follow implementation ${t.id}`);
      for (const r of (flex ? [] : task.requirements)) { if (!requirements.has(r)) errors.push(`Unknown requirement ${r}`); else (task.role === "qa" ? qaCovered : covered).add(r); }
      if (approved("design") && approved("spec") && !["passed", "running"].includes(s.runs[a.id]?.status) && task.depends_on.every((id: string) => s!.runs[id]?.status === "passed") && ((s.runs[a.id]?.attempts || 0) < 3 || s.runs[a.id]?.status === "paused")) ready.push(a.id);
    }
    for (const r of (flex ? [] : requirements)) if (!covered.has(r)) errors.push(`Uncovered requirement ${r}`);
    for (const r of (flex ? [] : requirements)) if (!qaCovered.has(r)) errors.push(`Requirement missing from independent QA: ${r}`);
    let stage = !select("brief").length ? "discovery" : !select("design").length ? "design" : !approved("design") ? "awaiting_design_approval" : !specs.length ? "specification" : !approved("spec") ? "awaiting_spec_approval" : !foundations.every(a => s!.runs[a.id]?.status === "passed") ? "foundation" : !tasks.every(a => s!.runs[a.id]?.status === "passed") ? "implementation" : !select("release").length ? "delivery" : approved("release") ? "complete" : "awaiting_release_approval";
    if (errors.length && approved("spec")) stage = "blocked";
    return { ...s, stage, gates, errors: [...new Set(errors)].sort(), ready: errors.length ? [] : ready, kinds: flex ? teamPack.flexible_kinds : teamPack.kinds };
  }
  async approve(gate: string, expected: string): Promise<TeamStatus> {
    return this.locked(async s => {
      const status = await this.status(s);
      if (!["design", "spec", "release"].includes(gate) || expected !== status.gates[gate]) throw new Error("Approval is stale; review the current revision first");
      if (gate === "design" && (!s.artifacts.some(a => a.kind === "brief") || !s.artifacts.some(a => a.kind === "design"))) throw new Error("Brief and design are required");
      if (gate === "design" && (await this.content(s.artifacts.find(a => a.kind === "brief")!)).open_questions.length) throw new Error("Resolve brief open questions before design approval");
      if (gate !== "design" && (status.errors.length || s.approvals.design !== status.gates.design)) throw new Error("Resolve readiness errors and approve design first: " + status.errors.join("; "));
      if (gate === "release" && (s.approvals.spec !== status.gates.spec || !s.artifacts.some(a => a.kind === "release") || s.artifacts.filter(a => a.kind === "task").some(a => s.runs[a.id]?.status !== "passed"))) throw new Error("Release requires approved spec, passing tasks and release artifact");
      if (gate === "spec") {
        s.files = {};
        if (s.execution_mode !== "flexible") for (const a of s.artifacts.filter(a => a.kind === "task")) { const task = await this.content(a); for (const path of [...task.read, ...task.write]) s.files[path] = await this.fileHash(path); }
        if (s.approvals.spec !== expected) s.runs = {};
        else for (const run of Object.values(s.runs)) if (run.status !== "passed") { run.attempts = 0; run.qa_repair_rounds=0; delete run.recovery; }
      }
      if (gate === "release" && s.execution_mode === "flexible") await applyExecution(this, s);
      s.approvals[gate] = expected; s.revision++; await this.persist(s); return this.status(s);
    });
  }
  async setExecutionMode(mode: string): Promise<TeamStatus> {
    if (!["flexible", "scoped"].includes(mode)) throw new Error("Choose flexible or scoped execution");
    return this.locked(async s => {
      if (s.execution_mode !== mode) {
        s.execution_mode = mode as TeamState["execution_mode"]; delete s.approvals.spec; delete s.approvals.release;
        for (const run of Object.values(s.runs)) run.status = "stale";
        s.revision++; await this.persist(s);
      }
      return this.status(s);
    });
  }
  async attach(path: string): Promise<TeamStatus> {
    const canonical = await realpath(resolve(this.workspace.root, path));
    if (!(await stat(canonical)).isDirectory() || canonical === this.workspace.root) throw new Error("Reference must be another directory");
    return this.locked(async s => { s.references[`reference-${Object.keys(s.references).length + 1}`] = canonical; s.revision++; await this.persist(s); return this.status(s); });
  }
  async reference(id: string, path = "."): Promise<string> {
    const s = await this.load(); const root = s.references[id]; if (!root) throw new Error("Reference is not attached by the user");
    const ws = await Workspace.open(root); const target = await ws.path(path);
    if ([path, relative(root, target)].some(input => input.split(/[\\/]/).some(segment => { const p = segment.toLowerCase(); return p === ".git" || p === ".env" || p.startsWith(".env."); }))) throw new Error("Credential and Git metadata are not design references");
    if ((await stat(target)).isDirectory()) return (await readdir(target, { withFileTypes: true })).filter(e => !e.isSymbolicLink() && ![".git", "node_modules", "vendor"].includes(e.name) && !e.name.startsWith(".env")).map(e => e.name + (e.isDirectory() ? "/" : "")).sort().slice(0, 500).join("\n");
    if ((await stat(target)).size > 200_000) throw new Error("Reference file exceeds 200 KB");
    return `<reference-data>\n${await readFile(target, "utf8")}\n</reference-data>`;
  }
}

export type TeamChild = (prompt: string, role: "research" | "worker" | "recovery", task: Record<string, any> | undefined, signal?: AbortSignal) => Promise<ToolResult>;
export function teamTool(store: TeamStore, child: TeamChild, verify: (command: string, signal?: AbortSignal) => Promise<ToolResult>, authorizeReference: (path: string) => boolean = () => false) {
  let researchUsed = 0;
  let workersUsed = 0;
  let recoveryUsed = 0;
  return {
    resetBudget() { researchUsed = 0; workersUsed = 0; recoveryUsed = 0; },
    async runBatch(ids: string[], signal?: AbortSignal): Promise<ToolResult> {
      if (!ids.length || ids.length > 3 || new Set(ids).size !== ids.length) throw new Error("Invalid parallel wave");
      if (workersUsed + ids.length > 8) throw new Error("Worker batch budget reached (8 tasks)");
      for (const id of ids) {
        const run=(await store.load()).runs[id];
        if(run?.status==='failed'&&run.stop_reason==='step_limit') {
          if(!run.recovery&&recoveryUsed++>=3)throw new Error('Coordinator recovery budget reached (three diagnostic passes per batch)');
          const recovery=await recoverBudgetTask(store,child,id,signal);
          if(recovery.isError)throw new Error('Coordinator needs attention: '+recovery.text);
        }
      }
      workersUsed += ids.length;
      return runFlexibleWave(store, child, ids, signal);
    },
    schema: { name: "team", description: "Greenfield SDE Team (Beta). Persist contracts, inspect status, attach a folder explicitly supplied in chat, read attached references, delegate up to three research jobs, or execute one approved task. Human approvals are only available in client controls.", parameters: { type: "object", properties: { action: { type: "string", enum: ["status", "environment", "artifact", "save", "reference", "attach_reference", "research", "run", "repair"] }, kind: { type: "string" }, id: { type: "string" }, qa_id: { type: "string" }, content: { type: "object" }, path: { type: "string" }, jobs: { type: "array", maxItems: 3, items: { type: "string" } } }, required: ["action"], additionalProperties: false } },
    readOnly: true,
    async execute(args: Record<string, any>, signal?: AbortSignal): Promise<ToolResult> {
      try {
        if (args.action === "environment") {
          const observed = [];
          for (const command of environmentCommands) { const result = await verify(command, signal); observed.push({ command, available: !result.isError, output: result.text.slice(0, 1500) }); }
          return { text: JSON.stringify({ observed, note: "Observed local tools; verify framework support in official docs. These version probes do not establish the latest Laravel release." }) };
        }
        if (args.action === "status") {
          const { files, runs, execution, ...status } = await store.status();
          return { text: JSON.stringify({ ...status, execution: execution ? { path: execution.path, branch: execution.branch, commit: execution.commit, applied_commit: execution.applied_commit } : undefined, artifacts: status.artifacts.map(a => ({ ...a, summary: a.summary.slice(0, 160) })), artifact_root: ".maxlabs/team", runs: Object.fromEntries(Object.entries(runs).map(([id, run]) => [id, { status: run.status, attempts: run.attempts, summary: run.output.slice(0, 600), stop_reason:run.stop_reason,qa_test_files:run.qa_test_files,qa_repair_rounds:run.qa_repair_rounds, checks: run.checks.map(({ command, passed, final, log }) => ({ command, passed, final, log })) }])) }) };
        }
        if (args.action === "artifact") {
          const state = await store.load(); const artifact = state.artifacts.find(a => a.kind === args.kind && a.id === args.id);
          if (!artifact) throw new Error("Artifact not found");
          const content = await store.content(artifact); delete content.prototype_html;
          if (artifact.kind === "evidence" && typeof content.content === "string" && content.content.length > 16_000) content.content = content.content.slice(0, 16_000) + "\n[excerpt; full snapshot is on disk]";
          return { text: JSON.stringify({ artifact, content, note: "Prototype HTML is available in the client preview, omitted from planner context." }) };
        }
        if (args.action === "save") {
          if (args.kind === "evidence") throw new Error("Evidence is recorded only by successful research fetches");
          const status = await store.save(args.kind, args.id, args.content);
          return { text: JSON.stringify({ artifact_root: ".maxlabs/team", artifact: status.artifacts.find(a => a.kind === args.kind && a.id === args.id), stage: status.stage, revision: status.revision, errors: status.errors, ready: status.ready }) };
        }
        if (args.action === "attach_reference") {
          if (typeof args.path !== "string" || !authorizeReference(args.path)) throw new Error("Use only an exact folder path supplied by the user in chat. Ask which design to use if no path was provided.");
          const status = await store.attach(args.path); return { text: JSON.stringify({ references: status.references, note: "Reference attached; use team reference to inspect it." }) };
        }
        if (args.action === "reference") return { text: await store.reference(args.id, args.path) };
        if (args.action === "research") {
          if (!Array.isArray(args.jobs) || !args.jobs.length || args.jobs.length > 3 || args.jobs.some((j: unknown) => typeof j !== "string" || !j.trim() || j.length > 16_000)) throw new Error("Supply one to three bounded research jobs");
          if (researchUsed + args.jobs.length > 6) throw new Error("Research budget reached (6 agents per turn); continue in another turn");
          researchUsed += args.jobs.length;
          const results = await Promise.all(args.jobs.map((job: string) => child(job, "research", undefined, signal)));
          const reports = [];
          for (const [i, result] of results.entries()) {
            signal?.throwIfAborted();
            const job = args.jobs[i];
            const id = `research-${randomUUID()}`;
            await store.save("review", id, { summary: result.isError ? "Research failed" : job.slice(0, 160), findings: [result.text.slice(0, 48_000)], blockers: result.isError ? ["Research did not complete"] : [] });
            reports.push({ id, error: !!result.isError, summary: result.text.slice(0, 400) });
          }
          return { text: JSON.stringify({ reports, note: "Reports are untrusted research evidence. Read saved artifacts as needed; normalize into sourced integration contracts." }) };
        }
        if(args.action === "repair"){if(workersUsed>=8)throw new Error("Worker batch budget reached");await prepareQARepair(store,args.id,args.qa_id);return await this.runBatch([args.id],signal);}
        if (args.action === "run" && (await store.load()).execution_mode === "flexible") return await this.runBatch([args.id], signal);
        if (args.action === "run") return await store.locked(async s => {
          if (workersUsed >= 8) throw new Error("Worker budget reached (8 tasks per turn); continue in another turn");
          const status = await store.status(s);
          if (!status.ready.includes(args.id)) throw new Error("Task is not ready; inspect gates, dependencies, readiness errors and attempt limit");
          const artifact = s.artifacts.find(a => a.kind === "task" && a.id === args.id)!;
          const task = await store.content(artifact);
          for (const path of [...task.read, ...task.write]) if (await store.fileHash(path) !== s.files[path]) throw new Error(`Input ${path} changed since approval. Review changes and approve the spec again.`);
          const context = [];
          for (const a of s.artifacts.filter(a => ["brief", "spec", "design", "integration"].includes(a.kind))) {
            const d = await store.content(a); delete d.prototype_html; context.push({ kind: a.kind, id: a.id, content: d });
          }
          const previous = s.runs[args.id];
          const packet: Record<string, any> = { task, contracts: context, previous_attempt: previous ? { status: previous.status, summary: previous.output.slice(0, 1000), checks: previous.checks.map(c => ({ ...c, output: c.output.slice(0, 2200) })) } : undefined };
          if (JSON.stringify(packet).length > 96_000) throw new Error("Worker context exceeds the beta budget; reduce contracts or release scope");
          workersUsed++;
          const run: TaskRun = { id: randomUUID(), status: "running", attempts: (s.runs[args.id]?.attempts || 0) + 1, output: "", checks: [] };
          s.runs[args.id] = run; delete s.approvals.release; await store.persist(s);
          try {
            signal?.throwIfAborted();
            if (task.bootstrap) {
              const setup = await store.bootstrap(task.bootstrap, verify, signal);
              const log = `.maxlabs/team/checks/${args.id}-${run.id}-${run.attempts}-0.log`;
              await mkdir(await store.path("checks", true), { recursive: true }); await writeFile(await store.path(log.slice(".maxlabs/team/".length), true), setup.text, { mode: 0o600 });
              run.bootstrap = { command: "runner: Laravel bootstrap " + task.bootstrap, passed: !setup.isError, output: setup.text, log }; packet.bootstrap_evidence = run.bootstrap;
              if (setup.isError) throw new Error(setup.text);
              for (const path of Object.keys(s.files)) s.files[path] = await store.fileHash(path);
            }
            const executeCheck = async (command: string) => {
              signal?.throwIfAborted(); const check = await verify(command, signal);
              const log = `.maxlabs/team/checks/${args.id}-${run.id}-${run.attempts}-${run.checks.length+1}.log`; const logPath = await store.path(log.slice(".maxlabs/team/".length), true);
              await mkdir(resolve(logPath, ".."), { recursive: true }); await writeFile(logPath, check.text.slice(0, 200_000) + (check.text.length > 200_000 ? "\n[log truncated at 200 KB]" : ""), { mode: 0o600 });
              run.checks.push({ command, passed: !check.isError, output: compactCheckOutput(check.text), log });
            };
            if (task.role === "qa") {
              for (const command of task.checks) await executeCheck(command);
              if (run.checks.some(c => !c.passed)) throw new Error("QA runner checks failed before independent review; see check evidence");
              packet.verification_evidence = run.checks;
              if (JSON.stringify(packet).length > 96000) throw new Error("QA context exceeds beta budget; refine contracts");
            }
            const result = await child(JSON.stringify(packet), "worker", task, signal);
            run.output = result.text.slice(0, 48_000);
            run.status = result.isError ? "failed" : "checking";
            if (!result.isError && task.role !== "qa") for (const command of task.checks) await executeCheck(command);
            run.status = !result.isError && run.checks.length === task.checks.length && run.checks.every(c => c.passed) ? "passed" : "failed";
          } catch (e) { run.status = signal?.aborted ? "interrupted" : "failed"; run.output = String((e as Error).message); }
          for (const path of Object.keys(s.files)) if (task.write.some((output: string) => path === output || output.startsWith(path + "/"))) s.files[path] = await store.fileHash(path);
          run.archive = `.maxlabs/team/runs/${args.id}-${run.id}.json`; const archive = await store.path(run.archive.slice(".maxlabs/team/".length), true);
          await mkdir(resolve(archive, ".."), { recursive: true }); await writeFile(archive, JSON.stringify(run, null, 2), { mode: 0o600 });
          await store.persist(s);
          return { text: JSON.stringify({ id: args.id, status: run.status, attempts: run.attempts, output: run.output.slice(0, 2_000), checks: run.checks.map(c => ({ ...c, output: c.output.slice(0, c.passed ? 500 : 4_000) })) }), isError: run.status !== "passed" };
        });
        throw new Error("Unknown team action");
      } catch (e) { return { text: `team: ${(e as Error).message}`, isError: true }; }
    },
  };
}

export function createTeamChild(store: TeamStore, transport: GatewayTransport, guard: PermissionGuard, settings: AgentSettings, workerModel: () => string = () => "", onTool?: AgentHooks["onTool"], onUsage?: AgentHooks["onUsage"]): TeamChild {
  return async (prompt, role, task, signal) => {
    if(role==='recovery')return recoveryChild(store,transport,guard,settings,prompt,task!,teamPack.recovery_instructions,signal,onTool,onUsage);
    if (role === "worker" && task?._flexible) return flexibleChild(store, transport, guard, settings, workerModel(), prompt, task, signal, onTool, onUsage);
    let fetches = 0;
    const registry = createTools(store.workspace);
    const scoped = new ToolRegistry();
    for (const name of registry.names()) {
      const tool = registry.get(name)!;
      if (role === "research" && name === "webfetch") scoped.register({ ...tool, async execute(args, signal) {
        if (++fetches > 6) return { text: "Research documentation budget reached (6 fetches). Reuse evidence and report remaining unknowns.", isError: true };
        const result = await tool.execute(args, signal);
        if (!result.isError) await store.save("evidence", `source-${randomUUID()}`, { summary: `API source: ${String(args.url).slice(0, 120)}`, source_url: args.url, retrieved_at: new Date().toISOString(), content: result.text, content_hash: digest(result.text) });
        return result;
      } });
      else if (tool.readOnly) scoped.register(tool);
      else if (role === "worker" && task?.role !== "qa" && ["write", "edit"].includes(name)) scoped.register({ ...tool, async execute(args, signal) {
        if (guard.mode === "plan") return { text: "Plan mode refuses worker mutations", isError: true };
        const path = String(args.path || "");
        if (!task?.write.includes(path) || !safeTaskPath(path)) return { text: "Worker write is outside its approved file scope", isError: true };
        try { await store.workerPath(path); } catch (e) { return { text: String((e as Error).message), isError: true }; }
        return tool.execute(args, signal);
      } });
    }
    if (role === "research") scoped.register({ schema: { name: "team_reference", description: "Read an explicitly attached design reference directory or file.", parameters: { type: "object", properties: { id: { type: "string" }, path: { type: "string" } }, required: ["id"] } }, readOnly: true, async execute(args) { try { return { text: await store.reference(String(args.id), String(args.path || ".")) }; } catch (e) { return { text: String(e), isError: true }; } } });
    const childGuard = role === "worker" ? new PermissionGuard("auto", guard.prompt) : guard;
    const child = new Agent(new SessionLog(), transport, scoped, childGuard, { ...settings, model: role === "worker" ? workerModel() || settings.model : settings.model, maxSteps: role === "worker" ? teamWorkerStepLimit(settings.maxSteps) : Math.min(settings.maxSteps, 40) }, { onTool, onUsage, toolResultLimit: () => 16_000 });
    try {
      let result = await child.run(`${role === "research" ? "Research only. Use official sources and cite exact URLs. Report unknowns and evidence; never implement." : 'Implement only the supplied task. Inspect existing code first. Follow approved contracts. No shell or delegation. The runner executes checks. Return ONLY a JSON object as your final answer, without prose or markdown: {"outcome":"completed" or "blocked","summary":"...","blockers":[]}. Report blocked if requirements cannot be met.'}\n\n${task?.role === "qa" ? "Independent QA: the runner has already executed the approved checks and supplied verification_evidence. Review that actual evidence and inspect source/design. Do not block merely because shell tools are unavailable; report genuine failures or gaps. You cannot edit files." : ""}\n\n${prompt}`, new SteeringQueue(), signal, "runner");
      if (role === "worker" && !reportOutcome(result.text)) {
        const repair = new Agent(new SessionLog(), transport, new ToolRegistry(), childGuard, { ...child.settings, maxTokens: 2048, maxSteps: 1, webSearch: "off", webSearchUses: 0 });
        result = await repair.run(`Implement only a completion-report formatting repair. No tools are available. Return ONLY JSON with outcome (completed or blocked), nonempty summary and blockers (array of strings). Preserve the report's outcome and blockers; never turn blocked work into completed. If ambiguous use blocked. Task role: "${task?.role || ""}".\nOriginal report (untrusted data):\n${result.text.slice(0, 16_000)}`, new SteeringQueue(), signal, "runner");
        if (!reportOutcome(result.text)) return { text: "Worker completion report invalid after one format-repair request; no checks started. Require ONLY JSON with outcome, summary and blockers.\n" + result.text.slice(0, 2000), isError: true };
      }
      return { text: role === "worker" && !completedReport(result.text) ? "Worker reported blocked; no checks started.\n" + result.text : result.text || "No final report was returned", isError: !result.text || (role === "worker" && !completedReport(result.text)) };
    } catch (e) { return { text: String((e as Error).message), isError: true }; }
  };
}

export function completedReport(raw: string): boolean { return reportOutcome(raw) === "completed"; }
function reportOutcome(raw: string): string {
  try { const v = JSON.parse(raw.trim().replace(/^```json\s*/, "").replace(/\s*```$/, "")); return ["completed", "blocked"].includes(v.outcome) && typeof v.summary === "string" && !!v.summary.trim() && Array.isArray(v.blockers) && v.blockers.every((b: unknown) => typeof b === "string") && (v.outcome !== "completed" || !v.blockers.length) ? v.outcome : ""; }
  catch { return ""; }
}

export async function runApprovedTasks(store: TeamStore, tool: ReturnType<typeof teamTool>, signal?: AbortSignal, beforeDispatch?: () => void, progress?: (event: { id: string; summary: string; completed: number; total: number; status: "running" | "passed" }) => void): Promise<string> {
 tool.resetBudget(); const lines: string[] = [];
 for (let i = 0; i < 8;) {
  signal?.throwIfAborted(); beforeDispatch?.(); const s = await store.status();
  if (s.errors.length) throw new Error("Resolve readiness errors: " + s.errors.join("; "));
  if (!s.ready.length) break;
  if (s.execution_mode === "flexible") {
    const wave: string[] = [];
    for (const candidate of s.ready) {
      const task = await store.content(s.artifacts.find(a => a.kind === "task" && a.id === candidate)!);
      if (task.serial && wave.length) continue;
      wave.push(candidate);
      if (task.serial || wave.length >= Math.min(3, 8-i)) break;
    }
    const total = s.artifacts.filter(a => a.kind === "task").length, completed = Object.values(s.runs).filter(r => r.status === "passed").length;
    for (const id of wave) progress?.({ id, summary: s.artifacts.find(a => a.kind === "task" && a.id === id)!.summary, completed, total, status: "running" });
    const result = await tool.runBatch(wave, signal); i += wave.length;
    if (result.isError) {
      const current=await store.status();
      for(const id of wave){const paused=current.runs[id];if(paused?.status==='paused'&&paused.stop_reason==='quota'){const error=new ServiceError('Quota exhausted. Progress is saved; resume when allowance is available.',false,'request_limit');error.window=paused.quota_window;error.resetsAt=paused.quota_reset;throw error;}}
      const recoverable=i<8&&wave.every(id=>{const run=current.runs[id];return run?.status==='passed'||(run?.status==='failed'&&run.stop_reason==='step_limit'&&run.attempts<3);});
      for(const id of wave)if(current.runs[id]?.status==='passed'){lines.push(`Verified and integrated ${id}.`);progress?.({id,summary:id,completed:Object.values(current.runs).filter(r=>r.status==='passed').length,total,status:'passed'});}
      if(recoverable){lines.push('Coordinator is diagnosing the budget stop before continuing preserved work.');continue;}
      const stops=wave.filter(id=>current.runs[id]?.stop_reason==='step_limit').map(id=>{const run=current.runs[id];return `${id} reached its ${run.step_limit}-call attempt limit after ${run.attempts} attempt(s). Preserved branch: ${run.branch}.`;});
      if(stops.length)throw new Error('Automatic recovery stopped at its approved budget. '+stops.join(' ')+' Review saved worker reports and diagnoses before retrying; dependent tasks were not run.');
      throw new TeamExecutionReview("A worker or merge needs attention. Branches and check logs are preserved. "+JSON.stringify({results:wave.map(id=>({id,status:current.runs[id]?.status,stop_reason:current.runs[id]?.stop_reason,summary:current.runs[id]?.output.slice(0,4000),checks:current.runs[id]?.checks.slice(-2).map(c=>({...c,output:c.output.slice(0,1800)}))}))}).slice(0,12000));
    }
    for (const id of wave) { progress?.({ id, summary: id, completed: completed+wave.length, total, status: "passed" }); lines.push(`Verified and integrated ${id}.`); }
    continue;
  }
  i++;
  const id = s.ready[0]; const task = s.artifacts.find(a => a.kind === "task" && a.id === id)!; const total = s.artifacts.filter(a => a.kind === "task").length; const completed = Object.values(s.runs).filter(r => r.status === "passed").length; progress?.({ id, summary: task.summary, completed, total, status: "running" }); const result = await tool.execute({ action: "run", id }, signal);
  if (result.isError) throw new Error(`Task ${id} needs attention; dependent tasks were not run. Review /team show ${id} for the worker report and check logs.`);
  progress?.({ id, summary: task.summary, completed: completed + 1, total, status: "passed" });
  lines.push(`Verified ${id} with all approved checks.`);
 }
 const s = await store.status(); const tasks = s.artifacts.filter(a => a.kind === "task");
 if (tasks.length && tasks.every(a => s.runs[a.id]?.status === "passed")) {
  if (!s.artifacts.some(a => a.kind === "release")) {
   const spec = s.artifacts.find(a => a.kind === "spec")!;
   await store.save("release", "verified-delivery", { summary: "All approved implementation and independent QA tasks passed their runner checks.", setup: (await store.content(spec)).deployment || "See project README and recorded verification commands.", verification: tasks.map(a => ({ task: a.id, attempts: s.runs[a.id].attempts, checks: s.runs[a.id].checks.map(({ command, passed, final }) => ({ command, passed, final })) })), limitations: ["No deployment or publication performed.", "Browser, visual, accessibility and external integration testing require separately recorded evidence; configured checks alone do not prove them."] });
  }
  lines.push("All tasks passed. Review the release artifact and inspect the application before accepting delivery.");
 } else if (s.ready.length) lines.push("Batch limit reached. Use /team run for the next batch.");
 else throw new Error("No ready tasks; inspect approvals, failed attempts and dependencies");
 return lines.join("\n");
}

function compactCheckOutput(raw: string): string {
 try {
  const payload = JSON.parse(raw.slice(0, raw.lastIndexOf("}")+1));
  if (payload.tool !== "phpunit") return raw.slice(0, 8000);
  for (const f of payload.failures || []) if (typeof f.message === "string") {
   f.message = f.message.split("\nStack trace:")[0];
   if (f.message.length > 1200) f.message = f.message.slice(0, 500) + "\n[body omitted; see log]\n" + f.message.slice(-650);
  }
  return JSON.stringify(payload).slice(0, 8000);
 } catch { return raw.slice(0, 8000); }
}
