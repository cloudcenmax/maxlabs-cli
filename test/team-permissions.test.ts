import assert from 'node:assert/strict';
import { mkdtemp, readFile, symlink, writeFile, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { workerAutoCommand } from '../src/team-permissions.ts';
import { FlexiblePermissions } from '../src/team-flexible.ts';
import { TeamStore } from '../src/greenfield.ts';
import { Workspace, createTools } from '../src/tools.ts';
import { PermissionGuard } from '../src/permission.ts';
const cases=JSON.parse(await readFile(new URL('../greenfield/worker-command-cases.json',import.meta.url),'utf8'));
test('worker Auto permits bounded test output, with the same command cases as desktop',async()=>{
 const root=await mkdtemp(join(tmpdir(),'team-permission-')),outside=await mkdtemp(join(tmpdir(),'team-outside-'));
 await writeFile(join(root,'.env.example'),'APP_NAME=Test');
 await symlink(outside,join(root,'escape'));await symlink(outside,join(root,'quoted-escape'));await writeFile(join(outside,'secret'),'private');await symlink(join(outside,'secret'),join(root,'link-to-secret.log'));
 const guard = new FlexiblePermissions(new TeamStore(await Workspace.open(root)),false,new PermissionGuard('auto',async()=> 'deny'));
 const tool:any={schema:{name:'verify'},readOnly:false};
 for(const entry of cases){const command=entry.command.replaceAll('{root}',root).replaceAll('{tmp}',tmpdir());const assessed=await workerAutoCommand(root,command);const auto=assessed!==undefined&&await guard.permit(tool,{command})===undefined;assert.equal(auto,entry.auto,command+' => '+assessed);}
});
test('worker setup preserves existing environment configuration',async()=>{
 const root=await mkdtemp(join(tmpdir(),'team-env-permission-'));await writeFile(join(root,'.env.example'),'APP_NAME=Test');await writeFile(join(root,'.env'),'APP_KEY=existing');
 assert.match((await workerAutoCommand(root,'cp .env.example .env'))!,/\.env/);
 assert.equal(await workerAutoCommand(root,'cp -n .env.example .env'),'true');
 assert.equal(await readFile(join(root,'.env'),'utf8'),'APP_KEY=existing');
});

test('reported inspection command executes without prompting or reading environment values',async()=>{
 const root=await mkdtemp(join(tmpdir(),'team-inspection-'));await mkdir(join(root,'tests/Feature'),{recursive:true});await writeFile(join(root,'.env'),'DO_NOT_PRINT_ENV_VALUE');
 for(const name of ['SeatsAndInvitesTest.php','SeatBillingTest.php'])await writeFile(join(root,'tests/Feature',name),'<?php // seat test fixture');
 let prompts=0;const workspace=await Workspace.open(root),guard=new FlexiblePermissions(new TeamStore(workspace),false,new PermissionGuard('auto',async()=>{prompts++;return 'deny';}));
 const command='ls -la .env 2>/dev/null; ls tests; cat tests/Feature/SeatsAndInvitesTest.php tests/Feature/SeatBillingTest.php | head -200',shell=createTools(workspace).get('bash')!;
 assert.equal(await guard.permit(shell,{command}),undefined);const result=await shell.execute({command});assert.equal(result.isError,false,result.text);assert.equal(prompts,0);assert.match(result.text,/seat test fixture/);assert.doesNotMatch(result.text,/DO_NOT_PRINT_ENV_VALUE/);
});

test('literal worker file payload executes unchanged without prompting',async()=>{
 const root=await mkdtemp(join(tmpdir(),'team-literal-'));
 const payload="<?php\n$literal = '$(touch should-not-exist)';\n";
 const command="cat > check.php <<'PHP'\n"+payload+"PHP\nprintf 'MaxLabs test passed\\n' > test-output.log\ncat test-output.log";
 let prompts=0;
 const workspace=await Workspace.open(root),tools=createTools(workspace),guard=new FlexiblePermissions(new TeamStore(workspace),false,new PermissionGuard('auto',async()=>{prompts++;return 'deny';}));
 const tool=tools.get('bash')!;
 assert.equal(await guard.permit(tool,{command}),undefined);
 const result=await tool.execute({command});assert.equal(result.isError,false,result.text);assert.equal(prompts,0);assert.match(result.text,/MaxLabs test passed/);
 assert.equal(await readFile(join(root,'check.php'),'utf8'),payload);
 await assert.rejects(readFile(join(root,'should-not-exist')),{code:'ENOENT'});
});
