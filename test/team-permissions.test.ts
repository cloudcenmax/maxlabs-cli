import assert from 'node:assert/strict';
import { mkdtemp, readFile, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { workerAutoCommand } from '../src/team-permissions.ts';
import { FlexiblePermissions } from '../src/team-flexible.ts';
import { TeamStore } from '../src/greenfield.ts';
import { Workspace } from '../src/tools.ts';
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
