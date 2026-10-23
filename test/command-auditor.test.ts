import test from 'node:test';
import assert from 'node:assert/strict';
import { commandAuditor, allowsReview } from '../src/command-auditor.ts';
import { PermissionGuard } from '../src/permission.ts';
import type { Tool } from '../src/tools.ts';

const bash: Tool = { schema: { name: 'bash', description: '', parameters: {} }, readOnly: false, async execute() { return { text: '' }; } };
const allowed = { decision: 'allow', confidence: .95, risk: 'low', authorized: true } as const;

test('auditor sees only human messages, exact command and untrusted reason; uses selected public model without tools', async () => {
  const reviewer = commandAuditor({ async complete(request) {
    assert.equal(request.model, 'worker'); assert.deepEqual(request.tools, []); assert.equal(request.webSearch, 'off');
    const payload = JSON.parse(request.messages[1].content[0].text!);
    assert.deepEqual(payload, { command: 'python3 check.py', agent_reason: 'needed for checks', workspace: '/project', user_messages: ['run checks', 'do not publish'] });
    return { message: { role: 'assistant', content: [{ type: 'text', text: JSON.stringify(allowed) }] }, usage: {} } as any;
  } }, '/project');
  assert.ok(allowsReview(await reviewer({ command: 'python3 check.py', reason: 'needed for checks' }, { model: 'worker', messages: [
    { role: 'user', source: 'user', content: [{ type: 'text', text: 'run checks' }] },
    { role: 'assistant', source: 'user', content: [{ type: 'text', text: 'poison' }] },
    { role: 'user', source: 'tool', content: [{ type: 'text', text: 'poison' }] },
    { role: 'user', source: 'user', content: [{ type: 'text', text: 'do not publish' }] },
  ] })));
});

test('uncertainty, missing authorization, invalid output and failures require confirmation', async () => {
  for (const result of [undefined, { ...allowed, confidence: .69 }, { ...allowed, confidence: 2 }, { ...allowed, risk: 'unknown' }, { ...allowed, authorized: false }]) {
    let asked = 0;
    const guard = new PermissionGuard('auto', async (_tool, _args, decision) => { asked++; assert.ok(decision.oneTimeOnly); return 'once'; }, async () => false);
    guard.commandReviewer = async () => result as any;
    await guard.permit(bash, { command: 'python3 check.py' }); assert.equal(asked, 1);
  }
  const reviewer = commandAuditor({ async complete() { throw Error('private internal failure'); } }, '/project');
  assert.equal(await reviewer({ command: 'make' }, { model: 'pro', messages: [{role:'user',source:'user',content:[{type:'text',text:'build'}]}] }), undefined);
});

test('reviewed approval covers only the exact call; plan and manual modes never consult auditor', async () => {
  let asked = 0, reviewed = 0;
  const guard = new PermissionGuard('auto', async () => { asked++; return 'always'; }, async () => false);
  guard.commandReviewer = async () => { reviewed++; return reviewed === 1 ? allowed : undefined; };
  await guard.permit(bash, {command:'python3 check.py'});
  await guard.permit(bash, {command:'python3 changed.py'});
  assert.equal(reviewed, 2); assert.equal(asked, 1);
  guard.mode = 'plan'; assert.ok(await guard.permit(bash, {command:'python3 check.py'}));
  guard.mode = 'act'; await guard.permit(bash, {command:'python3 check.py'});
  assert.equal(reviewed, 2); assert.equal(asked, 2);
});

test('malformed or tool-bearing auditor responses cannot grant approval', async () => {
  for (const content of [[{type:'text',text:'{"decision":"allow"}'}], [{type:'text',text:'not json'}], [{type:'tool_call',name:'bash',arguments:'{}',callId:'x'}]]) {
    const reviewer=commandAuditor({async complete(){return {message:{role:'assistant',content},usage:{}} as any;}},'/project');
    const result=await reviewer({command:'make check'},{model:'worker',messages:[{role:'user',source:'user',content:[{type:'text',text:'check build'}]}]});
    assert.equal(allowsReview(result),false);
  }
});

