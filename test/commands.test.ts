import assert from "node:assert/strict";
import test from "node:test";
import { defaultModel, resolveThinkingLevel } from "../src/commands.ts";

test("worker is the default model when it is enabled", () => {
  assert.equal(defaultModel("", [{ id: "pro" }, { id: "worker" }]), "worker");
  assert.equal(defaultModel("pro", [{ id: "pro" }, { id: "worker" }]), "pro");
  assert.equal(defaultModel("", [{ id: "pro" }]), "pro");
});

test("thinking accepts direct values and the documented placeholder form", () => {
  assert.equal(resolveThinkingLevel(["minimal"]), "minimal");
  assert.equal(resolveThinkingLevel(["LEVEL", "low"]), "low");
  assert.equal(resolveThinkingLevel(["LVL", "medium"]), "medium");
  assert.equal(resolveThinkingLevel(["high"]), "high");
  assert.equal(resolveThinkingLevel(["default"]), "default");
  assert.equal(resolveThinkingLevel(["unknown"]), undefined);
});
