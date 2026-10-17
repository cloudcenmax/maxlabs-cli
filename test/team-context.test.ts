import test from 'node:test';
import assert from 'node:assert/strict';
import {cachedContextTool,compactChecks} from '../src/team-context.ts';
test('cached context is read-only, paginated, allowlisted, and does not lose full evidence',async()=>{
 const sources={'check/task/0':'🧪'.repeat(10000)},tool=cachedContextTool(sources);assert.equal(tool.readOnly,true);
 const result=await tool.execute({key:'check/task/0',offset:4000,limit:6000}),page=JSON.parse(result.text);assert.equal(Array.from(page.text).length,1500);assert.equal(page.next_offset,5500);assert.equal(sources['check/task/0'],'🧪'.repeat(10000));
 for(const args of [{key:'../../.env'},{key:'check/task/0',limit:6001},{key:'check/task/0',offset:-1}])assert.equal((await tool.execute(args)).isError,true);
 const checks=Array.from({length:100},(_,i)=>({command:'test',passed:i%2===0,output:'log'.repeat(10000)})),compact=compactChecks(checks);assert.equal(compact.length,3);assert.ok(compact[2].output.length<750);assert.equal(checks[0].output.length,30000);
});
