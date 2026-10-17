import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { TeamStore, allowedCheck, completedReport, safeTaskPath, teamTool, createTeamChild } from "../src/greenfield.ts";
import { Workspace } from "../src/tools.ts";
import { PermissionGuard } from "../src/permission.ts";
const fixtures = JSON.parse(await readFile(new URL("../greenfield/fixtures.json", import.meta.url), "utf8"));
async function seeded() {
 const root = await mkdtemp(join(tmpdir(), "maxlabs-team-")); const store = new TeamStore(await Workspace.open(root));
 await store.setExecutionMode("scoped");
 for (const f of fixtures) await store.save(f.kind, f.id, f.content);
 return { store, root };
}
async function approve(store: TeamStore) { let s = await store.status(); await store.approve("design", s.gates.design); s = await store.status(); return store.approve("spec", s.gates.spec); }
const complete = async () => ({ text: '{"outcome":"completed","summary":"Implemented","blockers":[]}' });
test("team gates persist, dependencies order execution and release requires passing QA", async () => {
 const { store } = await seeded(); let s = await store.status(); assert.deepEqual(s.errors, []); assert.equal(s.stage, "awaiting_design_approval"); assert.deepEqual(s.ready, []);
 await assert.rejects(store.approve("spec", s.gates.spec), /approve design/);
 s = await approve(store); assert.deepEqual(s.ready, ["foundation"]);
 const tool = teamTool(store, complete, async () => ({ text: "Passed" }));
 assert.equal((await tool.execute({ action: "run", id: "qa" })).isError, true);
 assert.equal((await tool.execute({ action: "run", id: "foundation" })).isError, false);
 assert.deepEqual((await store.status()).ready, ["authorization"]);
 s = await store.status(); await assert.rejects(store.approve("release", s.gates.release), /passing tasks/);
 await tool.execute({ action: "run", id: "authorization" }); await tool.execute({ action: "run", id: "qa" });
 s = await store.status(); s = await store.approve("release", s.gates.release); assert.equal(s.stage, "complete");
 const reopened = new TeamStore(await Workspace.open(store.workspace.root)); assert.equal((await reopened.status()).stage, "complete");
});
test("failed checks never become passed and attempts are bounded", async () => {
 const { store } = await seeded(); await approve(store); const tool = teamTool(store, complete, async () => ({ text: "Assertion failed", isError: true }));
 for (let i = 0; i < 3; i++) assert.equal((await tool.execute({ action: "run", id: "foundation" })).isError, true);
 const s = await store.status(); assert.equal(s.runs.foundation.status, "failed"); assert.equal(s.runs.foundation.attempts, 3); assert.deepEqual(s.ready, []);
});
test("contract edits invalidate approvals and artifact tampering blocks execution", async () => {
 const { store } = await seeded(); await approve(store); const before = await store.status();
 const design = fixtures.find((f: any) => f.kind === "design"); await store.save("design", design.id, { ...design.content, summary: "Revised design" });
 let s = await store.status(); assert.equal(s.stage, "awaiting_design_approval"); await assert.rejects(store.approve("design", before.gates.design), /stale/);
 const a = s.artifacts.find(a => a.kind === "task")!; await writeFile(await store.path(a.path), '{}'); s = await store.status(); assert.ok(s.errors.some(e => e.includes("outside the workflow"))); assert.deepEqual(s.ready, []);
});
test("external changes to worker inputs require review and reapproval", async () => {
 const { store, root } = await seeded(); await approve(store); await mkdir(join(root, "app")); await writeFile(join(root, "app/Task.php"), "unexpected change");
 let called = false; const tool = teamTool(store, async () => { called = true; return complete(); }, async () => ({ text: "passed" }));
 const result = await tool.execute({ action: "run", id: "foundation" }); assert.equal(result.isError, true); assert.match(result.text, /changed since approval/); assert.equal(called, false);
});
test("task DAG rejects cycles, unknown requirements and mutable QA scope", async () => {
 const { store } = await seeded(); const task = fixtures.find((f: any) => f.id === "foundation");
 await store.save("task", task.id, { ...task.content, depends_on: ["qa"], requirements: ["invented"] });
 const s = await store.status(); assert.ok(s.errors.some(e => e.includes("cycle"))); assert.ok(s.errors.some(e => e.includes("Unknown requirement")));
 const qa = fixtures.find((f: any) => f.id === "qa"); await assert.rejects(store.save("task", qa.id, { ...qa.content, write: ["app/Task.php"] }), /read-only/);
});
test("references require user attachment and cannot traverse or expose environment files", async () => {
 const { store } = await seeded(); const reference = await mkdtemp(join(tmpdir(), "maxlabs-reference-")); await writeFile(join(reference, "theme.css"), ":root {color:red}"); await writeFile(join(reference, ".env"), "SECRET=yes");
 await assert.rejects(store.reference("reference-1", "theme.css"), /not attached/); await store.attach(reference); assert.match(await store.reference("reference-1", "theme.css"), /color:red/);
 await assert.rejects(store.reference("reference-1", "../other"), /escapes/); await assert.rejects(store.reference("reference-1", ".env"), /Credential/);
 const outside = await mkdtemp(join(tmpdir(), "maxlabs-outside-")); await symlink(outside, join(store.workspace.root, "escape")); await assert.rejects(store.save("task", "foundation", { ...fixtures[3].content, write: ["escape/file.php"] }).then(async () => { await approve(store); }), /outside/);
});
test("parallel research stores compact reports and does not execute application checks", async () => {
 const { store } = await seeded(); let active = 0; let peak = 0;
 const tool = teamTool(store, async () => { active++; peak = Math.max(peak, active); await new Promise(r => setTimeout(r, 10)); active--; return { text: "Evidence from https://example.test/docs" }; }, async () => { throw new Error("No checks during research"); });
 const result = await tool.execute({ action: "research", jobs: ["Inspect auth", "Inspect webhooks", "Inspect errors"] }); assert.equal(result.isError, undefined); assert.equal(peak, 3); assert.equal(JSON.parse(result.text).reports.length, 3);
});
test("worker reports and verification commands reject ambiguous success and shell expansion", () => {
 assert.equal(completedReport("All done"), false); assert.equal(completedReport('{"outcome":"blocked","summary":"Missing contract","blockers":["unknown"]}'), false); assert.equal(completedReport('{"outcome":"completed","summary":"Done","blockers":[]}'), true);
 for (const command of ["php artisan test; rm -rf app", "npm test $(id)", "php artisan test --configuration evil.xml", "php -r 'eval()'"]) assert.equal(allowedCheck(command), false);
 assert.equal(allowedCheck("php artisan test --filter=Tasks"), true); for (const path of ["../a", "/tmp/a", ".maxlabs/team/state.json", ".git/config", "app/../a", ".MAXLABS/team/state.json", ".Git/config", "app/*.php", "app/file.php."]) assert.equal(safeTaskPath(path), false);
});

