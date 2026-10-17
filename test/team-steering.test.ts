import test from 'node:test';
import assert from 'node:assert/strict';
import {TeamDirectionReview,teamDirectionPrompt} from '../src/team-steering.ts';
import {Agent} from '../src/agent.ts';
import {SessionLog,SteeringQueue} from '../src/session.ts';
import {ToolRegistry} from '../src/tools.ts';
import {PermissionGuard} from '../src/permission.ts';

test('direction handoffs are interpreted by the coordinator rather than classified as execution failures',async()=>{
 for(const direction of ['DO it parallely if possible','Add calendar export to the plan','Stop before starting another worker']){
  const handoff=new TeamDirectionReview([direction]);let sawDirection=false,calls=0;
  const transport:any={async complete(req:any){calls++;sawDirection=req.messages.some((m:any)=>m.content.some((b:any)=>b.text?.includes(direction)&&b.text.includes('normal coordinator request')));return {message:{role:'assistant',content:[{type:'text',text:'Direction reviewed; completed work is preserved.'}]},usage:{uncachedInputTokens:10,cacheReadTokens:0,cacheWriteTokens:0,outputTokens:5,reasoningTokens:0,webSearches:0}};}};
  const agent=new Agent(new SessionLog(),transport,new ToolRegistry(),new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:3,maxTokens:1024,thinking:'minimal',webSearch:'off',webSearchUses:0});
  const result=await agent.run(teamDirectionPrompt(handoff.directions),new SteeringQueue());assert.ok(sawDirection);assert.equal(calls,1);assert.match(result.text,/Direction reviewed/);assert.match(teamDirectionPrompt(handoff.directions),/wait for the normal client approval gate/);
 }
});
