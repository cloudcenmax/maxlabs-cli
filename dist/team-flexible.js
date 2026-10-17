import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, relative, isAbsolute }  from 'node:path';
import { randomUUID } from 'node:crypto';
import { Agent, StepLimitError,                 } from './agent.js';
import { sourceDigest, recoverySourceFiles, captureAction, budgetHandoff } from './team-recovery.js';

import { applicationChangeSummary, compactChecks, contextIndex, cachedContextTool } from './team-context.js';
import { qaTestPath, qaSourceChanges } from './team-qa.js';
import { projectAutoCommand } from './team-permissions.js';
import { PermissionGuard } from './permission.js';
import { SessionLog, SteeringQueue } from './session.js';
import { Workspace, createTools, ToolRegistry,           } from './tools.js';


import { completedReport, teamWorkerStepLimit,                                              } from './greenfield.js';
import { checkpoint, git, prepareExecution, sourceFiles, sameFiles, workerEnvironment } from './team-workspace.js';
function verified(checks                  )         {let start=0;checks.forEach((c,i)=>{if(c.final&&c.passed)start=i;});const latest=new Map(checks.slice(start).map(c=>[c.command,c.passed]));return latest.size>0&&[...latest.values()].every(Boolean);}
export async function runFlexibleWave(store          ,child          ,ids         ,signal             )                    {
 return store.locked(async state=>{
  const status=await store.status(state);if(!ids.length||ids.length>3||ids.some(id=>!status.ready.includes(id)))throw new Error('Parallel wave contains a task that is not ready');
  if(new Set(ids).size!==ids.length)throw new Error('Duplicate task in parallel wave');
  if(ids.length>1)for(const id of ids){const a=state.artifacts.find(a=>a.kind==='task'&&a.id===id) ;if((await store.content(a)).serial)throw new Error('Serial tasks must run alone');}
  const integration=await prepareExecution(store,state,signal),execution=state.execution ,briefs=[];const tasks=new Map                           (),contextSources                      ={};
  for(const a of state.artifacts){contextSources['artifact/'+a.kind+'/'+a.id]=JSON.stringify(await store.content(a));if(a.kind==='task')tasks.set(a.id,await store.content(a));if(['brief','spec','design','integration'].includes(a.kind)){const content=await store.content(a);delete content.prototype_html;briefs.push({kind:a.kind,content});}}
  for(const [id,run] of Object.entries(state.runs)){contextSources['run/'+id]=JSON.stringify(run);run.checks.forEach((c,i)=>{contextSources[`check/${id}/${i}`]=JSON.stringify(c);});}
  const evidence=Object.fromEntries(Object.entries(state.runs).map(([id,run])=>[id,{status:run.status,summary:run.output.slice(0,240),checks:(run.status==='passed'?compactChecks(run.checks).slice(-1):compactChecks(run.checks)).map(c=>({...c,command:c.command.slice(0,256)})),check_count:run.checks.length,context_key:'run/'+id,check_context_prefix:'check/'+id+'/',branch:run.branch}]));
  const jobs=[];
  for(const id of ids){const task=tasks.get(id) ,previous=state.runs[id],run        ={id:randomUUID(),status:'running',attempts:(previous?.attempts||0)+1,output:'',checks:[],qa_repair_rounds:previous?.qa_repair_rounds,repair_from:previous?.repair_from};let path=`${dirname(execution.path).replaceAll('\\','/')}/workers/${id}-${run.id}`,root=await store.path(path,true),branch=`task-${id}-${run.id}`;
   if(previous&&!previous.commit&&previous.workspace){previous.commit=await checkpoint(await store.path(previous.workspace),`Preserve interrupted task ${id}`,signal);await store.persist(state);}
   const base=previous?.status!=='passed'&&previous?.commit?previous.commit:'HEAD',resume=previous?.stop_reason==='step_limit'&&previous.recovery?.plan.action==='continue';
   if(resume){run.recovery_trail=[...(previous.recovery_trail||[]),previous.recovery ];path=previous.workspace ;root=await store.path(path);branch=previous.branch ;if(await sourceDigest(root)!==previous.recovery .source_hash)throw new Error('Worker changed after coordinator review');await checkpoint(root,`Preserve coordinator continuation ${id}`,signal);}else await git(integration,['worktree','add','-b',branch,root,base],signal);
   const packet                   ={task,briefs,workspace:root,branch,implementation_evidence:evidence,context_index:contextIndex(contextSources),previous_attempt:previous?{status:previous.status,summary:previous.output.slice(0,1600),branch:previous.branch,stop_reason:previous.stop_reason,steps_used:previous.steps_used,checks:compactChecks(previous.checks),recent_actions:previous.recent_actions}:undefined,recovery_plan:resume?previous.recovery .plan:undefined,qa_repair:previous?.repair_from?{qa_id:previous.repair_from,instruction:"Repair in-scope QA defects without weakening regression tests or changing requirements."}:undefined};
   if(base!=='HEAD'){try{await git(root,['merge','--no-edit',execution.commit ],signal);}catch{packet.merge_conflicts=await git(root,['diff','--name-only','--diff-filter=U'],signal);}}
   if(task.role==='qa')packet.change_summary=await applicationChangeSummary(root,execution.base_commit,signal);
   Object.assign(run,{branch,workspace:path});state.runs[id]=run;jobs.push({id,task,run,root,packet});
  }
  delete state.approvals.release;delete execution.verified_files;await store.persist(state);
  // Model work runs concurrently in separate worktrees; integration and manifest writes stay serialized.
  const results=await Promise.all(jobs.map(async job=>{const started=Date.now(); const {id,task,root}=job,run={...job.run,checks:[]                     };try{
    const before=await (task.role==='qa'?recoverySourceFiles:sourceFiles)(root),prompt=JSON.stringify(job.packet);if(Buffer.byteLength(prompt)>96000)throw new Error('Task brief exceeds context budget; split or shorten it');
    const record=async(command       ,result           ,final=false)=>{const log=`.maxlabs/team/checks/${id}-${run.id}-${run.attempts}-${run.checks.length+1}.log`;const target=await store.path(log.slice('.maxlabs/team/'.length),true);await mkdir(dirname(target),{recursive:true});await writeFile(target,result.text.slice(0,200000),{mode:0o600});run.checks.push({command,final,passed:!result.isError,output:result.text.slice(0,8000),log});};
    const report=await child(prompt,'worker',{...task,_flexible:true,_execution_path:run.workspace,_contextSources:contextSources,_recordVerification:record,_recordProgress:(steps       ,limit       )=>{run.steps_used=steps;run.step_limit=limit;},_recordStop:(reason       )=>{run.stop_reason=reason;},_recordActions:(actions               )=>{run.recent_actions=actions;},_recordUsage:(usage                 )=>{run.usage=usage;}},signal);run.output=report.text.slice(0,48000);const after=await (task.role==='qa'?recoverySourceFiles:sourceFiles)(root);
    run.status=!report.isError&&completedReport(report.text)&&verified(run.checks)?'passed':'failed';if(!report.isError&&completedReport(report.text)&&run.status==='failed'){run.stop_reason='verification_failed';run.output='Completion was not accepted: verification is missing or earlier failures remain unresolved. After repairs, run comprehensive verify with final:true. All check logs are preserved.\nWorker report: '+run.output;}
    if(task.role==='qa'){const changes=qaSourceChanges(before,after);if(!changes.valid){run.status='failed';run.stop_reason='qa_source_changed';run.output='Independent QA changed application source or pre-existing tests; review the preserved branch before continuing.';}else{run.qa_test_files=changes.added;run.reviewed_source_hash=await sourceDigest(root);if(run.status==='failed'&&run.checks.some(c=>!c.passed))run.stop_reason='qa_findings';}}
   }catch(error){run.status=signal?.aborted?'interrupted':'failed';run.output=String((error         ).message);}
   if(task.role!=='qa'||run.stop_reason!=='qa_source_changed')try{run.commit=await checkpoint(root,`Team task ${id}: ${run.status}`,signal);}catch(error){run.status='failed';run.output+='\n'+String((error         ).message);}
   run.duration_ms=Date.now()-started;return {...job,run};
  }));
  for(const result of results){const {id,task,run,root}=result;
   if(run.status==='passed'){
    if(task.role==='qa'){const reviewed=await recoverySourceFiles(root);if(!qaSourceChanges(await recoverySourceFiles(integration),reviewed).valid){run.status='failed';run.output='Integration changed during QA; verify the combined result again.';}else {if(run.qa_test_files?.length){try{await git(integration,['merge','--no-edit','--no-ff',run.branch ],signal);execution.commit=await git(integration,['rev-parse','HEAD'],signal);}catch(error){run.status='failed';run.output='QA test merge needs review: '+String((error         ).message);}}if(run.status==='passed')execution.verified_files=await sourceFiles(integration);}}
    else try{await git(integration,['merge','--no-edit','--no-ff',run.branch ],signal);execution.commit=await git(integration,['rev-parse','HEAD'],signal);run.merged=true;}catch(error){try{await git(integration,['merge','--abort'],signal);}catch{}run.status='failed';run.output='Merge conflict; task branch preserved for resolution.\n'+String((error         ).message);}
   }
   state.runs[id]=run;run.archive=`.maxlabs/team/runs/${id}-${run.id}.json`;const archive=await store.path(run.archive.slice('.maxlabs/team/'.length),true);await mkdir(dirname(archive),{recursive:true});await writeFile(archive,JSON.stringify(run,null,2),{mode:0o600});await store.persist(state);
  }
  return {text:JSON.stringify({results:results.map(({id,run})=>({id,status:run.status,branch:run.branch,merged:run.merged,output:run.output.slice(0,1600),checks:run.checks.map(c=>({...c,output:c.output.slice(0,1000)}))})),workspace:integration,branch:execution.branch}),isError:results.some(({run})=>run.status!=='passed')};
 });
}
export class FlexiblePermissions extends PermissionGuard {
          rootStore          ;         qa        ;         base                ;fetches=0;
          qaBefore                       ;
 constructor(store          ,qa        ,base                ,qaBefore                       ){super('auto',base.prompt);this.qaBefore=qaBefore;this.rootStore=store;this.qa=qa;this.base=base;}
 async permit(tool     ,args                       )                          {const name=tool.schema.name;
  if(['write','edit'].includes(name)){if(this.qa){const raw=String(args.path||''),path=isAbsolute(raw)?relative(this.rootStore.workspace.root,raw).replaceAll('\\','/'):raw;if(!this.qaBefore||!qaTestPath(path)||Object.hasOwn(this.qaBefore,path))return 'Independent QA may add or refine new regression tests, but cannot change application source or pre-existing tests';}try{await this.rootStore.workerPath(String(args.path||''));return;}catch(error){return String(error);}}
  if(name==='read'){try{await this.rootStore.workerPath(String(args.path||''));return;}catch(error){return String(error);}}
  if(name==='webfetch'){if(++this.fetches>6)return 'Documentation budget reached';return;}
  if(name==='bash'||name==='verify'){if(!await projectAutoCommand(this.rootStore.workspace.root,String(args.command||'')))return this.base.permit({...tool,schema:{...tool.schema,name:'bash'}},args);return;}
  return;
 }
}
export async function flexibleChild(store          ,transport                 ,base                ,settings              ,workerModel       ,prompt       ,task                   ,signal             ,onTool                      ,onChildUsage                       )                    {
 if(typeof task._recordVerification!=='function'||!task._execution_path) return {text:'Missing runner-owned verification recorder',isError:true};
 const usage={input_tokens:0,cached_input_tokens:0,output_tokens:0,reasoning_tokens:0,calls:0};
 const onUsage=(v    )=>{usage.input_tokens+=v.uncachedInputTokens+v.cacheReadTokens+v.cacheWriteTokens;usage.cached_input_tokens+=v.cacheReadTokens;usage.output_tokens+=v.outputTokens;usage.reasoning_tokens+=v.reasoningTokens;usage.calls++;task._recordUsage?.({...usage});onChildUsage?.(v);};
 const root=await store.path(task._execution_path),executionStore=new (store.constructor                    )(await Workspace.open(root));
 const registry=createTools(executionStore.workspace,{environment:workerEnvironment()}),tools=new ToolRegistry(),qa=task.role==='qa';
 for(const name of registry.names()){tools.register(registry.get(name) );}
 tools.register(cachedContextTool(task._contextSources||{}));
 const shell=registry.get('bash') ;tools.register({schema:{name:'verify',description:'Run a real outcome test, build, lint or acceptance command. The runner records actual exit status and output. Set final:true only for comprehensive task verification after all repairs; a passing final run supersedes earlier failed attempts while retaining their logs. Installation, version probes and skipped tests alone are not acceptance.',parameters:{type:'object',properties:{command:{type:'string'},timeout_ms:{type:'integer'},final:{type:'boolean'}},required:['command'],additionalProperties:false}},readOnly:false,async execute(args,signal){const result=process.platform==='win32'&&String(args.command).includes('|')?{text:'MaxLabs verification requires an unpiped command on Windows so a failed test cannot be hidden by an output filter.',isError:true}:await shell.execute(process.platform==='win32'?args:{...args,command:'set -e\n'+String(args.command)},signal);await task._recordVerification(String(args.command),result,args.final===true);return result;}});
 const recent               =[],log=new SessionLog(),limit=teamWorkerStepLimit(settings.maxSteps);
 const onWorkerTool                     =(name,args,result)=>{recent.push(captureAction(root,name,args,result));if(recent.length>4)recent.shift();task._recordActions?.([...recent]);onTool?.(name,args,result);};
 const guard=new FlexiblePermissions(executionStore,qa,base,qa?await recoverySourceFiles(root):undefined),agent=new Agent(log,transport,tools,guard,{...settings,model:workerModel||settings.model,maxSteps:limit},{onTool:onWorkerTool,onUsage,onStep:(step)=>{task._recordProgress?.(step,limit);if(limit>=4&&step===limit-2)log.append('steering',{role:'user',source:'runner',content:[{type:'text',text:'Runner budget notice: three model calls remain. Run final verification and return your report. If work remains, describe concrete remaining work so a retry can continue from this branch.'}]});},toolResultLimit:()=>16000});
 const instruction=`${qa?'Use team_context with context_index keys for detailed prior reports and checks. The change summary is a bounded navigation aid, not verification evidence. Independently review source, diff, regressions, weakened tests and implementation evidence. Run your own verification. You may add new regression tests, but cannot change application source or pre-existing tests. Report defects with failing verify evidence for orchestrator repair.':'Deliver the supplied outcomes in your isolated Git working branch. You choose files, dependencies and commands. Inspect the repository and project instructions first.'} Listed read/write/check paths are suggestions, not an allowlist. Follow the approved stack and decisions; the specification wins over contradictory prose. Use official scaffolding tools and a staging directory when partial files prevent in-place setup. Preserve useful existing work. Use verify for real outcome checks so the runner records evidence. Keep test logs/reports in the system temporary directory or .cache so they do not alter delivery source. If the project uses .env, initialize it with cp -n .env.example .env and never print its contents. You may repair failures within this task. After repairs, use verify with final:true for comprehensive task acceptance; chain required commands with && so any failure is reported. Do not publish, deploy, access other projects or alter workflow metadata. Finish ONLY JSON {"outcome":"completed" or "blocked","summary":"...","blockers":[]}; completed without passing recorded checks is blocked.`;
 try{let result=await agent.run(instruction+` Your budget is ${limit} model steps. Fresh worktrees omit installed dependencies; install from project manifests before testing. Output is bounded: prefer unpiped verification, avoid head, and rerun failed checks after repairs.`+'\n\n'+prompt,new SteeringQueue(),signal);
  if(!completedReport(result.text)&&!/^\s*\{/.test(result.text)){const repair=new Agent(new SessionLog(),transport,new ToolRegistry(),guard,{...settings,model:workerModel||settings.model,maxTokens:2048,maxSteps:1,webSearch:'off',webSearchUses:0},{onUsage});result=await repair.run('Format this report as ONLY JSON with outcome (completed or blocked), summary and blockers. Preserve its outcome and blockers; if ambiguous use blocked. No tools. Report:\n'+result.text.slice(0,16000),new SteeringQueue(),signal);}
  return {text:result.text,isError:!completedReport(result.text)};
 }catch(error){if(error instanceof StepLimitError){task._recordStop?.('step_limit');return {text:budgetHandoff(limit,recent),isError:true};}return {text:String((error         ).message),isError:true};}
}


//# sourceURL=../src/team-flexible.ts