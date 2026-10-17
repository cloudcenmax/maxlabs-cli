import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import { randomUUID } from 'node:crypto';
import { Agent, StepLimitError, type AgentHooks } from './agent.ts';
import { sourceDigest, captureAction, budgetHandoff } from './team-recovery.ts';
import type { WorkerAction } from './team-recovery.ts';
import { workerAutoCommand } from './team-permissions.ts';
import { PermissionGuard, freshApprovalRisk } from './permission.ts';
import { SessionLog, SteeringQueue } from './session.ts';
import { Workspace, createTools, ToolRegistry, type Tool } from './tools.ts';
import type { AgentSettings, ToolResult } from './types.ts';
import type { GatewayTransport } from './transport.ts';
import { completedReport, teamWorkerStepLimit, type TeamStore, type TeamChild, type TaskRun } from './greenfield.ts';
import { checkpoint, git, prepareExecution, sourceFiles, sameFiles, workerEnvironment } from './team-workspace.ts';
const outside=/(\.\.[/\\]|~[/\\]|\/(Users|home|etc|root|private|tmp|var|Applications|Volumes)(?:[/\s]|$)|\b(curl|wget|ssh|scp|sftp|rsync|security|sudo)\b|\.maxlabs|\.git[/\\]|(?:^|[\s/])\.env(?:[\s.]|$)|\b(?:npm|pnpm|yarn|bun)\s+publish\b)/i;
function verified(checks:TaskRun['checks']):boolean {const latest=new Map(checks.map(c=>[c.command,c.passed]));return latest.size>0&&[...latest.values()].every(Boolean);}
export async function runFlexibleWave(store:TeamStore,child:TeamChild,ids:string[],signal?:AbortSignal):Promise<ToolResult>{
 return store.locked(async state=>{
  const status=await store.status(state);if(!ids.length||ids.length>3||ids.some(id=>!status.ready.includes(id)))throw new Error('Parallel wave contains a task that is not ready');
  if(new Set(ids).size!==ids.length)throw new Error('Duplicate task in parallel wave');
  if(ids.length>1)for(const id of ids){const a=state.artifacts.find(a=>a.kind==='task'&&a.id===id)!;if((await store.content(a)).serial)throw new Error('Serial tasks must run alone');}
  const integration=await prepareExecution(store,state,signal),execution=state.execution!,briefs=[];const tasks=new Map<string,Record<string,any>>();
  for(const a of state.artifacts){if(a.kind==='task')tasks.set(a.id,await store.content(a));if(['brief','spec','design','integration'].includes(a.kind)){const content=await store.content(a);delete content.prototype_html;briefs.push({kind:a.kind,content});}}
  const evidence=Object.fromEntries(Object.entries(state.runs).map(([id,run])=>[id,{status:run.status,summary:run.output.slice(0,1000),checks:run.checks.map(c=>({...c,output:c.output.slice(0,1600)})),branch:run.branch}]));
  const jobs=[];
  for(const id of ids){const task=tasks.get(id)!,previous=state.runs[id],run:TaskRun={id:randomUUID(),status:'running',attempts:(previous?.attempts||0)+1,output:'',checks:[]};let path=`${dirname(execution.path).replaceAll('\\','/')}/workers/${id}-${run.id}`,root=await store.path(path,true),branch=`task-${id}-${run.id}`;
   if(previous&&!previous.commit&&previous.workspace){previous.commit=await checkpoint(await store.path(previous.workspace),`Preserve interrupted task ${id}`,signal);await store.persist(state);}
   const base=previous?.status!=='passed'&&previous?.commit?previous.commit:'HEAD',resume=previous?.stop_reason==='step_limit'&&previous.recovery?.plan.action==='continue';
   if(resume){run.recovery_trail=[...(previous.recovery_trail||[]),previous.recovery!];path=previous.workspace!;root=await store.path(path);branch=previous.branch!;if(await sourceDigest(root)!==previous.recovery!.source_hash)throw new Error('Worker changed after coordinator review');await checkpoint(root,`Preserve coordinator continuation ${id}`,signal);}else await git(integration,['worktree','add','-b',branch,root,base],signal);
   const packet:Record<string,any>={task,briefs,workspace:root,branch,implementation_evidence:evidence,previous_attempt:previous?{status:previous.status,summary:previous.output.slice(0,1600),branch:previous.branch,stop_reason:previous.stop_reason,steps_used:previous.steps_used,checks:previous.checks.map(c=>({...c,output:c.output.slice(0,1600)})),recent_actions:previous.recent_actions}:undefined,recovery_plan:resume?previous.recovery!.plan:undefined};
   if(base!=='HEAD'){try{await git(root,['merge','--no-edit',execution.commit!],signal);}catch{packet.merge_conflicts=await git(root,['diff','--name-only','--diff-filter=U'],signal);}}
   if(task.role==='qa')packet.change_summary=await git(root,['diff','--stat',execution.base_commit,'HEAD'],signal);
   Object.assign(run,{branch,workspace:path});state.runs[id]=run;jobs.push({id,task,run,root,packet});
  }
  delete state.approvals.release;delete execution.verified_files;await store.persist(state);
  // Model work runs concurrently in separate worktrees; integration and manifest writes stay serialized.
  const results=await Promise.all(jobs.map(async job=>{const started=Date.now(); const {id,task,root}=job,run={...job.run,checks:[] as TaskRun['checks']};try{
    const before=await sourceFiles(root),prompt=JSON.stringify(job.packet);if(prompt.length>96000)throw new Error('Task brief exceeds context budget; split or shorten it');
    const record=async(command:string,result:ToolResult)=>{const log=`.maxlabs/team/checks/${id}-${run.id}-${run.attempts}-${run.checks.length+1}.log`;const target=await store.path(log.slice('.maxlabs/team/'.length),true);await mkdir(dirname(target),{recursive:true});await writeFile(target,result.text.slice(0,200000),{mode:0o600});run.checks.push({command,passed:!result.isError,output:result.text.slice(0,8000),log});};
    const report=await child(prompt,'worker',{...task,_flexible:true,_execution_path:run.workspace,_recordVerification:record,_recordProgress:(steps:number,limit:number)=>{run.steps_used=steps;run.step_limit=limit;},_recordStop:(reason:string)=>{run.stop_reason=reason;},_recordActions:(actions:WorkerAction[])=>{run.recent_actions=actions;},_recordUsage:(usage:TaskRun["usage"])=>{run.usage=usage;}},signal);run.output=report.text.slice(0,48000);const after=await sourceFiles(root);
    run.status=!report.isError&&completedReport(report.text)&&verified(run.checks)?'passed':'failed';
    if(task.role==='qa'&&!sameFiles(before,after)){run.status='failed';run.output='Independent QA changed application source; review these changes before retrying.';}
   }catch(error){run.status=signal?.aborted?'interrupted':'failed';run.output=String((error as Error).message);}
   if(task.role!=='qa')try{run.commit=await checkpoint(root,`Team task ${id}: ${run.status}`,signal);}catch(error){run.status='failed';run.output+='\n'+String((error as Error).message);}
   run.duration_ms=Date.now()-started;return {...job,run};
  }));
  for(const result of results){const {id,task,run,root}=result;
   if(run.status==='passed'){
    if(task.role==='qa'){const reviewed=await sourceFiles(root);if(!sameFiles(reviewed,await sourceFiles(integration))){run.status='failed';run.output='Integration changed during QA; verify the combined result again.';}else execution.verified_files=reviewed;}
    else try{await git(integration,['merge','--no-edit','--no-ff',run.branch!],signal);execution.commit=await git(integration,['rev-parse','HEAD'],signal);run.merged=true;}catch(error){try{await git(integration,['merge','--abort'],signal);}catch{}run.status='failed';run.output='Merge conflict; task branch preserved for resolution.\n'+String((error as Error).message);}
   }
   state.runs[id]=run;run.archive=`.maxlabs/team/runs/${id}-${run.id}.json`;const archive=await store.path(run.archive.slice('.maxlabs/team/'.length),true);await mkdir(dirname(archive),{recursive:true});await writeFile(archive,JSON.stringify(run,null,2),{mode:0o600});await store.persist(state);
  }
  return {text:JSON.stringify({results:results.map(({id,run})=>({id,status:run.status,branch:run.branch,merged:run.merged,output:run.output.slice(0,1600),checks:run.checks.map(c=>({...c,output:c.output.slice(0,1000)}))})),workspace:integration,branch:execution.branch}),isError:results.some(({run})=>run.status!=='passed')};
 });
}
export class FlexiblePermissions extends PermissionGuard {
 readonly rootStore:TeamStore;readonly qa:boolean;readonly base:PermissionGuard;fetches=0;
 constructor(store:TeamStore,qa:boolean,base:PermissionGuard){super('auto',base.prompt);this.rootStore=store;this.qa=qa;this.base=base;}
 async permit(tool:Tool,args:Record<string,unknown>):Promise<string|undefined>{const name=tool.schema.name;
  if(['write','edit'].includes(name)){if(this.qa)return 'Independent QA cannot edit application source';try{await this.rootStore.workerPath(String(args.path||''));return;}catch(error){return String(error);}}
  if(name==='read'){try{await this.rootStore.workerPath(String(args.path||''));return;}catch(error){return String(error);}}
  if(name==='webfetch'){if(++this.fetches>6)return 'Documentation budget reached';return;}
  if(name==='bash'||name==='verify'){const assessed=await workerAutoCommand(this.rootStore.workspace.root,String(args.command||''));if(!assessed||freshApprovalRisk('bash',{command:assessed})||outside.test(assessed))return this.base.permit({...tool,schema:{...tool.schema,name:'bash'}},args);return;}
  return;
 }
}
export async function flexibleChild(store:TeamStore,transport:GatewayTransport,base:PermissionGuard,settings:AgentSettings,workerModel:string,prompt:string,task:Record<string,any>,signal?:AbortSignal,onTool?:AgentHooks['onTool'],onChildUsage?:AgentHooks['onUsage']):Promise<ToolResult>{
 if(typeof task._recordVerification!=='function'||!task._execution_path) return {text:'Missing runner-owned verification recorder',isError:true};
 const usage={input_tokens:0,cached_input_tokens:0,output_tokens:0,reasoning_tokens:0,calls:0};
 const onUsage=(v:any)=>{usage.input_tokens+=v.uncachedInputTokens+v.cacheReadTokens+v.cacheWriteTokens;usage.cached_input_tokens+=v.cacheReadTokens;usage.output_tokens+=v.outputTokens;usage.reasoning_tokens+=v.reasoningTokens;usage.calls++;task._recordUsage?.({...usage});onChildUsage?.(v);};
 const root=await store.path(task._execution_path),executionStore=new (store.constructor as typeof TeamStore)(await Workspace.open(root));
 const registry=createTools(executionStore.workspace,{environment:workerEnvironment()}),tools=new ToolRegistry(),qa=task.role==='qa';
 for(const name of registry.names()){if(qa&&['write','edit'].includes(name))continue;tools.register(registry.get(name)!);}
 const shell=registry.get('bash')!;tools.register({schema:{name:'verify',description:'Run a real outcome test, build, lint or acceptance command. The runner records actual exit status and output. Choose the project tooling; installation, version probes and skipped tests alone are not acceptance.',parameters:{type:'object',properties:{command:{type:'string'},timeout_ms:{type:'integer'}},required:['command'],additionalProperties:false}},readOnly:false,async execute(args,signal){const result=process.platform==='win32'&&String(args.command).includes('|')?{text:'MaxLabs verification requires an unpiped command on Windows so a failed test cannot be hidden by an output filter.',isError:true}:await shell.execute(args,signal);await task._recordVerification(String(args.command),result);return result;}});
 const recent:WorkerAction[]=[],log=new SessionLog(),limit=teamWorkerStepLimit(settings.maxSteps);
 const onWorkerTool:AgentHooks['onTool']=(name,args,result)=>{recent.push(captureAction(root,name,args,result));if(recent.length>4)recent.shift();task._recordActions?.([...recent]);onTool?.(name,args,result);};
 const guard=new FlexiblePermissions(executionStore,qa,base),agent=new Agent(log,transport,tools,guard,{...settings,model:workerModel||settings.model,maxSteps:limit},{onTool:onWorkerTool,onUsage,onStep:(step)=>{task._recordProgress?.(step,limit);if(limit>=4&&step===limit-2)log.append('steering',{role:'user',source:'runner',content:[{type:'text',text:'Runner budget notice: three model calls remain. Run final verification and return your report. If work remains, describe concrete remaining work so a retry can continue from this branch.'}]});},toolResultLimit:()=>16000});
 const instruction=`${qa?'Independently review source, diff, regressions, weakened tests and implementation evidence. Run your own verification. Do not edit application source.':'Deliver the supplied outcomes in your isolated Git working branch. You choose files, dependencies and commands. Inspect the repository and project instructions first.'} Listed read/write/check paths are suggestions, not an allowlist. Follow the approved stack and decisions; the specification wins over contradictory prose. Use official scaffolding tools and a staging directory when partial files prevent in-place setup. Preserve useful existing work. Use verify for real outcome checks so the runner records evidence. Keep test logs/reports in the system temporary directory or .cache so they do not alter delivery source. If the project uses .env, initialize it with cp -n .env.example .env and never print its contents. You may repair failures within this task. Do not publish, deploy, access other projects or alter workflow metadata. Finish ONLY JSON {"outcome":"completed" or "blocked","summary":"...","blockers":[]}; completed without passing recorded checks is blocked.`;
 try{let result=await agent.run(instruction+` Your budget is ${limit} model steps. Fresh worktrees omit installed dependencies; install from project manifests before testing. Output is bounded: prefer unpiped verification, avoid head, and rerun failed checks after repairs.`+'\n\n'+prompt,new SteeringQueue(),signal);
  if(!completedReport(result.text)&&!/^\s*\{/.test(result.text)){const repair=new Agent(new SessionLog(),transport,new ToolRegistry(),guard,{...settings,model:workerModel||settings.model,maxTokens:2048,maxSteps:1,webSearch:'off',webSearchUses:0},{onUsage});result=await repair.run('Format this report as ONLY JSON with outcome (completed or blocked), summary and blockers. Preserve its outcome and blockers; if ambiguous use blocked. No tools. Report:\n'+result.text.slice(0,16000),new SteeringQueue(),signal);}
  return {text:result.text,isError:!completedReport(result.text)};
 }catch(error){if(error instanceof StepLimitError){task._recordStop?.('step_limit');return {text:budgetHandoff(limit,recent),isError:true};}return {text:String((error as Error).message),isError:true};}
}
