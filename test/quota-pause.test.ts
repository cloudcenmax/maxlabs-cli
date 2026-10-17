import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,stat} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {QuotaCheckpointStore,quotaIsAvailable,type QuotaCheckpoint} from '../src/quota-pause.ts';
import {SessionLog} from '../src/session.ts';

test('resume availability uses settled usage and rejects unknown or exhausted allowance',()=>{
 const now=Date.parse('2026-10-18T00:00:00Z');
 assert.equal(quotaIsAvailable([{percent_used:100,percent_settled:94.43}],now),true);
 assert.equal(quotaIsAvailable([{percent_used:100,resets_at:'2026-10-24T00:00:00Z'}],now),false);
 assert.equal(quotaIsAvailable([{percent_used:100,resets_at:'2026-10-17T00:00:00Z'}],now),true);
 for(const windows of [undefined,[{}],[{percent_used:'bad'}],[{percent_used:100,resets_at:'invalid'}],[{percent_used:100}]])assert.equal(quotaIsAvailable(windows,now),false);
});

test('quota checkpoints survive restart with completed tool results and account isolation',async()=>{
 const directory=await mkdtemp(join(tmpdir(),'quota-pause-')),log=new SessionLog();
 log.append('user',{role:'user',source:'user',content:[{type:'text',text:'Build the task manager'}]});
 log.append('assistant',{role:'assistant',content:[{type:'tool_call',callId:'write-1',name:'write',arguments:'{"path":"app.php"}'}]});
 log.append('tool',{role:'tool',content:[{type:'tool_result',callId:'write-1',text:'File written'}]});
 const saved:QuotaCheckpoint={version:1,workspace:'/project',accountId:'personal',sessionId:'chat-1',events:log.events(),settings:{model:'worker',maxSteps:100,maxTokens:8192,thinking:'medium',webSearch:'off',webSearchUses:0},mode:'auto',teamMode:false,teamExecute:false,workerModel:'worker',window:'weekly'};
 const store=new QuotaCheckpointStore(directory,'/project');await store.save(saved);
 const reopened=new QuotaCheckpointStore(directory,'/project'),value=await reopened.load('/project','personal');assert.ok(value);
 const restored=new SessionLog();restored.restore(value.events);assert.deepEqual(restored.messages(),log.messages());assert.equal(value.sessionId,'chat-1');assert.equal(value.mode,'auto');
 assert.equal((await stat(reopened.path)).mode&0o777,0o600);
 await assert.rejects(()=>reopened.load('/project','another-account'),/another workspace\/account/);
 await assert.rejects(()=>reopened.load('/other-project','personal'),/another workspace\/account/);
 await reopened.clear();assert.equal(await reopened.load('/project','personal'),undefined);
});