test("real worker tools enforce exact file scope without repeated approval prompts", async () => {
 const { store, root } = await seeded(); let turn = 0;
 const transport: any = { async complete() { turn++; return { message: { role: "assistant", content: turn === 1 ? [{ type: "tool_call", callId: "escape", name: "write", arguments: JSON.stringify({ path: ".maxlabs/team/state.json", content: "{}" }) }, { type: "tool_call", callId: "allowed", name: "write", arguments: JSON.stringify({ path: "app/Task.php", content: "<?php // task" }) }] : [{ type: "text", text: '{"outcome":"completed","summary":"Done","blockers":[]}' }] }, usage: { uncachedInputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: 0, reasoningTokens: 0, webSearches: 0 } }; } };
 const guard = new PermissionGuard("act", async () => { throw new Error("Approved file scopes should not ask again"); });
 const child = createTeamChild(store, transport, guard, { model: "worker", thinking: "low", maxSteps: 3, maxTokens: 1024, webSearch: "off", webSearchUses: 0 });
 const before = await readFile(await store.path("state.json"), "utf8"); const result = await child("Implement", "worker", { write: ["app/Task.php"] }); assert.equal(result.isError, false); assert.equal(await readFile(await store.path("state.json"), "utf8"), before); assert.match(await readFile(join(root, "app/Task.php"), "utf8"), /task/);
});
test("bootstrap uses the official skeleton and refuses an existing application", async () => {
 const root = await mkdtemp(join(tmpdir(), "maxlabs-bootstrap-")); const store = new TeamStore(await Workspace.open(root)); await store.attach(await mkdtemp(join(tmpdir(), "reference-")));
 const result = await store.bootstrap("13.*", async command => { assert.match(command, /^composer create-project laravel\/laravel /); const path = /"(\.maxlabs\/team\/scaffold-[^"]+)"/.exec(command)![1]; await mkdir(join(root, path, "app"), { recursive: true }); await mkdir(join(root, path, "bootstrap"), { recursive: true }); await writeFile(join(root, path, "artisan"), "<?php"); await writeFile(join(root, path, "bootstrap/app.php"), "<?php"); await writeFile(join(root, path, "composer.json"), '{}'); await writeFile(join(root, path, "app/Example.php"), '<?php'); return { text: "Created" }; });
 assert.equal(result.isError, undefined); assert.equal(await readFile(join(root, "app/Example.php"), "utf8"), "<?php");
 assert.equal((await store.bootstrap("12.*", async () => { throw new Error("Must not execute"); })).isError, true);
});
test("interrupted executions can resume after an expired lease", async () => {
 const { store } = await seeded(); await approve(store); await store.locked(async s => { s.runs.foundation = { status: "running", attempts: 1, output: "", checks: [] }; await store.persist(s); });
 const s = await store.status(); assert.equal(s.runs.foundation.status, "interrupted"); assert.deepEqual(s.ready, ["foundation"]);
 const tool = teamTool(store, complete, async () => ({ text: "Passed" })); assert.equal((await tool.execute({ action: "run", id: "foundation" })).isError, false);
});
test("research evidence does not invalidate approved implementation", async () => {
 const { store } = await seeded(); const before = await approve(store); await store.save("evidence", "source-test", { summary: "Official API reference", source_url: "https://example.test/docs", retrieved_at: new Date().toISOString(), content: "API schema", content_hash: "fixture" });
 const after = await store.status(); assert.equal(after.gates.spec, before.gates.spec); assert.deepEqual(after.ready, ["foundation"]);
 const tool = teamTool(store, complete, async () => ({ text: "Passed" })); assert.equal((await tool.execute({ action: "save", kind: "evidence", id: "forged", content: {} })).isError, true);
});
test("research fetches persist source snapshots and flag truncated documentation", async () => {
 const { store } = await seeded(); const previousFetch = globalThis.fetch; globalThis.fetch = async () => new Response("x".repeat(90_000), { headers: { "content-type": "application/json" } });
 try {
  let step = 0; const transport: any = { async complete() { step++; return { message: { role: "assistant", content: step === 1 ? [{ type: "tool_call", callId: "source", name: "webfetch", arguments: '{"url":"https://93.184.216.34/openapi.json"}' }] : [{ type: "text", text: "Source is an excerpt; endpoint details remain unknown." }] }, usage: { uncachedInputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: 0, reasoningTokens: 0, webSearches: 0 } }; } };
  const child = createTeamChild(store, transport, new PermissionGuard("act", async () => "once"), { model: "planner", thinking: "low", maxSteps: 3, maxTokens: 1024, webSearch: "off", webSearchUses: 0 });
  assert.equal((await child("Research the endpoint", "research", undefined)).isError, false);
  const artifact = (await store.status()).artifacts.find(a => a.kind === "evidence")!; const source = await store.content(artifact); assert.equal(source.source_url, "https://93.184.216.34/openapi.json"); assert.match(source.content, /truncated/); assert.equal(source.content_hash.length, 64);
 } finally { globalThis.fetch = previousFetch; }
});
test("symlink aliases cannot expose reference credentials or overwrite workflow state", async () => {
 const { store, root } = await seeded(); await symlink(join(root, ".maxlabs/team"), join(root, "planning-alias")); await assert.rejects(store.workerPath("planning-alias/state.json"), /workflow state|protected/);
 const reference = await mkdtemp(join(tmpdir(), "reference-secret-")); await writeFile(join(reference, ".env"), "SECRET"); await symlink(join(reference, ".env"), join(reference, "theme.css")); await store.attach(reference); await assert.rejects(store.reference("reference-1", "theme.css"), /Credential/);
 await assert.rejects(store.save("brief", undefined as any, fixtures[0].content), /invalid/);
});

