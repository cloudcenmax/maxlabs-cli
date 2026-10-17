import assert from "node:assert/strict";
import test from "node:test";
import { GatewayTransport, parseUsage } from "../src/transport.ts";

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
