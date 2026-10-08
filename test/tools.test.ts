import assert from "node:assert/strict";
import { mkdtemp, readFile, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { createTools, Workspace } from "../src/tools.ts";

test("workspace tools read, write, edit, glob and grep", async () => {
  const root = await mkdtemp(join(tmpdir(), "maxlabs-cli-"));
  const workspace = await Workspace.open(root);
  const tools = createTools(workspace);
  await tools.get("write")!.execute({ path: "src/a.txt", content: "one\ntwo\n" });
  await tools.get("edit")!.execute({ path: "src/a.txt", old_text: "two", new_text: "second" });
  assert.equal(await readFile(join(root, "src/a.txt"), "utf8"), "one\nsecond\n");
  assert.match((await tools.get("glob")!.execute({ pattern: "**/*.txt" })).text, /src\/a.txt/);
  assert.match((await tools.get("grep")!.execute({ pattern: "second" })).text, /src\/a.txt:2/);
});

test("workspace rejects traversal and symlink escape", async () => {
  const root = await mkdtemp(join(tmpdir(), "maxlabs-cli-root-"));
  const outside = await mkdtemp(join(tmpdir(), "maxlabs-cli-out-"));
  await writeFile(join(outside, "secret"), "nope");
  await symlink(outside, join(root, "escape"));
  const workspace = await Workspace.open(root);
  await assert.rejects(workspace.path("../outside"), /escapes/);
  await assert.rejects(workspace.path("escape/secret"), /outside/);
});