test("unresolved brief decisions cannot be design-approved", async () => {
 const { store } = await seeded(); const brief = fixtures.find((f: any) => f.kind === "brief");
 await store.save("brief", brief.id, { ...brief.content, open_questions: ["Which database?"] });
 const s = await store.status(); await assert.rejects(store.approve("design", s.gates.design), /open questions/);
});
test("environment probes use only fixed commands and return observed failures", async () => {
 const { store } = await seeded(); const commands: string[] = [];
 const tool = teamTool(store, complete, async command => { commands.push(command); return { text: command === "php --version" ? "PHP 8.4" : "missing", isError: command !== "php --version" }; });
 const result = JSON.parse((await tool.execute({ action: "environment", command: "untrusted" })).text);
 assert.deepEqual(commands, ["php --version", "composer --version", "node --version", "npm --version"]); assert.equal(result.observed[0].available, true); assert.equal(result.observed[1].available, false);
});

test("worker report formatting gets one tool-free repair and never skips checks", async () => {
 const { store } = await seeded(); let requests = 0;
 const transport: any = { async complete(request: any) { requests++; if (requests === 2) assert.equal(request.tools.length, 0); return { message: { role: "assistant", content: [{ type: "text", text: requests === 1 ? 'Done.\n{"outcome":"completed","summary":"Implemented","blockers":[]}' : '{"outcome":"completed","summary":"Implemented","blockers":[]}' }] }, usage: { uncachedInputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: 0, reasoningTokens: 0, webSearches: 0 } }; } };
 const guard = new PermissionGuard("act", async () => "deny");
 const child = createTeamChild(store, transport, guard, { model: "worker", thinking: "low", maxSteps: 3, maxTokens: 1024, webSearch: "off", webSearchUses: 0 });
 assert.equal((await child("Implement", "worker", { role: "foundation", write: [] })).isError, false); assert.equal(requests, 2);
});
test("compact artifact reads omit prototype HTML while preserving it on disk", async () => {
 const { store } = await seeded(); const design = (await store.load()).artifacts.find(a => a.kind === "design")!;
 const tool = teamTool(store, complete, async () => ({ text: "passed" }));
 const result = JSON.parse((await tool.execute({ action: "artifact", kind: "design", id: design.id })).text);
 assert.equal(result.content.prototype_html, undefined); assert.equal(typeof (await store.content(design)).prototype_html, "string");
});

