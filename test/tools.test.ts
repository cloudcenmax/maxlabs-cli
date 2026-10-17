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

test('shell pipelines preserve failing stages instead of reporting the filter exit',async(t)=>{
 if(process.platform==='win32') return t.skip('Bash pipeline semantics');
 const root=await mkdtemp(join(tmpdir(),'maxlabs-pipefail-')),tool=createTools(await Workspace.open(root)).get('bash')!;
 for(const command of ['false 2>&1 | tail -20','false 2>&1 | head -40',"printf 'ok\\n' | tail -1 | false"]){const result=await tool.execute({command});assert.equal(result.isError,true,result.text);assert.doesNotMatch(result.text,/Command exited 0/);}
 const result=await tool.execute({command:"printf 'MaxLabs passed\\n' | tail -1"});assert.equal(result.isError,false,result.text);assert.match(result.text,/MaxLabs passed/);
});
