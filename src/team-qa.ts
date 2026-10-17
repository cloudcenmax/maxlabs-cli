import {safeTaskPath,type TeamStore} from './greenfield.ts';
import {sourceDigest,recoverySourceFiles} from './team-recovery.ts';
export function qaTestPath(path:string):boolean {
 if(!safeTaskPath(path))return false;const lower=path.replaceAll('\\','/').toLowerCase();if(!/\.(?:php|js|mjs|cjs|ts|tsx|jsx|py|go|rb|rs|java|kt|cs)$/.test(lower))return false;
 return lower.split('/').slice(0,-1).some(p=>['test','tests','__tests__'].includes(p))||/\.(?:test|spec)\.|(?:^|\/)test_|_test\.(?:go|py)$/.test(lower);
}
export function qaSourceChanges(before:Record<string,string>,after:Record<string,string>):{valid:boolean;added:string[]}{
 if(Object.entries(before).some(([p,h])=>after[p]!==h))return {valid:false,added:[]};const added=Object.keys(after).filter(p=>!(p in before));return {valid:added.every(qaTestPath),added:added.sort()};
}
export async function prepareQARepair(store:TeamStore,id:string,qaID:string):Promise<void>{
 await store.locked(async state=>{
  const status=await store.status(state);if(state.execution_mode!=='flexible'||status.errors.length||state.approvals.spec!==status.gates.spec||!state.execution)throw new Error('Repair requires an unchanged approved flexible plan');
  const artifact=state.artifacts.find(a=>a.kind==='task'&&a.id===id),qaArtifact=state.artifacts.find(a=>a.kind==='task'&&a.id===qaID),findings=state.runs[qaID],prior=state.runs[id];
  if(id===qaID||!artifact||!qaArtifact||(await store.content(artifact)).role==='qa'||(await store.content(qaArtifact)).role!=='qa'||!prior||prior.status==='running'||!findings||findings.status!=='failed'||findings.stop_reason!=='qa_findings'||!findings.commit)throw new Error('Repair requires recorded QA findings and an existing approved implementation task');
  if((findings.qa_repair_rounds||0)>=3||(prior.status!=='passed'&&prior.attempts>=3))throw new Error('QA repair budget reached; review findings before another attempt');
  if(!findings.checks.some(c=>!c.passed))throw new Error('QA repair requires an actual failed recorded check');
  const root=await store.path(findings.workspace!);if(await sourceDigest(root)!==findings.reviewed_source_hash)throw new Error('QA source changed since findings were recorded');
  if(!qaSourceChanges(await recoverySourceFiles(await store.path(state.execution.path)),await recoverySourceFiles(root)).valid)throw new Error('QA branch contains application changes; repair cannot reuse it');
  state.runs[id]={...prior,attempts:prior.status==='passed'?0:prior.attempts,status:'failed',stop_reason:'qa_repair',repair_from:qaID,commit:findings.commit,workspace:findings.workspace,branch:findings.branch,merged:false,checks:[...findings.checks],output:'Repair only defects exposed by QA within existing approved outcomes. Preserve and pass QA regression tests; do not weaken tests or change requirements. QA report: '+findings.output.slice(0,4000)};
  findings.qa_repair_rounds=(findings.qa_repair_rounds||0)+1;delete state.approvals.release;delete state.execution.verified_files;await store.persist(state);
 });
}
