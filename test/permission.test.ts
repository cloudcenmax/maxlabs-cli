import assert from "node:assert/strict";
import test from "node:test";
import { decide, freshApprovalRisk, isReadOnlyCommand } from "../src/permission.ts";
import type { Tool } from "../src/tools.ts";

const bash: Tool = {
  schema: { name: "bash", description: "shell", parameters: {} },
  readOnly: false,
  async execute() { return { text: "" }; },
};

test("auto mode permits audited inspection", () => {
  assert.equal(isReadOnlyCommand("rg --files | sort"), true);
  assert.equal(isReadOnlyCommand("ls src 2>/dev/null | head -20"), true);
  assert.equal(decide("auto", bash, { command: "rg --files | sort" }).effect, "allow");
  assert.equal(decide("auto", bash, { command: "ls src 2>/dev/null | head -20" }).effect, "allow");
});

test("auto mode catches destructive drift after an ordinary command", () => {
  const decision = decide("auto", bash, { command: "php artisan test && cd /tmp && rm -rf output" });
  assert.equal(decision.effect, "ask");
  assert.equal(decision.oneTimeOnly, true);
  assert.match(decision.reason, /deletes files|outside the working directory/);
});

test("remembered bash approval cannot cover opaque execution", () => {
  assert.match((freshApprovalRisk("bash", { command: "node -e 'process.exit()'" }) || ""), /inline code/);
  assert.match((freshApprovalRisk("bash", { command: "echo $(whoami)" }) || ""), /substitution/);
  assert.match((freshApprovalRisk("bash", { command: "printf hi > file" }) || ""), /redirects/);
});

test("plan mode refuses mutation", () => {
  assert.equal(decide("plan", bash, { command: "npm install" }).effect, "deny");
});
