import type { TeamStatus } from "./greenfield.ts";
const labels: Record<string, string> = { discovery: "Shape the idea", design: "Explore the design", awaiting_design_approval: "Design ready for review", specification: "Prepare the build plan", awaiting_spec_approval: "Build plan ready for review", foundation: "Build the foundation", implementation: "Implement the details", delivery: "Prepare the handoff", awaiting_release_approval: "Release ready for review", complete: "First release complete" };
export function teamNext(status: TeamStatus): string {
  if (status.stage === "awaiting_design_approval") return "Next: /team preview → try the design → /team approve design";
  if (status.stage === "awaiting_spec_approval") return "Next: /team show → review the scope and checks → /team approve spec";
  if (status.stage === "awaiting_release_approval") return "Next: /team show → inspect the application and handoff → /team approve release";
  if (Object.values(status.runs).some(r => ["failed", "interrupted"].includes(r.status))) return "Next: /team show → review failure evidence and affected files before /team approve spec to retry";
  if (status.ready.length) return "Next: /team run to continue approved tasks";
  return status.stage === "complete" ? "Handoff: /team show · Design: /team preview" : "Next: describe your idea or tell the team what to refine.";
}
export function teamStatusText(status: TeamStatus): string {
  const tasks = status.artifacts.filter(a => a.kind === "task");
  const readiness = status.artifacts.some(a => a.kind === "spec") ? status.errors : status.errors.filter(error => !/^(Exactly one |A review artifact is required|Plan needs )/.test(error));
  const phase = /design/.test(status.stage) ? 1 : /spec/.test(status.stage) ? 2 : ["foundation", "implementation", "delivery", "awaiting_release_approval", "complete"].includes(status.stage) ? 3 : 0;
  const steps = ["Discover", "Design", "Plan", "Build"].map((s, i) => `${i < phase || status.stage === "complete" ? "✓" : i === phase ? "●" : "○"} ${s}`).join("  →  ");
  return ["Greenfield SDE Team · Beta", labels[status.stage] || status.stage, steps, tasks.length ? `${tasks.filter(a => status.runs[a.id]?.status === "passed").length} of ${tasks.length} tasks verified` : "Any stack · idea to first release", ...readiness.slice(0, 3).map(e => "Needs attention: " + e), readiness.length > 3 ? `+ ${readiness.length - 3} more readiness issues (/team status --json)` : "", status.execution_mode === "flexible" ? "Flexible workers · up to 3 in parallel · isolated branches" : "Scoped legacy workers", status.execution ? `Build branch: ${status.execution.branch} · /team workspace` : "", Object.values(status.runs).some(r => r.usage) ? `Latest attempts: ${Object.values(status.runs).reduce((n,r)=>n+(r.usage?.input_tokens||0),0).toLocaleString()} input · ${Object.values(status.runs).reduce((n,r)=>n+(r.usage?.output_tokens||0),0).toLocaleString()} output tokens` : "", "", teamNext(status)].filter((s, i, all) => s || (i > 0 && all[i - 1])).join("\n");
}
const label = (key: string): string => key.replaceAll("_", " ").replace(/^./, c => c.toUpperCase());
/** Human-readable review. Raw contracts remain available with --json. */
export function teamArtifactText(content: Record<string, unknown>, depth = 0): string {
  return Object.entries(content).filter(([key]) => !["prototype_html", "content_hash"].includes(key)).map(([key, value]) => {
    const prefix = "  ".repeat(depth);
    const format = (v: unknown): string => Array.isArray(v) ? v.length ? "\n" + v.map(item => prefix + "  • " + (item && typeof item === "object" ? "\n" + teamArtifactText(item as Record<string, unknown>, depth + 2) : String(item))).join("\n") : "None" : v && typeof v === "object" ? "\n" + teamArtifactText(v as Record<string, unknown>, depth + 1) : String(v ?? "—");
    return prefix + label(key) + ": " + format(value);
  }).join("\n");
}
