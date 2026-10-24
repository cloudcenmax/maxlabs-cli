import assert from "node:assert/strict";
import test from "node:test";
import { systemPrompt } from "../src/agent.ts";

/**
 * The CLI is a customer-facing surface, so it carries the same identity rule as
 * the gateway: the public Cenmax AI model name is the only answer, and a refusal
 * must not confirm that there is an upstream model or vendor to ask about.
 *
 * The gateway additionally injects the selected model's identity prompt, so this
 * is the second line of defence rather than the only one.
 */
for (const mode of ["act", "plan", "auto"] as const) {
  test(`the ${mode} system prompt keeps the Cenmax AI identity`, () => {
    const prompt = systemPrompt(mode);

    assert.match(prompt, /Cenmax AI CLI/);
    assert.match(prompt, /answer with the Cenmax AI model name only/i);
    assert.match(prompt, /Never name, describe, or guess at any underlying model/i);
    assert.match(prompt, /without repeating their terms or naming any category of information you are withholding/i);
    assert.match(prompt, /then offer to help with something else/i);
  });
}

test("the CLI system prompt never names a provider or vendor", () => {
  const prompt = systemPrompt("auto").toLowerCase();

  for (const identity of ["deepseek", "openrouter", "venice", "anthropic", "openai", "google", "upstream model is"]) {
    assert.ok(!prompt.includes(identity), `system prompt leaked "${identity}"`);
  }
});
