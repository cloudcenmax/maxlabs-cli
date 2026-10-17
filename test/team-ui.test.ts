import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { TeamStore, teamTool } from "../src/greenfield.ts";
import { Workspace } from "../src/tools.ts";
import { TeamPreview } from "../src/team-preview.ts";
import { teamStatusText, teamArtifactText } from "../src/team-view.ts";
import { referenceNamed } from "../src/team-reference.ts";
const fixtures = JSON.parse(await readFile(new URL("../greenfield/fixtures.json", import.meta.url), "utf8"));
async function seed() {
 const store = new TeamStore(await Workspace.open(await mkdtemp(join(tmpdir(), "team-ui-"))));
 await store.setExecutionMode("scoped");
 for (const f of fixtures) await store.save(f.kind, f.id, f.content);
 return store;
}
test("browser preview serves only the validated design on an isolated sandboxed origin", async t => {
 const store = await seed(); let launched = "";
 const preview = new TeamPreview(store, async url => { launched = url; }); t.after(() => preview.close());
 const result = await preview.show(); assert.equal(result.opened, true); assert.equal(result.url, launched);
 const response = await fetch(result.url); assert.equal(response.status, 200); assert.match(await response.text(), /Create task/);
 assert.match(response.headers.get("content-security-policy")!, /sandbox allow-scripts/); assert.match(response.headers.get("content-security-policy")!, /default-src 'none'/);
 assert.equal((await fetch(new URL("/state.json", result.url))).status, 404);
 assert.equal((await fetch(result.url, { method: "POST" })).status, 404);
 const design = fixtures.find(f => f.kind === "design"); await store.save("design", design.id, { ...design.content, prototype_html: "<h1>Revised preview</h1>" });
 assert.equal((await preview.show()).url, result.url); assert.match(await (await fetch(result.url)).text(), /Revised preview/);
});
test("browser launch failure leaves a working URL; invalid designs never start a preview", async t => {
 const store = await seed(); const preview = new TeamPreview(store, async () => { throw new Error("no opener"); }); t.after(() => preview.close());
 const result = await preview.show(); assert.equal(result.opened, false); assert.equal((await fetch(result.url)).status, 200);
 const a = (await store.load()).artifacts.find(a => a.kind === "design")!; await writeFile(await store.path(a.path), "{}");
 await assert.rejects(preview.show(), /changed outside/);
});
test("terminal review exposes scope and checks without HTML or state hashes", async () => {
 const s = await (await seed()).status(); const text = teamStatusText(s);
 assert.match(text, /Design ready for review/); assert.match(text, /\/team preview/); assert.doesNotMatch(text, /awaiting_design_approval|schema_version|[a-f0-9]{64}/);
 const document = teamArtifactText({ summary: "A task", write: ["app/Task.php"], checks: ["php artisan test"], prototype_html: "SECRET_HTML" });
 assert.match(document, /app\/Task.php/); assert.match(document, /php artisan test/); assert.doesNotMatch(document, /SECRET_HTML/);
});
test("chat reference attachment requires a literal human-provided folder", async () => {
 for (const [path, text, expected] of [["../design", "Use ../design for inspiration", true], ["../design", "Use ../design-secret", false], ["/", "Use /", false], ["design", "Use this design", false], ["design", 'Use "design"', true], ["../My Design", "Use `../My Design`", true]] as const) assert.equal(referenceNamed(path, text), expected);
 const store = await seed(); const path = await mkdtemp(join(tmpdir(), "team-reference-")); await writeFile(join(path, "theme.css"), "color: red");
 const blocked = teamTool(store, async () => ({ text: "" }), async () => ({ text: "" })); assert.equal((await blocked.execute({ action: "attach_reference", path })).isError, true);
 const tool = teamTool(store, async () => ({ text: "" }), async () => ({ text: "" }), value => referenceNamed(value, "Use " + path));
 assert.equal((await tool.execute({ action: "attach_reference", path })).isError, undefined); assert.match(await store.reference("reference-1", "theme.css"), /red/);
});

test("new project status invites discovery without treating unwritten plans as failures", async () => {
 const store = new TeamStore(await Workspace.open(await mkdtemp(join(tmpdir(), "team-discovery-"))));
 const text = teamStatusText(await store.status()); assert.match(text,/Shape the idea/); assert.match(text,/Flexible workers/); assert.doesNotMatch(text,/Needs attention|readiness issues|Laravel/);
});
