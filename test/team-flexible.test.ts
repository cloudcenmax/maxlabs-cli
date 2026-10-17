import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { TeamStore, teamTool, teamWorkerStepLimit, runApprovedTasks, type TeamChild } from '../src/greenfield.ts';
import { Workspace, createTools } from '../src/tools.ts';
import { workerEnvironment, sourceFiles, git } from '../src/team-workspace.ts';
const report={text:'{"outcome":"completed","summary":"Outcomes verified","blockers":[]}'};
test('Team worker attempts allow 100 model calls and respect explicit smaller budgets',()=>{
 for(const [configured,expected] of [[0,100],[-1,100],[100,100],[101,100],[60,60],[2,2]])assert.equal(teamWorkerStepLimit(configured),expected);
});
async function seed(conflict=false) {
 const root=await mkdtemp(join(tmpdir(),'team-flexible-')),store=new TeamStore(await Workspace.open(root));
 await store.save('brief','product',{summary:'Personal task tools',scope:['Two simple operations'],open_questions:[]});
 await store.save('design','direction',{summary:'Simple interface',prototype_html:'<h1>Tasks</h1>'});
 await store.save('spec','product',{summary:'A Node application',stack:{runtime:'node'},acceptance:['Create tasks','Complete tasks'],open_questions:[]});
 for(const id of ['alpha','beta'])await store.save('task',id,{summary:`Deliver ${id}`,acceptance:[`Operation ${id} works`]});
 await store.save('task','qa',{summary:'Independent verification',role:'qa',depends_on:['alpha','beta'],acceptance:['Combined behavior works']});
 await store.save('review','review',{summary:'Reviewed outcomes and edge cases',findings:[],blockers:[]});
 assert.deepEqual((await store.status()).errors,[]);let s=await store.status();await store.approve('design',s.gates.design);s=await store.status();await store.approve('spec',s.gates.spec);
 let active=0,peak=0;const seen:string[]=[];
 const child:TeamChild=async(raw,role,task)=>{
  const packet=JSON.parse(raw),id=packet.task.summary.split(' ').at(-1),tools=createTools(await Workspace.open(packet.workspace));
  active++;peak=Math.max(peak,active);seen.push(packet.branch);
  try{await new Promise(r=>setTimeout(r,35));
   const command=task!.role==='qa'?'node --test acceptance.test.mjs':`node --check ${conflict?'shared':id}.mjs`;
   if(task!.role!=='qa')await writeFile(join(packet.workspace,`${conflict?'shared':id}.mjs`),`export const value = '${id}';\n`);
   else {assert.match(await readFile(join(packet.workspace,'alpha.mjs'),'utf8'),/alpha/);assert.match(await readFile(join(packet.workspace,'beta.mjs'),'utf8'),/beta/);}
   const result=await tools.get('bash')!.execute({command});await task!._recordVerification(command,result);return report;
  }finally{active--;}
 };
 // Functional acceptance is independently executed in the QA worktree.
 await writeFile(join(root,'acceptance.test.mjs'),`import test from 'node:test';import assert from 'node:assert/strict';import {value as a} from './alpha.mjs';import {value as b} from './beta.mjs';test('both outcomes',()=>{assert.equal(a,'alpha');assert.equal(b,'beta')});`);
 return {root,store,child,peak:()=>peak,seen};
}
test('flexible workers run concurrently on separate branches, merge, verify, and apply only on release acceptance',async()=>{
 const {root,store,child,peak,seen}=await seed();const tool=teamTool(store,child,async()=>({text:'unused'}));
 await runApprovedTasks(store,tool);let s=await store.status();assert.equal(peak(),2);assert.equal(new Set(seen).size,3);assert.equal(s.stage,'awaiting_release_approval');assert.ok(s.runs.alpha.merged&&s.runs.beta.merged);assert.equal(s.runs.qa.status,'passed');assert.ok(s.execution?.verified_files);
 await assert.rejects(readFile(join(root,'alpha.mjs')),{code:'ENOENT'});const combined=await store.path(s.execution!.path);assert.match(await readFile(join(combined,'beta.mjs'),'utf8'),/beta/);
 s=await store.approve('release',s.gates.release);assert.equal(s.stage,'complete');assert.match(await readFile(join(root,'alpha.mjs'),'utf8'),/alpha/);
 assert.match(await git(combined,['branch','--list']),/task-alpha/);
});
test('parallel merge conflict preserves branches and stops QA dispatch',async()=>{
 const {store,child}=await seed(true),tool=teamTool(store,child,async()=>({text:'unused'}));await assert.rejects(runApprovedTasks(store,tool),/merge needs attention/);const s=await store.status();assert.equal(s.runs.beta.status,'failed');assert.match(s.runs.beta.output,/Merge conflict/);assert.ok(s.runs.beta.commit);assert.equal(s.runs.qa,undefined);assert.deepEqual(s.ready,['beta']);
});
test('release blocks original drift and build changes after QA',async()=>{
 const {root,store,child}=await seed();await runApprovedTasks(store,teamTool(store,child,async()=>({text:'unused'})));let s=await store.status();await writeFile(join(root,'alpha.mjs'),'user work');await assert.rejects(store.approve('release',s.gates.release),/Original project changed/);assert.equal(await readFile(join(root,'alpha.mjs'),'utf8'),'user work');
 await writeFile(join(await store.path(s.execution!.path),'alpha.mjs'),'unreviewed');await assert.rejects(store.approve('release',s.gates.release),/changed after QA/);
});
test('missing runner evidence cannot pass; source snapshots omit secrets and dependencies',async()=>{
 const {root,store}=await seed();const result=await teamTool(store,async()=>report,async()=>({text:'unused'})).runBatch(['alpha']);assert.equal(result.isError,true);assert.equal((await store.status()).runs.alpha.status,'failed');
 await writeFile(join(root,'.env'),'secret');const files=await sourceFiles(root);assert.equal(files['.env'],undefined);process.env.MAXLABS_TEST_SECRET='private';process.env.OPENAI_API_KEY='private';const env=workerEnvironment();assert.equal(env.OPENAI_API_KEY,undefined);assert.equal(env.MAXLABS_TEST_SECRET,undefined);delete process.env.MAXLABS_TEST_SECRET;delete process.env.OPENAI_API_KEY;
});
test('legacy mode survives reopen, and human mode change invalidates approvals',async()=>{
 const {store}=await seed();await store.setExecutionMode('scoped');let s=await store.load();assert.equal(s.approvals.spec,undefined);delete (s as any).execution_mode;await store.persist(s);assert.equal((await store.load()).execution_mode,'scoped');await store.setExecutionMode('flexible');assert.equal((await store.status()).stage,'awaiting_spec_approval');
});
test('serial tasks are scheduled alone and QA shell source edits cannot pass',async()=>{
 const {store,root}=await seed();let s=await store.status();const a=s.artifacts.find(a=>a.id==='alpha')!;await store.save('task','alpha',{...await store.content(a),serial:true});s=await store.status();await store.approve('spec',s.gates.spec);
 let active=0,peak=0;const order:string[]=[];
 const child:TeamChild=async(raw,_role,task)=>{const packet=JSON.parse(raw);active++;peak=Math.max(peak,active);order.push(packet.task.summary);await new Promise(r=>setTimeout(r,20));if(task!.role==='qa')await writeFile(join(packet.workspace,'unreviewed.txt'),'QA changed source');await task!._recordVerification('node --check acceptance.test.mjs',{text:'checked'});active--;return report;};
 await assert.rejects(runApprovedTasks(store,teamTool(store,child,async()=>({text:'unused'}))),/merge needs attention/);s=await store.status();assert.equal(peak,1);assert.deepEqual(order.slice(0,2).sort(),['Deliver alpha','Deliver beta']);assert.equal(order[2],'Independent verification');assert.equal(s.runs.qa.status,'failed');assert.match(s.runs.qa.output,/changed application source/);await assert.rejects(readFile(join(root,'unreviewed.txt')),{code:'ENOENT'});
});
test('flexible child tools operate only in the assigned worktree and record provider usage',async()=>{
 const {store,root}=await seed();const {createTeamChild}=await import('../src/greenfield.ts');const {PermissionGuard}=await import('../src/permission.ts');let calls=0;
 const usage={uncachedInputTokens:100,cacheReadTokens:30,cacheWriteTokens:0,outputTokens:20,reasoningTokens:5,webSearches:0};
 const transport:any={async complete(){calls++;return {message:{role:'assistant',content:calls===1?[{type:'tool_call',callId:'write',name:'write',arguments:JSON.stringify({path:'chosen.mjs',content:'export const ok = true;'})},{type:'tool_call',callId:'verify',name:'verify',arguments:JSON.stringify({command:'node --check chosen.mjs > /tmp/maxlabs-worker-check.log 2>&1'})}]:[{type:'text',text:report.text}]},usage};}};
 const settings:any={model:'test',maxSteps:5,maxTokens:2048,webSearch:'off',webSearchUses:0};const child=createTeamChild(store,transport,new PermissionGuard('act',async()=>{throw new Error('unexpected approval');}),settings);
 const result=await teamTool(store,child,async()=>({text:'unused'})).runBatch(['alpha']);assert.equal(result.isError,false,result.text);const run=(await store.status()).runs.alpha;assert.equal(run.usage?.calls,2);assert.equal(run.usage?.input_tokens,260);assert.equal(run.usage?.output_tokens,40);assert.equal(run.usage?.reasoning_tokens,10);assert.equal(run.checks.length,1);await assert.rejects(readFile(join(root,'chosen.mjs')),{code:'ENOENT'});
});
test('retry recovers uncommitted files left by a stopped older worker',async()=>{
 const {store,root,child}=await seed();const tool=teamTool(store,async()=>({text:'blocked',isError:true}),async()=>({text:'unused'}));await tool.runBatch(['alpha']);const state=await store.load(),run=state.runs.alpha,oldRoot=await store.path(run.workspace!);await writeFile(join(oldRoot,'recovered.mjs'),'export const recovered=true;');delete run.commit;run.status='interrupted';await store.persist(state);
 const resumed=await teamTool(store,child,async()=>({text:'unused'})).runBatch(['alpha']);assert.equal(resumed.isError,false,resumed.text);const s=await store.status();assert.match(await readFile(join(await store.path(s.execution!.path),'recovered.mjs'),'utf8'),/recovered/);await assert.rejects(readFile(join(root,'recovered.mjs')),{code:'ENOENT'});
});

