import assert from "node:assert/strict";
import test from "node:test";
import { parseUsage } from "../src/transport.ts";

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
