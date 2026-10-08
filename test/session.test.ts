import assert from "node:assert/strict";
import test from "node:test";
import { SessionLog, SteeringQueue } from "../src/session.ts";

test("session log is append-only to callers", () => {
  const log = new SessionLog();
  log.ensureSystem("stable");
  log.append("user", { role: "user", content: [{ type: "text", text: "hello" }] });
  const events = log.events();
  events[0].message.content[0] = { type: "text", text: "changed" };
  assert.equal(log.messages()[0].content[0].type, "text");
  assert.equal((log.messages()[0].content[0] as { text: string }).text, "stable");
  assert.deepEqual(log.events().map((event) => event.seq), [1, 2]);
});

test("steering drains once at a safe boundary", () => {
  const queue = new SteeringQueue();
  queue.push("focus on tests");
  queue.push("  ");
  assert.deepEqual(queue.drain(), ["focus on tests"]);
  assert.deepEqual(queue.drain(), []);
});
