import assert from "node:assert/strict";
import test from "node:test";
import { Presentation, renderStartupBrand, StreamingMarkdown } from "../src/presentation.ts";

test("submitted prompts have explicit high-contrast foreground and background", () => {
  const ui = new Presentation(true, { COLORTERM: "truecolor" });
  const prompt = ui.submittedPrompt("check the tests");
  assert.match(prompt, /\x1b\[48;2;255;102;178m/);
  assert.match(prompt, /\x1b\[38;2;0;0;0m/);
  assert.match(prompt, / ❯ check the tests /);
  assert.match(prompt, /\x1b\[0m$/);
});

test("prompt bands can be disabled without losing user-message distinction", () => {
  const ui = new Presentation(true, { MAXLABS_PROMPT_BAND: "off" });
  const prompt = ui.submittedPrompt("hello");
  assert.doesNotMatch(prompt, /\x1b\[48;/);
  assert.match(prompt, /❯/);
});

test("non-terminal output contains no ANSI escapes", () => {
  const ui = new Presentation(false);
  assert.equal(ui.submittedPrompt("hello"), "> hello");
  assert.equal(ui.modeNotice("auto"), "  auto mode on  audited safe work runs unattended. shift+tab for plan mode.");
});

test("streaming markdown waits for complete lines and preserves fence state", () => {
  const renderer = new StreamingMarkdown(new Presentation(false));
  assert.equal(renderer.feed("# Head"), "");
  assert.equal(renderer.feed("ing\n```ts\nconst x = 1;\n"), "# Heading\n```ts\nconst x = 1;\n");
  assert.equal(renderer.feed("```\nlast"), "```\n");
  assert.equal(renderer.flush(), "last");
});

test("startup branding is stable and animation-free in CI", async () => {
  let output = "";
  await renderStartupBrand(
    new Presentation(false),
    { model: "worker", thinking: "medium", auth: "oauth-first" },
    { columns: 80, write(text) { output += text; } },
    { CI: "1" },
  );
  assert.match(output, /maxlabs\/cli/);
  assert.match(output, /worker  ·  think:medium  ·  oauth-first/);
  assert.doesNotMatch(output, /\x1b/);
});

test("NO_COLOR disables ANSI styling", () => {
  const ui = new Presentation(true, { NO_COLOR: "1", COLORTERM: "truecolor" });
  assert.equal(ui.brand("MaxLabs"), "MaxLabs");
});

test("MAXLABS_COLOR explicitly overrides NO_COLOR", () => {
  const ui = new Presentation(true, { NO_COLOR: "1", MAXLABS_COLOR: "1", COLORTERM: "truecolor" });
  assert.match(ui.brand("MaxLabs"), /\x1b\[38;2;255;102;178m/);
});