test('failing piped verification cannot merge a worker branch',async()=>{
 const {store}=await seed();const {createTeamChild}=await import('../src/greenfield.ts');const {PermissionGuard}=await import('../src/permission.ts');let calls=0;
 const transport:any={async complete(){calls++;return {message:{role:'assistant',content:calls===1?[{type:'tool_call',callId:'verify',name:'verify',arguments:JSON.stringify({command:'false 2>&1 | tail -20'})}]:[{type:'text',text:report.text}]},usage:{uncachedInputTokens:10,cacheReadTokens:90,cacheWriteTokens:0,outputTokens:5,reasoningTokens:0,webSearches:0}};}};
 const child=createTeamChild(store,transport,new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:3,maxTokens:1024,thinking:'minimal',webSearch:'off',webSearchUses:0});
 const result=await teamTool(store,child,async()=>({text:'unused'})).runBatch(['alpha']);assert.equal(result.isError,true);const run=(await store.status()).runs.alpha;assert.equal(run.status,'failed');assert.equal(run.merged,undefined);assert.equal(run.checks[0].passed,false);
});

function recoveryTransport(keepCapping=false,mutate=false,blind=false,cacheOutput=false){
 let originalBranch='',recoveryCalls=0;
 const usage={uncachedInputTokens:10,cacheReadTokens:90,cacheWriteTokens:0,outputTokens:5,reasoningTokens:0,webSearches:0};
 const transport:any={async complete(request:any){
  let packet:any={};let recovery=false,hasResult=false;
  for(const m of request.messages)for(const b of m.content){if(b.type==='tool_result')hasResult=true;if(b.text?.startsWith('You are the MaxLabs engineering coordinator'))recovery=true;const pos=b.text?.lastIndexOf('\n\n{')??-1;if(pos>=0)try{packet=JSON.parse(b.text.slice(pos+2));}catch{}}
  const call=(name:string,args:any)=>({type:'tool_call',callId:name,name,arguments:JSON.stringify(args)}),answer=(content:any[])=>({message:{role:'assistant',content},usage});
  if(recovery){recoveryCalls++;if(!hasResult&&!blind){if(cacheOutput)await writeFile(join(packet.workspace,'.phpunit.result.cache'),'generated test timings');return answer([mutate?call('verify',{command:'printf unexpected > partial.txt'}):call('read',{path:'partial.txt'})]);}return answer([{type:'text',text:JSON.stringify({action:'continue',summary:'Observed fixture failure; finish using the preserved dependency setup.',next_steps:['Complete alpha.mjs and run targeted verification without discarding partial work or vendor.']})}]);}
  const id=packet.task?.role==='qa'?'qa':packet.task?.summary?.split(' ').at(-1);
  if(id==='alpha'&&(!packet.recovery_plan||keepCapping)){originalBranch ||= packet.branch;return answer(hasResult?[call('verify',{command:"printf 'Invalid signature fixture\\n' >&2; false"})]:[call('write',{path:'partial.txt',content:'preserved work'}),call('write',{path:'vendor/retained.txt',content:'installed dependency fixture'})]);}
  if(hasResult)return answer([{type:'text',text:report.text}]);
  if(id==='qa')return answer([call('verify',{command:'node --test acceptance.test.mjs'})]);
  return answer([call('write',{path:id+'.mjs',content:`export const value='${id}';`}),call('verify',{command:(id==='alpha'?'test -f partial.txt && test -f vendor/retained.txt && ':'')+'node --check '+id+'.mjs'})]);
 }};
 return {transport,get originalBranch(){return originalBranch;},get recoveryCalls(){return recoveryCalls;}};
}
test('orchestrator diagnoses a budget stop and continues automatically with existing setup',async()=>{
 const {store}=await seed();const {createTeamChild}=await import('../src/greenfield.ts');const {PermissionGuard}=await import('../src/permission.ts');const model=recoveryTransport(false,false,false,true);let usageCalls=0;
 const child=createTeamChild(store,model.transport,new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:2,maxTokens:1024,thinking:'minimal',webSearch:'off',webSearchUses:0},()=>'',undefined,()=>usageCalls++);
 await runApprovedTasks(store,teamTool(store,child,async()=>({text:'unused'})));
 const s=await store.status(),run=s.runs.alpha;assert.equal(run.status,'passed');assert.equal(run.attempts,2);assert.equal(run.branch,model.originalBranch);assert.equal(model.recoveryCalls,2);assert.equal(s.runs.qa.status,'passed');assert.ok(usageCalls>=10);
 assert.equal(run.recovery_trail?.length,1);assert.equal(run.recovery_trail![0].plan.action,'continue');assert.equal(run.recovery_trail![0].usage.calls,2);
 assert.deepEqual(run.recovery_trail![0].changed_files,[]);
 assert.match(await readFile(join(await store.path(run.workspace!),'vendor/retained.txt'),'utf8'),/dependency/);
});
test('new user direction stops dispatch before automatic recovery',async()=>{
 const {store}=await seed();const {createTeamChild}=await import('../src/greenfield.ts');const {PermissionGuard}=await import('../src/permission.ts');const model=recoveryTransport();let dispatches=0;
 const child=createTeamChild(store,model.transport,new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:2,maxTokens:1024,thinking:'minimal',webSearch:'off',webSearchUses:0});
 await assert.rejects(runApprovedTasks(store,teamTool(store,child,async()=>({text:'unused'})),undefined,()=>{if(++dispatches>1)throw new Error('new user direction');}),/new user direction/);
 assert.equal(model.recoveryCalls,0);assert.equal((await store.status()).runs.alpha.attempts,1);
});
test('automatic recovery stops at three attempts and rejects source edits or blind retries',async()=>{
 const {createTeamChild}=await import('../src/greenfield.ts');const {PermissionGuard}=await import('../src/permission.ts');
 for(const mode of ['bounded','mutate','blind']){
  const {store}=await seed(),model=recoveryTransport(mode==='bounded',mode==='mutate',mode==='blind');
  const child=createTeamChild(store,model.transport,new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:2,maxTokens:1024,thinking:'minimal',webSearch:'off',webSearchUses:0});
  await assert.rejects(runApprovedTasks(store,teamTool(store,child,async()=>({text:'unused'}))),mode==='bounded'?/Automatic recovery stopped at its approved budget.*Preserved branch:/:/Coordinator needs attention/);const s=await store.status();assert.equal(s.runs.alpha.attempts,mode==='bounded'?3:1);assert.equal(s.runs.qa,undefined);if(mode==='bounded')assert.equal(model.recoveryCalls,4);else assert.equal(s.runs.alpha.recovery?.plan.action,'blocked');if(mode==='mutate'){assert.deepEqual(s.runs.alpha.recovery?.changed_files,['partial.txt']);assert.match(s.runs.alpha.recovery!.plan.summary,/partial.txt/);}
 }
});
