import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, writeFile, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { TeamStore, safeTaskPath, teamTool } from '../src/greenfield.ts';
import { Workspace } from '../src/tools.ts';
const fixtures=JSON.parse(await readFile(new URL('../greenfield/fixtures.json',import.meta.url),'utf8'));
async function seed(){const root=await mkdtemp(join(tmpdir(),'team-inputs-'));const store=new TeamStore(await Workspace.open(root));for(const f of fixtures)await store.save(f.kind,f.id,f.content);return {store,root};}
test('directory reads approve, detect changed children, and refresh after approved writes',async()=>{
 const {store,root}=await seed();await mkdir(join(root,'app'),{recursive:true});await writeFile(join(root,'app/Task.php'),'before');
 const f=fixtures[3];await store.save('task',f.id,{...f.content,read:['app']});let s=await store.status();await store.approve('design',s.gates.design);s=await store.status();await store.approve('spec',s.gates.spec);
 const old=(await store.load()).files.app;await writeFile(join(root,'app/Task.php'),'drift');assert.notEqual(await store.fileHash('app'),old);
 const tool=teamTool(store,async()=>({text:'{"outcome":"completed","summary":"done","blockers":[]}'}),async()=>({text:'Passed'}));assert.match((await tool.execute({action:'run',id:f.id})).text,/changed since approval/);
 await store.approve('spec',(await store.status()).gates.spec);
 const writer=teamTool(store,async()=>{await writeFile(join(root,'app/Task.php'),'approved write');return {text:'{"outcome":"completed","summary":"done","blockers":[]}'};},async()=>({text:'Passed'}));assert.equal((await writer.execute({action:'run',id:f.id})).isError,false);assert.equal((await store.load()).files.app,await store.fileHash('app'));
});
test('environment templates are valid while real credentials and directory writes remain blocked',async()=>{
 assert.equal(safeTaskPath('.env.example'),true);for(const path of ['.env','.env.production','.env.example.bak'])assert.equal(safeTaskPath(path),false);
 const {store,root}=await seed();await mkdir(join(root,'app'),{recursive:true});await store.save('task','foundation',{...fixtures[3].content,write:['app']});assert.match((await store.status()).errors.join(' '),/is a directory/);
 await writeFile(join(root,'app/.env'),'SECRET');await assert.rejects(store.fileHash('app'),/protected/);await symlink(root,join(root,'app/loop'));await assert.rejects(store.fileHash('app'));
});
test('bootstrap restores a partial project without overwriting its files or environment',async()=>{
 const {store,root}=await seed();await mkdir(join(root,'app'),{recursive:true});await writeFile(join(root,'app/Example.php'),'preserved');await writeFile(join(root,'.env'),'preserved secret');
 const result=await store.bootstrap('12.*',async command=>{const staging=/"(\.maxlabs\/team\/scaffold-[^"]+)"/.exec(command)![1];await mkdir(join(root,staging,'bootstrap'),{recursive:true});await mkdir(join(root,staging,'app'),{recursive:true});for(const file of ['composer.json','artisan','bootstrap/app.php','app/Example.php'])await writeFile(join(root,staging,file),'official');return {text:'Created'};});assert.equal(result.isError,undefined);assert.equal(await readFile(join(root,'app/Example.php'),'utf8'),'preserved');assert.equal(await readFile(join(root,'.env'),'utf8'),'preserved secret');assert.equal(await readFile(join(root,'artisan'),'utf8'),'official');
});
