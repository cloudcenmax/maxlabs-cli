import { createHash, randomUUID } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import { Agent } from './agent.js';

import { SessionLog, SteeringQueue } from './session.js';
import { ToolRegistry, Workspace, createTools } from './tools.js';
import { FlexiblePermissions } from './team-flexible.js';




import { sourceFiles, workerEnvironment, git } from './team-workspace.js';




export async function recoverySourceFiles(root       )                                {
 const files=await sourceFiles(root);for(const path of Object.keys(files))if(path==='.phpunit.result.cache'||path.startsWith('.phpunit.cache/')||files.artisan&&!path.endsWith('/.gitignore')&&(/^(?:storage\/(?:logs|framework\/(?:cache|sessions|views))\/|bootstrap\/cache\/)/.test(path)||path==='database/database.sqlite'||path.startsWith('database/database.sqlite-')))delete files[path];
 return files;
}
function fingerprint(files                      )        {
 const keys=Object.keys(files).sort((a,b)=>Buffer.compare(Buffer.from(a),Buffer.from(b))),h=createHash('sha256');
 for(const key of keys)h.update(key+'\0'+files[key]+'\n');return h.digest('hex');
}
export async function sourceDigest(root       )                 { return fingerprint(await recoverySourceFiles(root)); }
export function parseRecovery(raw       )              {
 const p=JSON.parse(raw.trim().replace(/^```json\s*/,'').replace(/\s*```$/,''));
 if(!['continue','blocked'].includes(p.action)||typeof p.summary!=='string'||!p.summary.trim()||p.summary.length>2400||!Array.isArray(p.next_steps)||p.next_steps.length>6||p.next_steps.some((s        )=>typeof s!=='string'||!s.trim()||s.length>1600)||(p.action==='continue'&&!p.next_steps.length))throw new Error('Coordinator did not return a valid recovery diagnosis');
 return {action:p.action,summary:p.summary,next_steps:p.next_steps};
}
function excerpt(text       ,limit=2400)        { return text.length<=limit?text:text.slice(0,limit/2)+'\n[output excerpt]\n'+text.slice(-limit/2); }
export function captureAction(root       ,name       ,args                       ,result           )              {
 return {tool:name,target:String(args.command||args.path||'').replaceAll(root,'.').replace(/^cd \. && /,'').slice(0,1024),output:excerpt(result.text.replaceAll(root,'.')),failed:Boolean(result.isError)};
}
export function budgetHandoff(limit       ,actions               )        {
 const last=[...actions].reverse().find(a=>a.failed)||actions.at(-1);
 return `Worker reached its ${limit}-step budget. Work is checkpointed for coordinator diagnosis.`+(last?`\nLatest ${last.failed?'failure':'action'}: ${last.tool} ${last.target}\n${last.output}`:'');
}
export async function recoverBudgetTask(store          ,child          ,id       ,signal             )                     {
 return store.locked(async state=>{
  const run=state.runs[id];if(!run||run.status!=='failed'||run.stop_reason!=='step_limit')return {text:'No recovery required'};
  const status=await store.status(state);if(!status.ready.includes(id)||run.attempts>=3)throw new Error(`Task ${id} has exhausted its approved attempts or is no longer ready`);
  if(!run.workspace||!state.execution)throw new Error('Preserved worker workspace is unavailable');
  const root=await store.path(run.workspace),beforeFiles=await recoverySourceFiles(root),before=fingerprint(beforeFiles);
  if(run.recovery){if(run.recovery.plan.action!=='continue')throw new Error('Coordinator needs attention: '+run.recovery.plan.summary);if(before!==run.recovery.source_hash)throw new Error('Worker source changed after coordinator review');return {text:JSON.stringify(run.recovery.plan)};}
  const task=await store.content(state.artifacts.find(a=>a.kind==='task'&&a.id===id) ),briefs=[];
  for(const a of state.artifacts)if(['brief','spec'].includes(a.kind))briefs.push({kind:a.kind,content:await store.content(a)});
  const recovery         ={plan:{action:'blocked',summary:'Diagnosis incomplete',next_steps:[]},source_hash:before,checks:[],usage:{input_tokens:0,cached_input_tokens:0,output_tokens:0,reasoning_tokens:0,calls:0},archive:`.maxlabs/team/recovery/${id}-${run.id}-${randomUUID()}.json`};
  const packet={task,briefs,workspace:root,branch:run.branch,prior_summary:run.output.slice(0,4000),recent_actions:run.recent_actions,checks:run.checks.slice(-3).map(c=>({...c,output:c.output.slice(0,2400)})),change_summary:await git(root,['diff','--stat',state.execution.base_commit,'HEAD'],signal),attempt:run.attempts};
  const prompt=JSON.stringify(packet);if(prompt.length>64000)throw new Error('Recovery context exceeds budget; refine the task before continuing');
  const record=async(command       ,result           )=>{const log=`.maxlabs/team/checks/recovery-${id}-${run.id}-${randomUUID()}.log`,target=await store.path(log.slice('.maxlabs/team/'.length),true);await mkdir(dirname(target),{recursive:true});await writeFile(target,result.text.slice(0,200000),{mode:0o600});recovery.checks.push({command,passed:!result.isError,output:excerpt(result.text.replaceAll(root,'.')),log});};
  const result=await child(prompt,'recovery',{...task,_execution_path:run.workspace,_recordVerification:record,_recordUsage:(usage                  )=>{recovery.usage=usage;}},signal);
  signal?.throwIfAborted();const afterFiles=await recoverySourceFiles(root),after=fingerprint(afterFiles);
  recovery.changed_files=[...new Set([...Object.keys(beforeFiles),...Object.keys(afterFiles)])].filter(path=>beforeFiles[path]!==afterFiles[path]).sort();
  try{recovery.plan=parseRecovery(result.text);if(result.isError&&recovery.plan.action==='continue')recovery.plan={action:'blocked',summary:'Diagnostic execution failed; continuation was refused.',next_steps:[]};}catch{recovery.plan={action:'blocked',summary:result.isError?result.text.slice(0,2400):'Coordinator did not return a valid recovery diagnosis',next_steps:[]};}
  if(after!==before)recovery.plan={action:'blocked',summary:'Application source changed during diagnosis: '+recovery.changed_files.join(', ').slice(0,1600)+'. Review the preserved branch before continuing.',next_steps:[]};
  recovery.source_hash=after;
  const target=await store.path(recovery.archive.slice('.maxlabs/team/'.length),true);await mkdir(dirname(target),{recursive:true});await writeFile(target,JSON.stringify(recovery,null,2),{mode:0o600});run.recovery=recovery;await store.persist(state);
  return {text:JSON.stringify(recovery.plan),isError:recovery.plan.action!=='continue'};
 });
}
export async function recoveryChild(store          ,transport                 ,base                ,settings              ,prompt       ,task                   ,instructions       ,signal             ,onTool                      ,onChildUsage                       )                    {
 onTool?.('team',{action:'recovery'},{text:'Coordinator is inspecting the preserved branch before continuing.'});
 const root=await store.path(task._execution_path),workspace=await Workspace.open(root),executionStore=new (store.constructor                    )(workspace),registry=createTools(workspace,{environment:workerEnvironment()}),tools=new ToolRegistry();
 for(const name of ['read','glob','grep'])tools.register(registry.get(name) );
 const shell=registry.get('bash') ;tools.register({schema:{name:'verify',description:'Run a targeted diagnostic check without changing application source; output and exit status are recorded.',parameters:{type:'object',properties:{command:{type:'string'},timeout_ms:{type:'integer'}},required:['command'],additionalProperties:false}},readOnly:false,async execute(args,signal){const result=process.platform==='win32'&&String(args.command).includes('|')?{text:'Use an unpiped diagnostic command on Windows',isError:true}:await shell.execute(args,signal);await task._recordVerification(String(args.command),result);return result;}});
 const usage={input_tokens:0,cached_input_tokens:0,output_tokens:0,reasoning_tokens:0,calls:0},diagnosticLog=new SessionLog();let inspected=false;
 const agent=new Agent(diagnosticLog,transport,tools,new FlexiblePermissions(executionStore,true,base),{...settings,maxSteps:4,webSearch:'off',webSearchUses:0},{onStep(step){if(step===3)diagnosticLog.append('steering',{role:'user',source:'runner',content:[{type:'text',text:'Two diagnostic calls remain, including this call. Return the recovery JSON now if the evidence supports concrete worker next steps; leave further implementation debugging to the worker. Reserve the last call for the final diagnosis.'}]});},onUsage(v){usage.input_tokens+=v.uncachedInputTokens+v.cacheReadTokens+v.cacheWriteTokens;usage.cached_input_tokens+=v.cacheReadTokens;usage.output_tokens+=v.outputTokens;usage.reasoning_tokens+=v.reasoningTokens;usage.calls++;task._recordUsage({...usage});onChildUsage?.(v);},onTool(name,args,result){if(!result.isError||name==='verify'&&!result.text.startsWith('Permission denied:'))inspected=true;onTool?.(name,args,result);},toolResultLimit:()=>6000});
 try{const result=await agent.run(instructions+'\n\n'+prompt,new SteeringQueue(),signal),plan=parseRecovery(result.text);if(plan.action==='continue'&&!inspected)return {text:'Coordinator did not inspect source or run a diagnostic check; refusing a blind retry.',isError:true};return {text:JSON.stringify(plan),isError:plan.action!=='continue'};}catch(error){return {text:'Coordinator diagnosis could not finish: '+String((error         ).message),isError:true};}
}


//# sourceURL=../src/team-recovery.ts