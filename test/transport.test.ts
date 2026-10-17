import assert from "node:assert/strict";
import test from "node:test";
import { GatewayTransport, parseUsage } from "../src/transport.ts";
import { ServiceError } from '../src/transport.ts';
import { Agent } from '../src/agent.ts';
import { SessionLog, SteeringQueue } from '../src/session.ts';
import { ToolRegistry } from '../src/tools.ts';
import { PermissionGuard } from '../src/permission.ts';

test('agent does not repeat a terminal SSE service failure',async t=>{
 let requests=0;t.mock.method(globalThis,'fetch',async()=>{requests++;return new Response('data: {"error":{"code":"quota_exhausted"}}\n\n',{headers:{'content-type':'text/event-stream'}});});
 const transport=new GatewayTransport({baseUrl:'https://fixture.invalid/v1',sessionId:'test'});
 const agent=new Agent(new SessionLog(),transport,new ToolRegistry(),new PermissionGuard('auto',async()=> 'deny'),{model:'worker',maxSteps:3,maxTokens:32,thinking:'default',webSearch:'off',webSearchUses:0});
 await assert.rejects(()=>agent.run('continue',new SteeringQueue()),/request limit/);assert.equal(requests,1);
});

test('an SSE error after tool fragments never exposes executable calls',async t=>{
 const body='data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write","arguments":"{}"}}]}}]}\n\ndata: {"error":{"code":"service_unavailable"}}\n\n';
 t.mock.method(globalThis,'fetch',async()=>new Response(body,{headers:{'content-type':'text/event-stream'}}));
 const transport=new GatewayTransport({baseUrl:'https://fixture.invalid/v1',sessionId:'test'});
 await assert.rejects(()=>transport.complete({model:'worker',messages:[],tools:[],maxTokens:32,thinking:'default',webSearch:'off',webSearchUses:0}),ServiceError);
});

for(const code of ['quota_exhausted','authentication_error','context_length_exceeded','invalid_request_error','service_unavailable'])test('SSE service error '+code+' is classified without leaking payload',async t=>{
 t.mock.method(globalThis,'fetch',async()=>new Response('data: '+JSON.stringify({error:{code,message:'private routing identity and trace'}})+'\n\ndata: [DONE]\n\n',{headers:{'content-type':'text/event-stream'}}));
 const transport=new GatewayTransport({baseUrl:'https://fixture.invalid/v1',sessionId:'test'});
 await assert.rejects(()=>transport.complete({model:'worker',messages:[],tools:[],maxTokens:32,thinking:'default',webSearch:'off',webSearchUses:0}),(error:Error)=>{assert.ok(error instanceof ServiceError);assert.equal(error.retryable,code==='service_unavailable');assert.doesNotMatch(error.message,/private|neither text/);return true;});
});

test('buffered SSE completion and trailing data without newline retain tools and usage',async t=>{
 const payload={choices:[{message:{content:'MaxLabs response',tool_calls:[{id:'c1',function:{name:'read',arguments:'{"path":"README.md"}'}}]},finish_reason:'tool_calls'}],usage:{completion_tokens:3}};
 t.mock.method(globalThis,'fetch',async()=>new Response('data: '+JSON.stringify(payload)+'\n\ndata: {"choices":[],"usage":{"completion_tokens":4}}',{headers:{'content-type':'text/event-stream'}}));
 const transport=new GatewayTransport({baseUrl:'https://fixture.invalid/v1',sessionId:'test'}),result=await transport.complete({model:'worker',messages:[],tools:[],maxTokens:32,thinking:'default',webSearch:'off',webSearchUses:0});
 assert.equal(result.message.content.length,2);assert.equal(result.usage.outputTokens,4);
});

test("usage buckets remain disjoint", () => {
  const usage = parseUsage({
    prompt_tokens: 1_000,
    completion_tokens: 80,
    prompt_tokens_details: { cached_tokens: 700, cache_write_tokens: 100 },
    completion_tokens_details: { reasoning_tokens: 20 },
    server_tool_use: { web_search_requests: 2 },
  });
  assert.deepEqual(usage, {
    uncachedInputTokens: 200,
    cacheReadTokens: 700,
    cacheWriteTokens: 100,
    outputTokens: 80,
    reasoningTokens: 20,
    webSearches: 2,
  });
});

for (const endpoint of ["models", "complete"] as const) {
  for (const fixture of [
    { name: "html", status: 500, type: "text/html", body: "<!DOCTYPE html><html><style>secret debug CSS</style></html>", want: /HTML error page/ },
    { name: "mislabeled html", status: 502, type: "application/json", body: "<html>secret debug CSS</html>", want: /HTML error page/ },
    { name: "escaped html", status: 500, type: "application/json", body: '{"error":{"message":"\\u003chtml>secret debug CSS\\u003c/html>"}}', want: /HTML error page/ },
    { name: "quota", status: 429, type: "application/json", body: JSON.stringify({ error: { message: "Quota exhausted", code: "quota_exhausted", window: "weekly", resets_at: "2026-10-20T00:00:00Z" }, trace: "secret debug CSS" }), want: /Quota exhausted; code=quota_exhausted; window=weekly; resets_at=2026-10-20T00:00:00Z/ },
    { name: "plain", status: 503, type: "text/plain", body: "Service temporarily unavailable", want: /Service temporarily unavailable/ },
    { name: "empty", status: 500, type: "text/plain", body: "", want: /Internal Server Error/ },
    { name: "debug json", status: 500, type: "application/json", body: '{"exception":"secret debug CSS","trace":["private path"]}', want: /Internal Server Error/ },
    { name: "long", status: 500, type: "text/plain", body: "x".repeat(8000), want: /\.\.\./ },
  ]) {
    test(`${endpoint} summarizes ${fixture.name} errors`, async (t) => {
      t.mock.method(globalThis, "fetch", async () => new Response(fixture.body, { status: fixture.status, headers: { "content-type": fixture.type } }));
      const transport = new GatewayTransport({ baseUrl: "https://fixture.invalid/v1", sessionId: "test" });
      await assert.rejects(() => endpoint === "models" ? transport.models() : transport.complete({ model: "worker", messages: [], tools: [], maxTokens: 32, thinking: "default", webSearch: "off", webSearchUses: 0 }), (error: Error) => {
        assert.match(error.message, fixture.want);
        assert.match(error.message, new RegExp(`Gateway returned ${fixture.status}:`));
        assert.doesNotMatch(error.message, /secret debug CSS|<html/);
        assert.ok([...error.message].length < 550);
        return true;
      });
    });
  }
}
