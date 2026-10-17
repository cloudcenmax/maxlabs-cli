import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {mkdtemp,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';

test('CLI exits on quota and resumes after restart without replaying completed work',async t=>{
 const root=await mkdtemp(join(tmpdir(),'quota-cli-e2e-'));let available=false,calls=0,restored=false;
 const server=createServer(async(req,res)=>{
  res.setHeader('content-type','application/json');
  if(req.url==='/v1/models'){res.end(JSON.stringify({data:[{id:'worker',description:'Worker by MaxLabs'}]}));return;}
  if(req.url==='/v1/usage/me'){res.end(JSON.stringify({windows:[{type:'weekly',percent_used:100,percent_settled:available?20:100,resets_at:'2099-01-01T00:00:00Z'}]}));return;}
  if(req.url!=='/v1/chat/completions'){res.writeHead(404);res.end('{}');return;}
  let raw='';for await(const chunk of req)raw+=chunk;const body=JSON.parse(raw);calls++;
  res.setHeader('content-type','text/event-stream');
  if(available){restored=body.messages.some((m:any)=>m.role==='tool'&&m.tool_call_id==='saved-write');res.end('data: '+JSON.stringify({choices:[{delta:{content:'Resumed from saved progress.'},finish_reason:'stop'}]})+'\n\ndata: [DONE]\n\n');return;}
  if(calls===1){res.end('data: '+JSON.stringify({choices:[{delta:{tool_calls:[{index:0,id:'saved-write',type:'function',function:{name:'bash',arguments:JSON.stringify({command:"printf 'completed work' > progress.txt"})}}]},finish_reason:'tool_calls'}]})+'\n\ndata: [DONE]\n\n');return;}
  res.end('data: {"error":{"code":"quota_exhausted","type":"rate_limit_error","window":"weekly","resets_at":"2099-01-01T00:00:00Z"}}\n\ndata: [DONE]\n\n');
 });
 await new Promise<void>(resolve=>server.listen(0,'127.0.0.1',resolve));t.after(()=>server.close());
 const port=(server.address() as {port:number}).port,entry=fileURLToPath(new URL('../src/cli.ts',import.meta.url)),repo=dirname(dirname(entry));
 const args=['--workspace',root,'--base-url',`http://127.0.0.1:${port}/v1`,'--api-key','fixture','--no-oauth-login','--oauth-store',join(root,'private','oauth.json'),'--auto','--model','worker','--max-steps','4','--max-tokens','32'];
 async function run(extra:string[],input=''):Promise<string>{return new Promise((resolve,reject)=>{
  const child=spawn(process.execPath,['--disable-warning=ExperimentalWarning',entry,...args,...extra],{cwd:repo,env:{...process.env,NO_COLOR:'1',CI:'1'},stdio:['pipe','pipe','pipe']});let output='';
  const timer=setTimeout(()=>{child.kill('SIGTERM');reject(new Error('CLI did not exit: '+output));},20000);
  child.stdout.on('data',chunk=>output+=chunk);child.stderr.on('data',chunk=>output+=chunk);child.on('error',error=>{clearTimeout(timer);reject(error);});child.on('exit',code=>{clearTimeout(timer);code===0?resolve(output):reject(new Error('CLI exited '+code+': '+output));});child.stdin.end(input);
 });}
 const first=await run(['--prompt','Write progress and continue']);assert.match(first,/Paused · usage limit/);assert.equal(await readFile(join(root,'progress.txt'),'utf8'),'completed work');assert.equal(calls,2);
 const blocked=await run([], '/resume\n/exit\n');assert.match(blocked,/Quota is still exhausted/);assert.equal(calls,2);
 available=true;const resumed=await run([], '/resume\n/exit\n');assert.match(resumed,/Resumed from saved progress/);assert.equal(calls,3);assert.equal(restored,true);assert.equal(await readFile(join(root,'progress.txt'),'utf8'),'completed work');
});