test("deterministic dispatch completes the DAG and preserves archived results", async () => {
 const { runApprovedTasks } = await import("../src/greenfield.ts"); const { store } = await seeded(); await approve(store); let children = 0;
 const tool = teamTool(store, async () => { children++; return complete(); }, async () => ({ text: "Passed" }));
 assert.match(await runApprovedTasks(store, tool), /All tasks passed/); assert.equal(children, 3);
 const s = await store.status(); assert.equal(s.stage, "awaiting_release_approval"); for (const run of Object.values(s.runs)) assert.ok(run.archive);
});
test("deterministic dispatch never starts dependents after failed checks", async () => {
 const { runApprovedTasks } = await import("../src/greenfield.ts"); const { store } = await seeded(); await approve(store); let children = 0;
 const tool = teamTool(store, async () => { children++; return complete(); }, async () => ({ text: "Assertion failed", isError: true }));
 await assert.rejects(runApprovedTasks(store, tool), /dependent tasks were not run/); assert.equal(children, 1);
});

test("independent QA receives successful check evidence before its review", async () => {
 const { store } = await seeded(); await approve(store); let checks = 0; let reviewed = false;
 const tool = teamTool(store, async (prompt, _role, task) => { if (task?.role === "qa") { const p = JSON.parse(prompt); assert.ok(p.verification_evidence.length); assert.ok(p.verification_evidence.every((c: any) => c.passed)); assert.equal(checks, 3); reviewed = true; } return complete(); }, async () => { checks++; return { text: "Passed" }; });
 await tool.execute({ action: "run", id: "foundation" }); await tool.execute({ action: "run", id: "authorization" }); assert.equal((await tool.execute({ action: "run", id: "qa" })).isError, false); assert.equal(reviewed, true); assert.equal(checks, 3);
});