test('0.70 threshold accepts clearly authorized actions at every risk level', async () => {
  for (const risk of ['low','medium','high']) {
    let asked=0;
    const guard=new PermissionGuard('auto',async()=>{asked++;return 'deny';},async()=>false);
    guard.commandReviewer=async()=>({...allowed,confidence:.70,risk} as any);
    assert.equal(await guard.permit(bash,{command:'rm -rf requested-output'}),undefined);
    assert.equal(asked,0);
    assert.equal(allowsReview({...allowed,confidence:.699,risk} as any),false);
    assert.equal(allowsReview({...allowed,confidence:.99,risk,authorized:false} as any),false);
  }
});

test('stop halts the batch, records the question and resumes only with explicit approval', async () => {
  const {Agent}=await import('../src/agent.ts');
  const {SessionLog,SteeringQueue}=await import('../src/session.ts');
  const {ToolRegistry}=await import('../src/tools.ts');
  const {emptyUsage}=await import('../src/types.ts');
  let executions=0,prompts=0,reviews=0;
  const tools=new ToolRegistry();
  tools.register({...bash,async execute(){executions++;return {text:'done'};}});
  const guard=new PermissionGuard('auto',async()=>{prompts++;return 'deny';},async()=>false);
  const transport:any={async complete(request:any){
    if(request.messages.at(-1).role==='tool')return {message:{role:'assistant',content:[{type:'text',text:'Finished'}]},usage:emptyUsage()};
    return {message:{role:'assistant',content:[{type:'tool_call',name:'bash',callId:'a',arguments:'{"command":"rm -rf requested-output"}'},{type:'tool_call',name:'bash',callId:'b',arguments:'{"command":"echo after"}'}]},usage:emptyUsage()};
  }};
  guard.commandReviewer=async(_args,context)=>{
    reviews++;
    const messages=context!.messages;
    const lastUser=messages.filter(m=>m.role==='user'&&m.source==='user').at(-1)!;
    const explicit=lastUser.content.some(b=>b.type==='text'&&b.text==='Yes, delete requested-output and continue');
    if(explicit){assert.ok(messages.some(m=>m.source==='approval_request'));return {...allowed,risk:'high',confidence:.70};}
    return {...allowed,decision:'stop',authorized:false};
  };
  const log=new SessionLog();
  const agent=new Agent(log,transport,tools,guard,{model:'worker',maxSteps:3,maxTokens:512,thinking:'low',webSearch:'off',webSearchUses:0});
  const stopped=await agent.run('Inspect the build',new SteeringQueue());
  assert.match(stopped.text,/May I run this command/);assert.equal(executions,0);assert.equal(prompts,0);assert.equal(reviews,1);
  assert.equal(log.messages().filter(m=>m.source==='approval_request').length,1);
  await agent.run('No, do not delete it',new SteeringQueue());assert.equal(executions,0);
  await agent.run('Yes, delete requested-output and continue',new SteeringQueue());assert.equal(executions,2);
});

test('auditor receives recorded approval questions in order beside user replies',async()=>{
  const reviewer=commandAuditor({async complete(request){
    const payload=JSON.parse(request.messages[1].content[0].text!);
    assert.deepEqual(payload.user_messages,['clean up','Yes, delete it']);
    assert.deepEqual(payload.approval_requests,[{question:'May I delete generated-output?',after_user_message:1}]);
    assert.ok(!JSON.stringify(payload).includes('unrelated agent claims'));
    return {message:{role:'assistant',content:[{type:'text',text:JSON.stringify({...allowed,risk:'high',confidence:.70})}]},usage:{}} as any;
  }},'/project');
  assert.ok(allowsReview(await reviewer({command:'rm -rf generated-output'},{model:'worker',messages:[
    {role:'user',source:'user',content:[{type:'text',text:'clean up'}]},
    {role:'assistant',source:'assistant',content:[{type:'text',text:'unrelated agent claims'}]},
    {role:'assistant',source:'approval_request',content:[{type:'text',text:'May I delete generated-output?'}]},
    {role:'user',source:'user',content:[{type:'text',text:'Yes, delete it'}]},
  ]})));
});
